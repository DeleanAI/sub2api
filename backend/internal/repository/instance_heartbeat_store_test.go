//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const testLivenessWindow = 45 * time.Second

func newInstanceStoreFixture(t *testing.T) (*miniredis.Miniredis, service.InstanceHeartbeatStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, NewInstanceHeartbeatStore(rdb)
}

func TestInstanceHeartbeatStoreRoundTrip(t *testing.T) {
	mr, store := newInstanceStoreFixture(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	mr.SetTime(now)
	inst := service.InstanceInfo{ID: "a", Hostname: "host-a", Version: "1.0.0"}

	require.NoError(t, store.Heartbeat(ctx, inst, testLivenessWindow))

	active, err := store.ListActive(ctx, testLivenessWindow)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "a", active[0].ID)
	require.Equal(t, "host-a", active[0].Hostname)
	require.Equal(t, "1.0.0", active[0].Version)
	require.Equal(t, now.Unix(), active[0].LastSeen.Unix())

	// 整个 key 挂 2×窗口 的 TTL：部署整体下线后不留垃圾。
	require.Equal(t, 2*testLivenessWindow, mr.TTL(instanceHeartbeatKey))

	// 同一实例再次心跳只更新时间戳，不产生重复成员。
	mr.SetTime(now.Add(15 * time.Second))
	require.NoError(t, store.Heartbeat(ctx, inst, testLivenessWindow))
	active, err = store.ListActive(ctx, testLivenessWindow)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, now.Add(15*time.Second).Unix(), active[0].LastSeen.Unix())
}

func TestInstanceHeartbeatStoreExpiresStaleInstances(t *testing.T) {
	mr, store := newInstanceStoreFixture(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	mr.SetTime(t0)
	a := service.InstanceInfo{ID: "a", Hostname: "host-a"}
	b := service.InstanceInfo{ID: "b", Hostname: "host-b"}

	require.NoError(t, store.Heartbeat(ctx, a, testLivenessWindow))

	// 窗口内可见，窗口外不可见（读取侧的时间过滤，基准取自 Redis TIME）。
	mr.SetTime(t0.Add(44 * time.Second))
	active, err := store.ListActive(ctx, testLivenessWindow)
	require.NoError(t, err)
	require.Len(t, active, 1)
	mr.SetTime(t0.Add(46 * time.Second))
	active, err = store.ListActive(ctx, testLivenessWindow)
	require.NoError(t, err)
	require.Empty(t, active)

	// 别人的心跳顺手淘汰过期成员（写入侧的物理清理）。
	mr.SetTime(t0.Add(60 * time.Second))
	require.NoError(t, store.Heartbeat(ctx, b, testLivenessWindow))
	active, err = store.ListActive(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "b", active[0].ID)
}

// TestInstanceHeartbeatStoreIgnoresCallerClocks 钉死"时间基准只有 Redis 一个"。
//
// 之前打分用写入方的时钟、算截止用读取方的时钟：慢 60s 的副本写下偏小的分数，
// 对别人不可见——"只有我一个实例"于是成立，原地更新被放行，而这段代码存在的全部
// 意义就是在有别的副本时拒绝它。快 60s 的副本更糟：一次 ZREMRANGEBYSCORE 就能把
// 所有健康成员删光。接口不再接收调用方的时刻，这条断言用真实响应验证它。
func TestInstanceHeartbeatStoreIgnoresCallerClocks(t *testing.T) {
	mr, store := newInstanceStoreFixture(t)
	ctx := context.Background()
	serverNow := time.Unix(1_700_000_000, 0)
	mr.SetTime(serverNow)

	// 两个副本先后心跳；本地时钟无从传入，分数只可能来自 Redis。
	require.NoError(t, store.Heartbeat(ctx, service.InstanceInfo{ID: "slow"}, testLivenessWindow))
	require.NoError(t, store.Heartbeat(ctx, service.InstanceInfo{ID: "fast"}, testLivenessWindow))

	active, err := store.ListActive(ctx, testLivenessWindow)
	require.NoError(t, err)
	require.Len(t, active, 2, "同一个 Redis 时钟下两个副本必须互相可见")
	for _, inst := range active {
		require.Equal(t, serverNow.Unix(), inst.LastSeen.Unix(), "分数必须是 Redis 的 TIME")
	}
}

func TestInstanceHeartbeatStoreRemove(t *testing.T) {
	mr, store := newInstanceStoreFixture(t)
	ctx := context.Background()
	mr.SetTime(time.Unix(1_700_000_000, 0))
	a := service.InstanceInfo{ID: "a", Hostname: "host-a", Version: "1.0.0"}
	b := service.InstanceInfo{ID: "b", Hostname: "host-b", Version: "1.0.0"}
	require.NoError(t, store.Heartbeat(ctx, a, testLivenessWindow))
	require.NoError(t, store.Heartbeat(ctx, b, testLivenessWindow))

	require.NoError(t, store.Remove(ctx, a))

	active, err := store.ListActive(ctx, testLivenessWindow)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "b", active[0].ID)
}

// 解析不了的成员意味着「有一个我不认识的进程在跑」，必须报错让调用方按未知处理。
func TestInstanceHeartbeatStoreFailsClosedOnUndecodableMember(t *testing.T) {
	mr, store := newInstanceStoreFixture(t)
	now := time.Unix(1_700_000_000, 0)
	mr.SetTime(now)
	require.NoError(t, store.Heartbeat(context.Background(), service.InstanceInfo{ID: "a"}, testLivenessWindow))
	mr.ZAdd(instanceHeartbeatKey, float64(now.Unix()), "not-json")

	_, err := store.ListActive(context.Background(), testLivenessWindow)

	require.Error(t, err)
	require.ErrorContains(t, err, "not decodable")
}

// 端到端：两个注册表进程共享一个 Redis，互相看见、优雅退出后立刻消失、Redis 故障时计数未知。
func TestInstanceRegistryEndToEndWithRedis(t *testing.T) {
	mr, store := newInstanceStoreFixture(t)
	ctx := context.Background()

	first := service.NewInstanceRegistry(store, "1.0.0")
	first.Start()
	t.Cleanup(first.Stop)
	require.Eventually(t, func() bool {
		active, err := store.ListActive(ctx, testLivenessWindow)
		return err == nil && len(active) == 1 && active[0].ID == first.Self().ID
	}, 5*time.Second, 10*time.Millisecond, "first heartbeat must land in Redis")

	second := service.NewInstanceRegistry(store, "1.0.1")
	second.Start()
	require.Eventually(t, func() bool {
		active, err := first.ActiveInstances(ctx)
		return err == nil && len(active) == 2
	}, 5*time.Second, 10*time.Millisecond, "peer must become visible")

	second.Stop()
	active, err := first.ActiveInstances(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1, "graceful stop must deregister immediately, not after the liveness window")
	require.Equal(t, first.Self().ID, active[0].ID)

	mr.SetError("ERR redis is down")
	_, err = first.ActiveInstances(ctx)
	require.Error(t, err, "Redis outage must surface as unknown, never as a count")
	mr.SetError("")
}
