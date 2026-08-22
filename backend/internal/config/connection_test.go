package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestApplyDatabaseURLRoundTripsIntoDSN(t *testing.T) {
	var db DatabaseConfig
	require.NoError(t, ApplyDatabaseURL(&db,
		"postgres://app:p%40ss%20word@db.internal:5433/sub2api?sslmode=verify-full&sslrootcert=/certs/ca.pem&application_name=sub2api"))

	require.Equal(t, "db.internal", db.Host)
	require.Equal(t, 5433, db.Port)
	require.Equal(t, "app", db.User)
	require.Equal(t, "p@ss word", db.Password)
	require.Equal(t, "sub2api", db.DBName)
	require.Equal(t, "verify-full", db.SSLMode)
	require.Equal(t, map[string]string{"sslrootcert": "/certs/ca.pem", "application_name": "sub2api"}, db.ExtraParams)

	// 额外参数按名排序、带空格的值按 libpq 规则加引号，输出是确定的
	wantDSN := "host=db.internal port=5433 user=app password='p@ss word' dbname=sub2api sslmode=verify-full application_name=sub2api sslrootcert=/certs/ca.pem"
	require.Equal(t, wantDSN, db.DSN())
	require.Equal(t, wantDSN+" TimeZone=UTC", db.DSNWithTimezone("UTC"))
	require.Equal(t, strings.Replace(wantDSN, "dbname=sub2api", "dbname=postgres", 1), db.DSNForDatabase("postgres"))

	// lib/pq 必须能解析渲染结果（引号/转义规则与它一致）
	_, err := pq.NewConnector(db.DSNWithTimezone("UTC"))
	require.NoError(t, err)
}

func TestApplyDatabaseURLDefaultsFollowURLSemantics(t *testing.T) {
	db := DatabaseConfig{Host: "old", Port: 1, User: "old", Password: "old", DBName: "old", SSLMode: "disable", ExtraParams: map[string]string{"sslrootcert": "/old"}}
	require.NoError(t, ApplyDatabaseURL(&db, "postgresql://db.internal/app"))

	// URL 没写的部分取 URL 语义下的默认值，而不是沿用离散字段
	require.Equal(t, "db.internal", db.Host)
	require.Equal(t, 5432, db.Port)
	require.Empty(t, db.User)
	require.Empty(t, db.Password)
	require.Equal(t, "app", db.DBName)
	require.Empty(t, db.SSLMode)
	require.Nil(t, db.ExtraParams)
	require.Equal(t, "host=db.internal port=5432 dbname=app", db.DSN())
}

func TestApplyDatabaseURLEmptyLeavesFieldsUntouched(t *testing.T) {
	db := DatabaseConfig{Host: "keep", Port: 5432, DBName: "keep"}
	require.NoError(t, ApplyDatabaseURL(&db, "   "))
	require.Equal(t, DatabaseConfig{Host: "keep", Port: 5432, DBName: "keep"}, db)
}

func TestApplyDatabaseURLRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name, raw, wantErr string
	}{
		{"unknown param names the param", "postgres://u:p@h:5432/db?sslmod=disable", `unknown parameter "sslmod"`},
		{"reserved param must live in the URL body", "postgres://u:p@h:5432/db?host=other", `"host" must be written in the URL itself`},
		{"duplicate param", "postgres://u:p@h:5432/db?sslrootcert=a&sslrootcert=b", `given 2 times`},
		{"scheme", "mysql://u:p@h:3306/db", "scheme must be postgres:// or postgresql://"},
		{"missing host", "postgres:///db", "host is required"},
		{"missing dbname", "postgres://u:p@h:5432", "path must be the database name"},
		{"nested path", "postgres://u:p@h:5432/db/extra", "path must be the database name"},
		{"bad port", "postgres://u:p@h:99999/db", `invalid port "99999"`},
		{"unparsable", "postgres://u:p@h:abc/db", "invalid port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var db DatabaseConfig
			err := ApplyDatabaseURL(&db, tc.raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
			// 错误文本会进启动日志，绝不能带出密码
			require.NotContains(t, err.Error(), "u:p@")
		})
	}
}

func TestDatabaseConfigResolveTimezoneRule(t *testing.T) {
	t.Run("conflicting TimeZone is an error", func(t *testing.T) {
		db := DatabaseConfig{URL: "postgres://u:p@h:5432/db?TimeZone=UTC"}
		err := db.Resolve("Asia/Shanghai")
		require.Error(t, err)
		require.Contains(t, err.Error(), `TimeZone="UTC"`)
		require.Contains(t, err.Error(), `timezone="Asia/Shanghai"`)
	})
	t.Run("matching TimeZone is dropped, the application timezone renders it", func(t *testing.T) {
		db := DatabaseConfig{URL: "postgres://u:p@h:5432/db?timezone=utc"}
		require.NoError(t, db.Resolve("UTC"))
		require.Nil(t, db.ExtraParams)
		require.Equal(t, "host=h port=5432 user=u password=p dbname=db TimeZone=UTC", db.DSNWithTimezone("UTC"))
	})
	t.Run("config file extra_params are canonicalised through the same table", func(t *testing.T) {
		db := DatabaseConfig{Host: "h", Port: 5432, DBName: "db", ExtraParams: map[string]string{"SSLROOTCERT": "/ca.pem", "bogus": "x"}}
		err := db.Resolve("UTC")
		require.Error(t, err)
		require.Contains(t, err.Error(), `unknown parameter "bogus"`)

		db.ExtraParams = map[string]string{"SSLROOTCERT": "/ca.pem"}
		require.NoError(t, db.Resolve("UTC"))
		require.Equal(t, map[string]string{"sslrootcert": "/ca.pem"}, db.ExtraParams)
	})
}

func TestDatabaseConfigValidateConnection(t *testing.T) {
	base := DatabaseConfig{Host: "h", Port: 5432, DBName: "db", SSLMode: "disable"}
	require.NoError(t, base.ValidateConnection())

	cases := []struct {
		name    string
		mutate  func(d *DatabaseConfig)
		wantErr string
	}{
		{"port zero", func(d *DatabaseConfig) { d.Port = 0 }, "database.port"},
		{"port too large", func(d *DatabaseConfig) { d.Port = 70000 }, "database.port"},
		{"sslmode typo", func(d *DatabaseConfig) { d.SSLMode = "disabled" }, "database.sslmode"},
		{"extra param typo", func(d *DatabaseConfig) { d.ExtraParams = map[string]string{"sslrootcrt": "/ca"} }, `unknown parameter "sslrootcrt"`},
		{"extra param must not shadow a field", func(d *DatabaseConfig) { d.ExtraParams = map[string]string{"password": "x"} }, `"password" must be set through its own field`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.mutate(&d)
			err := d.ValidateConnection()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestQuoteDSNValue(t *testing.T) {
	cases := map[string]string{
		"plain":           "plain",
		"with space":      "'with space'",
		"it's":            `'it\'s'`,
		`back\slash`:      `'back\\slash'`,
		"tab\there":       "'tab\there'",
		"p@ss#word=value": "p@ss#word=value",
	}
	for in, want := range cases {
		require.Equal(t, want, quoteDSNValue(in), "input %q", in)
	}
}

func TestApplyRedisURL(t *testing.T) {
	t.Run("full form", func(t *testing.T) {
		r := RedisConfig{Host: "old", Port: 1}
		require.NoError(t, ApplyRedisURL(&r, "redis://app:secret@cache.internal:6380/2"))
		require.Equal(t, "cache.internal", r.Host)
		require.Equal(t, 6380, r.Port)
		require.Equal(t, "app", r.Username)
		require.Equal(t, "secret", r.Password)
		require.Equal(t, 2, r.DB)
		require.False(t, r.EnableTLS)
	})
	t.Run("rediss enables TLS and defaults the port", func(t *testing.T) {
		r := RedisConfig{Password: "discrete"}
		require.NoError(t, ApplyRedisURL(&r, "rediss://cache.internal"))
		require.Equal(t, "cache.internal", r.Host)
		require.Equal(t, 6379, r.Port)
		require.True(t, r.EnableTLS)
		// URL 是唯一来源：它没写密码，离散的密码就不再生效
		require.Empty(t, r.Password)
		require.Equal(t, 0, r.DB)
	})
	t.Run("db via query parameter", func(t *testing.T) {
		var r RedisConfig
		require.NoError(t, ApplyRedisURL(&r, "redis://cache:6379?db=3"))
		require.Equal(t, 3, r.DB)
	})
	t.Run("empty leaves fields untouched", func(t *testing.T) {
		r := RedisConfig{Host: "keep", Port: 6379}
		require.NoError(t, ApplyRedisURL(&r, ""))
		require.Equal(t, RedisConfig{Host: "keep", Port: 6379}, r)
	})

	cases := []struct {
		name, raw, wantErr string
	}{
		{"tuning parameters are not accepted", "redis://cache:6379?pool_size=10", `query parameter "pool_size" is not supported`},
		{"unix sockets are not addressable", "unix:///tmp/redis.sock", "scheme must be redis:// or rediss://"},
		{"bad database index", "redis://cache:6379/x", "invalid database number"},
		{"bad port", "redis://u:p@cache:abc", "invalid port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r RedisConfig
			err := ApplyRedisURL(&r, tc.raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
			require.NotContains(t, err.Error(), "u:p@")
		})
	}
}

func TestRedisConfigResolveNormalisesSentinelAddrs(t *testing.T) {
	r := RedisConfig{
		Host:          "ignored",
		Port:          6379,
		SentinelAddrs: []string{" s1:26379, s2:26379 ", "", "s3:26379"},
		MasterName:    " mymaster ",
		TLSServerName: " redis.internal ",
	}
	require.NoError(t, r.Resolve())
	require.Equal(t, []string{"s1:26379", "s2:26379", "s3:26379"}, r.SentinelAddrs)
	require.Equal(t, "mymaster", r.MasterName)
	require.Equal(t, "redis.internal", r.TLSServerName)
	require.True(t, r.SentinelEnabled())

	single := RedisConfig{Host: "cache", Port: 6379}
	require.NoError(t, single.Resolve())
	require.False(t, single.SentinelEnabled())
}

func TestRedisConfigValidateConnection(t *testing.T) {
	cases := []struct {
		name    string
		cfg     RedisConfig
		wantErr string
	}{
		{"sentinels without master name", RedisConfig{SentinelAddrs: []string{"s1:26379"}}, "must be set together"},
		{"master name without sentinels", RedisConfig{Host: "h", Port: 6379, MasterName: "m"}, "must be set together"},
		{"sentinel addr without port", RedisConfig{SentinelAddrs: []string{"s1"}, MasterName: "m"}, `"s1" must be host:port`},
		{"sentinel addr bad port", RedisConfig{SentinelAddrs: []string{"s1:abc"}, MasterName: "m"}, "invalid port"},
		{"sentinel username without password", RedisConfig{SentinelAddrs: []string{"s1:26379"}, MasterName: "m", SentinelUsername: "u"}, "redis.sentinel_username requires redis.sentinel_password"},
		{"single mode needs a host", RedisConfig{Port: 6379}, "redis.host is required"},
		{"single mode needs a valid port", RedisConfig{Host: "h"}, "redis.port"},
		{"negative db", RedisConfig{Host: "h", Port: 6379, DB: -1}, "redis.db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateConnection()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// host/port 与哨兵同时配置是允许的：host/port 不用，但不是错误
	both := RedisConfig{Host: "h", Port: 6379, SentinelAddrs: []string{"s1:26379"}, MasterName: "m"}
	require.NoError(t, both.ValidateConnection())
}

func TestCheckRedisClusterUnsupported(t *testing.T) {
	require.NoError(t, CheckRedisClusterUnsupported([]string{"redis.host", "redis.sentinel_addrs"}, []string{"REDIS_HOST=x", "PATH=/bin"}))

	err := CheckRedisClusterUnsupported([]string{"redis.cluster_addrs"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis.cluster_addrs")
	require.Contains(t, err.Error(), "CROSSSLOT")
	require.Contains(t, err.Error(), "concurrency_cache.go")
	require.Contains(t, err.Error(), "scheduler_cache.go")
	require.Contains(t, err.Error(), "redis.sentinel_addrs")

	err = CheckRedisClusterUnsupported(nil, []string{"REDIS_CLUSTER_ADDRS=a:1,b:2"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "REDIS_CLUSTER_ADDRS")
	require.NotContains(t, err.Error(), "a:1,b:2")
}

func TestEnvName(t *testing.T) {
	require.Equal(t, "REDIS_SENTINEL_ADDRS", EnvName("redis.sentinel_addrs"))
	require.Equal(t, "DATABASE_URL", EnvName("database.url"))
}

// clearConnectionEnv 屏蔽开发机 shell 里可能存在的连接变量，让下面的 Load 测试只看到自己设置的值。
func clearConnectionEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"DATABASE_URL", "REDIS_URL", "REDIS_SENTINEL_ADDRS", "REDIS_MASTER_NAME", "REDIS_SENTINEL_USERNAME", "REDIS_SENTINEL_PASSWORD", "REDIS_TLS_SERVER_NAME"} {
		t.Setenv(name, "")
	}
}

func TestLoadDatabaseURLOverridesDiscreteFields(t *testing.T) {
	resetViperWithJWTSecret(t)
	clearConnectionEnv(t)
	t.Setenv("DATABASE_HOST", "ignored.host")
	t.Setenv("DATABASE_SSLMODE", "disable")
	t.Setenv("DATABASE_URL", "postgres://app:secret@db.example:5433/app?sslmode=require&sslrootcert=/certs/ca.pem")
	t.Setenv("TZ", "UTC")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "db.example", cfg.Database.Host)
	require.Equal(t, 5433, cfg.Database.Port)
	require.Equal(t, "app", cfg.Database.User)
	require.Equal(t, "secret", cfg.Database.Password)
	require.Equal(t, "app", cfg.Database.DBName)
	require.Equal(t, "require", cfg.Database.SSLMode)
	require.Equal(t, "host=db.example port=5433 user=app password=secret dbname=app sslmode=require sslrootcert=/certs/ca.pem TimeZone=UTC",
		cfg.Database.DSNWithTimezone(cfg.Timezone))
	// 连接池参数不属于 URL，继续由离散配置控制
	require.Equal(t, 256, cfg.Database.MaxOpenConns)
}

func TestLoadDatabaseURLTimezoneConflictFails(t *testing.T) {
	resetViperWithJWTSecret(t)
	clearConnectionEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app:secret@db.example:5432/app?TimeZone=UTC")
	t.Setenv("TZ", "Asia/Shanghai")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "TimeZone")
}

func TestLoadRedisURLOverridesDiscreteFields(t *testing.T) {
	resetViperWithJWTSecret(t)
	clearConnectionEnv(t)
	t.Setenv("REDIS_HOST", "ignored.host")
	t.Setenv("REDIS_PASSWORD", "ignored")
	t.Setenv("REDIS_URL", "rediss://:redispw@cache.example:6380/1")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "cache.example", cfg.Redis.Host)
	require.Equal(t, 6380, cfg.Redis.Port)
	require.Equal(t, "redispw", cfg.Redis.Password)
	require.Equal(t, 1, cfg.Redis.DB)
	require.True(t, cfg.Redis.EnableTLS)
	require.Equal(t, 1024, cfg.Redis.PoolSize)
}

func TestLoadRedisSentinelFromEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	clearConnectionEnv(t)
	t.Setenv("REDIS_SENTINEL_ADDRS", "s1:26379, s2:26379,s3:26379")
	t.Setenv("REDIS_MASTER_NAME", "mymaster")
	t.Setenv("REDIS_SENTINEL_PASSWORD", "sentinel-pw")
	t.Setenv("REDIS_TLS_SERVER_NAME", "redis.internal")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, []string{"s1:26379", "s2:26379", "s3:26379"}, cfg.Redis.SentinelAddrs)
	require.Equal(t, "mymaster", cfg.Redis.MasterName)
	require.Equal(t, "sentinel-pw", cfg.Redis.SentinelPassword)
	require.Equal(t, "redis.internal", cfg.Redis.TLSServerName)
	require.True(t, cfg.Redis.SentinelEnabled())
}

func TestLoadRedisSentinelRequiresMasterName(t *testing.T) {
	resetViperWithJWTSecret(t)
	clearConnectionEnv(t)
	t.Setenv("REDIS_SENTINEL_ADDRS", "s1:26379")

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis.master_name")
}

func TestLoadRejectsRedisClusterSettings(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		clearConnectionEnv(t)
		t.Setenv("REDIS_CLUSTER_ADDRS", "a:7000,b:7000")

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "REDIS_CLUSTER_ADDRS")
		require.Contains(t, err.Error(), "CROSSSLOT")
	})
	t.Run("config file", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		clearConnectionEnv(t)
		configFile := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(configFile, []byte("redis:\n  cluster_addrs: [\"a:7000\"]\n"), 0o600))
		t.Setenv("CONFIG_FILE", configFile)

		_, err := Load()
		require.Error(t, err)
		require.Contains(t, err.Error(), "redis.cluster_addrs")
	})
}
