package setup

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
)

// clearConnectionEnv 屏蔽开发机 shell 里可能存在的连接变量，让测试只看到自己设置的值。
func clearConnectionEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"DATABASE_URL", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_DBNAME", "DATABASE_SSLMODE",
		"REDIS_URL", "REDIS_HOST", "REDIS_PORT", "REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_DB", "REDIS_ENABLE_TLS",
		"REDIS_SENTINEL_ADDRS", "REDIS_MASTER_NAME", "REDIS_SENTINEL_USERNAME", "REDIS_SENTINEL_PASSWORD", "REDIS_TLS_SERVER_NAME",
		"TZ", "TIMEZONE",
	} {
		t.Setenv(name, "")
	}
}

func TestSetupConfigFromEnvKeepsDiscreteDefaults(t *testing.T) {
	clearConnectionEnv(t)
	t.Setenv("DATABASE_HOST", "postgres")
	t.Setenv("DATABASE_PASSWORD", "pw")
	t.Setenv("REDIS_HOST", "redis")

	cfg, err := setupConfigFromEnv()
	require.NoError(t, err)
	require.Equal(t, DatabaseConfig{Host: "postgres", Port: 5432, User: "postgres", Password: "pw", DBName: "sub2api", SSLMode: "disable"}, cfg.Database)
	require.Equal(t, RedisConfig{Host: "redis", Port: 6379}, cfg.Redis)
	require.Equal(t, "Asia/Shanghai", cfg.Timezone)
}

func TestSetupConfigFromEnvHonoursConnectionURLs(t *testing.T) {
	clearConnectionEnv(t)
	t.Setenv("DATABASE_HOST", "ignored")
	t.Setenv("DATABASE_URL", "postgres://app:secret@db.internal:5433/sub2api?sslmode=require&sslrootcert=/certs/ca.pem")
	t.Setenv("REDIS_HOST", "ignored")
	t.Setenv("REDIS_PASSWORD", "ignored")
	t.Setenv("REDIS_URL", "rediss://:redispw@cache.internal:6380/2")
	t.Setenv("TZ", "UTC")

	cfg, err := setupConfigFromEnv()
	require.NoError(t, err)

	require.Equal(t, DatabaseConfig{
		Host: "db.internal", Port: 5433, User: "app", Password: "secret", DBName: "sub2api", SSLMode: "require",
		ExtraParams: map[string]string{"sslrootcert": "/certs/ca.pem"},
	}, cfg.Database)
	require.Equal(t, RedisConfig{Host: "cache.internal", Port: 6380, Password: "redispw", DB: 2, EnableTLS: true}, cfg.Redis)

	// 安装阶段的每一条 DSN 都带着 URL 给出的全部参数，维护库连接也不例外
	bootstrapDSN, targetDSN := buildDatabaseConnectionDSNs(&cfg.Database)
	require.Equal(t, "host=db.internal port=5433 user=app password=secret dbname=postgres sslmode=require sslrootcert=/certs/ca.pem", bootstrapDSN)
	require.Equal(t, "host=db.internal port=5433 user=app password=secret dbname=sub2api sslmode=require sslrootcert=/certs/ca.pem", targetDSN)
	require.Equal(t, targetDSN, cfg.Database.connection().DSN())
}

func TestSetupConfigFromEnvRejectsConflictingTimezone(t *testing.T) {
	clearConnectionEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app:secret@db.internal:5432/sub2api?TimeZone=UTC")
	t.Setenv("TZ", "Asia/Shanghai")

	_, err := setupConfigFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "TimeZone")
}

func TestSetupConfigFromEnvSentinel(t *testing.T) {
	clearConnectionEnv(t)
	t.Setenv("REDIS_SENTINEL_ADDRS", "s1:26379, s2:26379,s3:26379")
	t.Setenv("REDIS_MASTER_NAME", "mymaster")
	t.Setenv("REDIS_SENTINEL_USERNAME", "sentinel")
	t.Setenv("REDIS_SENTINEL_PASSWORD", "sentinel-pw")
	t.Setenv("REDIS_ENABLE_TLS", "true")
	t.Setenv("REDIS_TLS_SERVER_NAME", "redis.internal")

	cfg, err := setupConfigFromEnv()
	require.NoError(t, err)
	require.Equal(t, []string{"s1:26379", "s2:26379", "s3:26379"}, cfg.Redis.SentinelAddrs)
	require.Equal(t, "mymaster", cfg.Redis.MasterName)
	require.Equal(t, "sentinel", cfg.Redis.SentinelUsername)
	require.Equal(t, "sentinel-pw", cfg.Redis.SentinelPassword)
	require.Equal(t, "redis.internal", cfg.Redis.TLSServerName)
	require.True(t, cfg.Redis.EnableTLS)

	t.Setenv("REDIS_MASTER_NAME", "")
	_, err = setupConfigFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis.master_name")
}

func TestSetupConfigFromEnvRejectsRedisCluster(t *testing.T) {
	clearConnectionEnv(t)
	t.Setenv("REDIS_CLUSTER_ADDRS", "a:7000,b:7000")

	_, err := setupConfigFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "REDIS_CLUSTER_ADDRS")
	require.Contains(t, err.Error(), "CROSSSLOT")
}

func TestWriteConfigFilePersistsConnectionTopology(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	require.NoError(t, writeConfigFile(&SetupConfig{
		Database: DatabaseConfig{Host: "db", Port: 5432, DBName: "sub2api", ExtraParams: map[string]string{"sslrootcert": "/certs/ca.pem"}},
		Redis: RedisConfig{
			SentinelAddrs:    []string{"s1:26379", "s2:26379"},
			MasterName:       "mymaster",
			SentinelPassword: "sentinel-pw",
			TLSServerName:    "redis.internal",
			EnableTLS:        true,
		},
	}))

	data, err := os.ReadFile(GetConfigFilePath())
	require.NoError(t, err)
	text := string(data)
	for _, want := range []string{
		"extra_params:", "sslrootcert: /certs/ca.pem",
		"sentinel_addrs:", "- s1:26379", "- s2:26379",
		"master_name: mymaster", "sentinel_password: sentinel-pw", "tls_server_name: redis.internal",
	} {
		require.Contains(t, text, want)
	}

	// 单机安装写出的文件不带拓扑字段，保持与以前一致
	require.NoError(t, writeConfigFile(&SetupConfig{Redis: RedisConfig{Host: "redis", Port: 6379}}))
	data, err = os.ReadFile(GetConfigFilePath())
	require.NoError(t, err)
	require.False(t, strings.Contains(string(data), "sentinel_addrs"))
	require.False(t, strings.Contains(string(data), "extra_params"))
}

func TestTestRedisConnectionUsesSharedClient(t *testing.T) {
	server := miniredis.RunT(t)
	host, portText, err := net.SplitHostPort(server.Addr())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	require.NoError(t, TestRedisConnection(&RedisConfig{Host: host, Port: port}))

	server.Close()
	err = TestRedisConnection(&RedisConfig{Host: host, Port: port})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ping failed")
}
