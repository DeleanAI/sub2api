package setup

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// clearConnectionEnv 屏蔽开发机 shell 里可能存在的连接变量，让测试只看到自己设置的值。
func clearConnectionEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"DATABASE_URL", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_USER", "DATABASE_PASSWORD", "DATABASE_DBNAME", "DATABASE_SSLMODE",
		"REDIS_URL", "REDIS_HOST", "REDIS_PORT", "REDIS_USERNAME", "REDIS_PASSWORD", "REDIS_DB", "REDIS_ENABLE_TLS",
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

func TestWriteConfigFilePersistsExtraParams(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	require.NoError(t, writeConfigFile(&SetupConfig{
		Database: DatabaseConfig{Host: "db", Port: 5432, DBName: "sub2api", ExtraParams: map[string]string{"sslrootcert": "/certs/ca.pem"}},
	}))

	data, err := os.ReadFile(GetConfigFilePath())
	require.NoError(t, err)
	require.Contains(t, string(data), "extra_params:")
	require.Contains(t, string(data), "sslrootcert: /certs/ca.pem")

	// 没有额外参数时写出的文件与以前一致
	require.NoError(t, writeConfigFile(&SetupConfig{Database: DatabaseConfig{Host: "db", Port: 5432, DBName: "sub2api"}}))
	data, err = os.ReadFile(GetConfigFilePath())
	require.NoError(t, err)
	require.False(t, strings.Contains(string(data), "extra_params"))
}
