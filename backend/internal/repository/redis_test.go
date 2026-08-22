package repository

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func tunedRedisConfig() *config.RedisConfig {
	return &config.RedisConfig{
		Host:                "localhost",
		Port:                6379,
		Username:            "app-user",
		Password:            "secret",
		DB:                  2,
		DialTimeoutSeconds:  5,
		ReadTimeoutSeconds:  3,
		WriteTimeoutSeconds: 4,
		PoolSize:            100,
		MinIdleConns:        10,
	}
}

func TestBuildRedisOptionsSingleNode(t *testing.T) {
	opts := buildRedisOptions(tunedRedisConfig())
	require.Equal(t, "localhost:6379", opts.Addr)
	require.Equal(t, "app-user", opts.Username)
	require.Equal(t, "secret", opts.Password)
	require.Equal(t, 2, opts.DB)
	require.Equal(t, 5*time.Second, opts.DialTimeout)
	require.Equal(t, 3*time.Second, opts.ReadTimeout)
	require.Equal(t, 4*time.Second, opts.WriteTimeout)
	require.Equal(t, 100, opts.PoolSize)
	require.Equal(t, 10, opts.MinIdleConns)
	require.NotNil(t, opts.Dialer)
	require.Nil(t, opts.TLSConfig)

	// TLS 由 Dialer 自己握手，不能再设置 TLSConfig（否则 go-redis 会在 TLS 上再握一次 TLS）
	cfgTLS := tunedRedisConfig()
	cfgTLS.EnableTLS = true
	optsTLS := buildRedisOptions(cfgTLS)
	require.Nil(t, optsTLS.TLSConfig)
	require.NotNil(t, optsTLS.Dialer)
}

func TestBuildRedisFailoverOptions(t *testing.T) {
	cfg := tunedRedisConfig()
	cfg.SentinelAddrs = []string{"s1:26379", "s2:26379"}
	cfg.MasterName = "mymaster"
	cfg.SentinelUsername = "sentinel-user"
	cfg.SentinelPassword = "sentinel-pw"
	cfg.EnableTLS = true

	opts := buildRedisFailoverOptions(cfg)
	require.Equal(t, "mymaster", opts.MasterName)
	require.Equal(t, []string{"s1:26379", "s2:26379"}, opts.SentinelAddrs)
	require.Equal(t, "sentinel-user", opts.SentinelUsername)
	require.Equal(t, "sentinel-pw", opts.SentinelPassword)
	require.Equal(t, "app-user", opts.Username)
	require.Equal(t, "secret", opts.Password)
	require.Equal(t, 2, opts.DB)
	require.Equal(t, 100, opts.PoolSize)
	require.NotNil(t, opts.Dialer)
	require.Nil(t, opts.TLSConfig)

	// 哨兵地址是配置的副本，客户端内部打乱顺序时不会改到配置
	opts.SentinelAddrs[0] = "changed"
	require.Equal(t, "s1:26379", cfg.SentinelAddrs[0])
}

// TestRedisOptionsParityBetweenSingleAndSentinel 遍历 go-redis 的两个选项结构体声明：
// 凡是两边都有的同名字段，单机与 Sentinel 构造出来的值必须相等。新的调优项只加到一边会在这里失败，
// 而不是等到切换拓扑的那天才发现连接池参数少了一半。
func TestRedisOptionsParityBetweenSingleAndSentinel(t *testing.T) {
	cfg := tunedRedisConfig()
	cfg.EnableTLS = true
	cfg.SentinelAddrs = []string{"s1:26379"}
	cfg.MasterName = "mymaster"

	single := reflect.ValueOf(*buildRedisOptions(cfg))
	failover := reflect.ValueOf(*buildRedisFailoverOptions(cfg))

	compared := 0
	compare := func(from, to reflect.Value, fromName, toName string) {
		for i := 0; i < from.NumField(); i++ {
			field := from.Type().Field(i)
			if field.PkgPath != "" || from.Field(i).IsZero() {
				continue
			}
			counterpart := to.FieldByName(field.Name)
			if !counterpart.IsValid() {
				continue // 只存在于一边的字段（如 Addr / MasterName）没有对应关系
			}
			compared++
			if field.Type.Kind() == reflect.Func {
				// 闭包在每个调用点各自实例化，代码指针不可比；两边都装上了同一来源的拨号器即为一致
				require.False(t, counterpart.IsNil(), "%s.%s is set but %s.%s is nil", fromName, field.Name, toName, field.Name)
				continue
			}
			require.Equal(t, from.Field(i).Interface(), counterpart.Interface(), "%s.%s differs from %s.%s", fromName, field.Name, toName, field.Name)
		}
	}
	compare(single, failover, "Options", "FailoverOptions")
	compare(failover, single, "FailoverOptions", "Options")

	// 共用参数的每个字段都应参与了比较（两个方向各一次），否则测试在空转
	require.GreaterOrEqual(t, compared, 2*reflect.TypeOf(redisCommonOptions{}).NumField())
}

// TestRedisCommonOptionsFieldsExistOnBothStructs 直接检查共用参数的字段名在两个 go-redis 结构体上都存在且类型一致：
// 升级 go-redis 改了字段名时，这里先于任何连接失败。
func TestRedisCommonOptionsFieldsExistOnBothStructs(t *testing.T) {
	common := reflect.TypeOf(redisCommonOptions{})
	for _, target := range []reflect.Type{reflect.TypeOf(redis.Options{}), reflect.TypeOf(redis.FailoverOptions{})} {
		for i := 0; i < common.NumField(); i++ {
			field := common.Field(i)
			counterpart, ok := target.FieldByName(field.Name)
			require.True(t, ok, "%s has no field %s", target.Name(), field.Name)
			require.Equal(t, counterpart.Type, field.Type, "%s.%s type", target.Name(), field.Name)
		}
	}
}

func TestRedisTLSServerName(t *testing.T) {
	require.Equal(t, "redis.internal", redisTLSServerName("10.0.0.5:6379", "redis.internal"))
	require.Equal(t, "10.0.0.5", redisTLSServerName("10.0.0.5:6379", ""))
	require.Equal(t, "sentinel-1.svc", redisTLSServerName("sentinel-1.svc:26379", ""))
	require.Equal(t, "::1", redisTLSServerName("[::1]:6379", ""))
	require.Equal(t, "bare-host", redisTLSServerName("bare-host", ""))
}

// tlsSNIRecorder 是一个只收 ClientHello 的 TLS 监听器：它记录客户端发来的 SNI。
// 客户端随后会因为证书不可信而握手失败，但 ServerName 的推导已经在 ClientHello 里体现出来了。
func tlsSNIRecorder(t *testing.T) (addr string, sni <-chan string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	observed := make(chan string, 8)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			observed <- hello.ServerName
			return nil, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = conn.(*tls.Conn).Handshake()
				_ = conn.Close()
			}()
		}
	}()
	return listener.Addr().String(), observed
}

func TestRedisDialerDerivesTLSServerNamePerDial(t *testing.T) {
	addr, sni := tlsSNIRecorder(t)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	waitSNI := func(t *testing.T) string {
		select {
		case name := <-sni:
			return name
		case <-time.After(5 * time.Second):
			t.Fatal("server saw no ClientHello")
			return ""
		}
	}

	t.Run("server name follows the dialed host", func(t *testing.T) {
		dial := newRedisDialer(true, "", 2*time.Second)
		_, err := dial(context.Background(), "tcp", "localhost:"+port)
		require.Error(t, err, "self-signed certificate must not be trusted")
		require.Equal(t, "localhost", waitSNI(t))
	})

	t.Run("explicit override wins for every dial", func(t *testing.T) {
		dial := newRedisDialer(true, "redis.internal", 2*time.Second)
		_, err := dial(context.Background(), "tcp", "127.0.0.1:"+port)
		require.Error(t, err)
		require.Equal(t, "redis.internal", waitSNI(t))
	})

	t.Run("plain dial performs no TLS", func(t *testing.T) {
		plain, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = plain.Close() }()
		go func() {
			conn, err := plain.Accept()
			if err == nil {
				_ = conn.Close()
			}
		}()
		dial := newRedisDialer(false, "", 0)
		conn, err := dial(context.Background(), "tcp", plain.Addr().String())
		require.NoError(t, err)
		_, isTLS := conn.(*tls.Conn)
		require.False(t, isTLS)
		_ = conn.Close()
	})
}

func TestNewRedisClientSingleNodeAgainstMiniredis(t *testing.T) {
	server := miniredis.RunT(t)
	host, portText, err := net.SplitHostPort(server.Addr())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	client := NewRedisClient(&config.RedisConfig{Host: host, Port: port, DB: 1, PoolSize: 4})
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())
	require.NoError(t, client.Set(ctx, "k", "v", 0).Err())
	// 写进了配置指定的 DB 1，说明 DB 与自定义 Dialer 都生效了
	require.Equal(t, "v", mustGet(t, server, 1, "k"))
}

func mustGet(t *testing.T, server *miniredis.Miniredis, db int, key string) string {
	t.Helper()
	value, err := server.DB(db).Get(key)
	require.NoError(t, err)
	return value
}
