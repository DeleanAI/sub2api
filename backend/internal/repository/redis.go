package repository

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/redis/go-redis/v9"
)

// InitRedis 初始化应用使用的 Redis 客户端（依赖注入入口）。
func InitRedis(cfg *config.Config) *redis.Client {
	client := NewRedisClient(&cfg.Redis)
	if cfg.Server.EnableServerTiming {
		client.AddHook(serverTimingRedisHook{})
	}
	return client
}

// NewRedisClient 是仓库里唯一的 Redis 客户端构造函数：主服务与安装向导的连通性测试都走这里，
// 所以"单机还是 Sentinel、TLS 怎么校验"只有一种答案。两种模式都返回 *redis.Client
// （NewFailoverClient 的返回类型就是它），调用方不需要感知拓扑。
//
// 单机模式下 redis.host/redis.port 是目标地址；Sentinel 模式下主节点地址由哨兵发现，
// host/port 即使配置了也不使用——启动日志会把这一点写出来，而不是悄悄忽略。
func NewRedisClient(cfg *config.RedisConfig) *redis.Client {
	if cfg.SentinelEnabled() {
		opts := buildRedisFailoverOptions(cfg)
		slog.Info("redis client configured",
			"redis.mode", "sentinel",
			"master", opts.MasterName,
			"sentinels", len(opts.SentinelAddrs),
			"db", opts.DB,
			"tls", cfg.EnableTLS,
			"tls_server_name", cfg.TLSServerName,
			"ignored", "redis.host/redis.port (the master address is discovered from the sentinels)",
		)
		return redis.NewFailoverClient(opts)
	}
	opts := buildRedisOptions(cfg)
	slog.Info("redis client configured",
		"redis.mode", "single",
		"addr", opts.Addr,
		"db", opts.DB,
		"tls", cfg.EnableTLS,
		"tls_server_name", cfg.TLSServerName,
	)
	return redis.NewClient(opts)
}

// redisCommonOptions 是两种客户端共用的调优/鉴权/拨号参数。字段名与 go-redis 的 Options 和
// FailoverOptions 完全一致，applyTo 按名拷贝：新增一个调优项只需在这里加一个字段，两种模式
// 同时生效，不存在"改了单机忘了哨兵"的可能。字段名对不上会在第一次构造客户端时 panic，
// 那是编程错误（go-redis 改名或这里拼错），启动即失败比静默少配一项安全。
//
// 连接池参数说明：
//   - PoolSize: 控制最大并发连接数
//   - MinIdleConns: 保持最小空闲连接，减少冷启动延迟
//   - DialTimeout/ReadTimeout/WriteTimeout: 精确控制各阶段超时
type redisCommonOptions struct {
	Username     string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
	MinIdleConns int
	// Dialer 同时负责 TCP 与 TLS（见 newRedisDialer）；设置了 Dialer 之后 go-redis 不再自行处理
	// TLSConfig，所以这里不能再设置 TLSConfig，否则会在 TLS 上再握一次 TLS。
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newRedisCommonOptions(cfg *config.RedisConfig) redisCommonOptions {
	dialTimeout := time.Duration(cfg.DialTimeoutSeconds) * time.Second
	return redisCommonOptions{
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  dialTimeout,
		ReadTimeout:  time.Duration(cfg.ReadTimeoutSeconds) * time.Second,
		WriteTimeout: time.Duration(cfg.WriteTimeoutSeconds) * time.Second,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		Dialer:       newRedisDialer(cfg.EnableTLS, cfg.TLSServerName, dialTimeout),
	}
}

// applyTo 把共用参数按字段名写进 *redis.Options 或 *redis.FailoverOptions。
func (c redisCommonOptions) applyTo(dst any) {
	target := reflect.ValueOf(dst).Elem()
	source := reflect.ValueOf(c)
	for i := 0; i < source.NumField(); i++ {
		name := source.Type().Field(i).Name
		field := target.FieldByName(name)
		if !field.IsValid() {
			panic(fmt.Sprintf("redis: %T has no field %q; redisCommonOptions must only name fields shared by redis.Options and redis.FailoverOptions", dst, name))
		}
		field.Set(source.Field(i))
	}
}

// buildRedisOptions 构建单机模式的连接选项。
func buildRedisOptions(cfg *config.RedisConfig) *redis.Options {
	opts := &redis.Options{Addr: cfg.Address()}
	newRedisCommonOptions(cfg).applyTo(opts)
	return opts
}

// buildRedisFailoverOptions 构建 Sentinel 模式的连接选项。
func buildRedisFailoverOptions(cfg *config.RedisConfig) *redis.FailoverOptions {
	opts := &redis.FailoverOptions{
		MasterName:       cfg.MasterName,
		SentinelAddrs:    slices.Clone(cfg.SentinelAddrs),
		SentinelUsername: cfg.SentinelUsername,
		SentinelPassword: cfg.SentinelPassword,
	}
	newRedisCommonOptions(cfg).applyTo(opts)
	return opts
}

// redisDefaultDialTimeout 与 go-redis Options.init 补的默认值一致。自定义 Dialer 看不到 go-redis
// 事后补的默认值（闭包在 init 之前就捕获了配置），所以零值要在这里自己补。
const redisDefaultDialTimeout = 5 * time.Second

// newRedisDialer 返回唯一的拨号实现。TLS 的 ServerName 取自本次拨号的目标主机（或 tlsServerName 覆盖），
// 因此哨兵与主节点各按自己的地址校验证书：Sentinel 模式下 go-redis 用同一个 Dialer 连哨兵和被发现的主节点，
// 一个写死的 ServerName 必然与其中一方不匹配。单机模式下目标就是 host:port，行为与以前的
// ServerName=host 完全相同。
func newRedisDialer(enableTLS bool, tlsServerName string, dialTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if dialTimeout <= 0 {
		dialTimeout = redisDefaultDialTimeout
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		netDialer := &net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 5 * time.Minute, // 与 go-redis 默认 Dialer 一致
		}
		conn, err := netDialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if !enableTLS {
			return conn, nil
		}
		tlsConn := tls.Client(conn, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: redisTLSServerName(addr, tlsServerName),
		})
		handshakeCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("redis tls handshake with %s: %w", addr, err)
		}
		return tlsConn, nil
	}
}

// redisTLSServerName 决定证书校验用的名字：显式覆盖优先，否则取拨号地址的主机部分。
func redisTLSServerName(addr, override string) string {
	if override != "" {
		return override
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // 没有端口的地址整体就是主机名
	}
	return host
}
