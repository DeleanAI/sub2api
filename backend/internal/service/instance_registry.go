package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	// instanceHeartbeatInterval 是心跳周期。
	instanceHeartbeatInterval = 15 * time.Second
	// instanceLivenessWindow 是存活窗口：连续漏掉 3 次心跳才视为离线。崩溃的实例最多在
	// 这段时间内继续被计数——这是有意的保守：宁可多挡 45 秒的原地更新，也不能少算一个实例。
	instanceLivenessWindow = 45 * time.Second
	// instanceRegistryOpTimeout 是单次注册表操作（心跳 / 注销 / 查询）的超时。
	instanceRegistryOpTimeout = 2 * time.Second
)

// InstanceInfo 描述一个正在运行的 sub2api 进程。
type InstanceInfo struct {
	ID       string
	Hostname string
	Version  string
	LastSeen time.Time
}

// InstanceHeartbeatStore 是实例注册表的持久化端口；Redis 实现在 repository 层。
// InstanceHeartbeatStore 的时间基准由存储侧统一提供，接口刻意不接收调用方的时刻。
//
// 之前打分用写入方的时钟、算截止用读取方的时钟，两边各走各的表：慢 60s 的 B 写下
// 偏小的分数，A 按自己的时钟算截止就看不见 B，于是"只有我一个实例"成立，原地更新
// 被放行——而这段代码存在的全部意义就是在有别的副本时拒绝原地更新。快 60s 的 A 更糟，
// 它的 ZREMRANGEBYSCORE 会把所有健康成员直接删掉。45s 的窗口只能容忍 <45s 的偏移，
// 对一段以"失败要往安全侧倒"为设计目标的代码来说，这是反的。
type InstanceHeartbeatStore interface {
	// Heartbeat 记录 inst 存活，并淘汰 window 之前的记录。时刻取自存储侧。
	Heartbeat(ctx context.Context, inst InstanceInfo, window time.Duration) error
	// ListActive 返回 window 之内有过心跳的实例。截止时刻同样取自存储侧。
	ListActive(ctx context.Context, window time.Duration) ([]InstanceInfo, error)
	// Remove 立即注销 inst，优雅停机时调用，避免滚动重启期间把已退出的进程多算一个窗口。
	Remove(ctx context.Context, inst InstanceInfo) error
}

// LiveInstanceCounter 回答「这个部署现在有几个进程在跑」。
type LiveInstanceCounter interface {
	// ActiveInstances 返回存活窗口内的实例，总是包含调用方自己。
	// 返回 error 表示数量未知（例如 Redis 不可达）：调用方必须按未知处理，不能当作 1。
	ActiveInstances(ctx context.Context) ([]InstanceInfo, error)
}

// InstanceRegistry 让每个进程在 Redis 里维持心跳，并据此回答当前有多少个实例存活。
//
// 它存在的原因是原地自更新只会替换「处理这次请求的那一个进程」的二进制：多副本时其余
// 副本仍在跑旧版本，而容器重启又会把改动抹掉。要拒绝这种更新，先得知道自己不是孤身一人。
type InstanceRegistry struct {
	store    InstanceHeartbeatStore
	self     InstanceInfo
	interval time.Duration
	window   time.Duration
	now      func() time.Time

	stopCh    chan struct{}
	done      chan struct{}
	started   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once

	// heartbeatFailing 只在状态翻转时写日志：故障期间每 15 秒刷一条只会淹没真正的信号。
	heartbeatFailing atomic.Bool
}

// NewInstanceRegistry 为当前进程生成稳定的运行期实例 ID（进程生命周期内不变）。
func NewInstanceRegistry(store InstanceHeartbeatStore, version string) *InstanceRegistry {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	return &InstanceRegistry{
		store: store,
		self: InstanceInfo{
			ID:       uuid.NewString(),
			Hostname: hostname,
			Version:  version,
		},
		interval: instanceHeartbeatInterval,
		window:   instanceLivenessWindow,
		now:      time.Now,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Self 返回当前进程的实例信息。
func (r *InstanceRegistry) Self() InstanceInfo {
	return r.self
}

// Start 立即发送第一次心跳，之后按周期续约。
func (r *InstanceRegistry) Start() {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
		r.started.Store(true)
		slog.Info("instance_registry: started",
			"instance_id", r.self.ID,
			"hostname", r.self.Hostname,
			"version", r.self.Version,
			"heartbeat_interval", r.interval,
			"liveness_window", r.window)
		go r.run()
	})
}

func (r *InstanceRegistry) run() {
	defer close(r.done)

	r.heartbeat()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.heartbeat()
		case <-r.stopCh:
			return
		}
	}
}

func (r *InstanceRegistry) heartbeat() {
	ctx, cancel := context.WithTimeout(context.Background(), instanceRegistryOpTimeout)
	defer cancel()

	err := r.store.Heartbeat(ctx, r.self, r.window)
	switch {
	case err != nil && !r.heartbeatFailing.Swap(true):
		slog.Warn("instance_registry: heartbeat failed; peers stop counting this instance after the liveness window, and in-app updates are refused while the instance count is unknown",
			"instance_id", r.self.ID, "error", err)
	case err == nil && r.heartbeatFailing.Swap(false):
		slog.Info("instance_registry: heartbeat recovered", "instance_id", r.self.ID)
	}
}

// Stop 停止心跳并主动注销。注销失败不是致命的：记录原因后由存活窗口兜底。
func (r *InstanceRegistry) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		if !r.started.Load() {
			return
		}
		close(r.stopCh)
		<-r.done

		ctx, cancel := context.WithTimeout(context.Background(), instanceRegistryOpTimeout)
		defer cancel()
		if err := r.store.Remove(ctx, r.self); err != nil {
			slog.Warn("instance_registry: deregister failed; peers will keep counting this instance until the liveness window expires",
				"instance_id", r.self.ID, "liveness_window", r.window, "error", err)
			return
		}
		slog.Info("instance_registry: deregistered", "instance_id", r.self.ID)
	})
}

// ActiveInstances 实现 LiveInstanceCounter。
func (r *InstanceRegistry) ActiveInstances(ctx context.Context) ([]InstanceInfo, error) {
	now := r.now()
	instances, err := r.store.ListActive(ctx, r.window)
	if err != nil {
		return nil, fmt.Errorf("instance registry: list active instances: %w", err)
	}

	// 本进程显然活着：第一次心跳尚未落地、或注册表被清空（FLUSHALL）时也必须算上自己，
	// 否则「0 个实例」会让调用方误判为单机。
	for _, inst := range instances {
		if inst.ID == r.self.ID {
			return instances, nil
		}
	}
	self := r.self
	self.LastSeen = now
	return append(instances, self), nil
}
