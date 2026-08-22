package setup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"

	_ "github.com/lib/pq"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Config paths
const (
	ConfigFileName             = "config.yaml"
	InstallLockFile            = ".installed"
	defaultUserConcurrency     = 5
	simpleModeAdminConcurrency = 30
	defaultMigrationTimeout    = 60 * time.Second
)

func setupDefaultAdminConcurrency() int {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RUN_MODE")), config.RunModeSimple) {
		return simpleModeAdminConcurrency
	}
	return defaultUserConcurrency
}

// GetDataDir returns the directory holding config.yaml and the install lock file.
// 解析规则只有 config.ResolveDataDir 一份；这里只声明用途相关的回退值：
// 裸机安装时安装标记和二进制放在工作目录（/opt/sub2api），运行期数据在 ./data。
func GetDataDir() string {
	return config.ResolveDataDir(".")
}

// GetConfigFilePath returns the full path to config.yaml
func GetConfigFilePath() string {
	return GetDataDir() + "/" + ConfigFileName
}

// GetInstallLockPath returns the full path to .installed lock file
func GetInstallLockPath() string {
	return GetDataDir() + "/" + InstallLockFile
}

// SetupConfig holds the setup configuration
type SetupConfig struct {
	Database                DatabaseConfig `json:"database" yaml:"database"`
	Redis                   RedisConfig    `json:"redis" yaml:"redis"`
	Admin                   AdminConfig    `json:"admin" yaml:"-"` // Not stored in config file
	Server                  ServerConfig   `json:"server" yaml:"server"`
	JWT                     JWTConfig      `json:"jwt" yaml:"jwt"`
	Totp                    TotpConfig     `json:"totp" yaml:"totp"`
	Timezone                string         `json:"timezone" yaml:"timezone"` // e.g. "Asia/Shanghai", "UTC"
	MigrationTimeoutSeconds int            `json:"migration_timeout_seconds" yaml:"migration_timeout_seconds,omitempty"`
}

// TotpConfig 是落库密文密钥（名字沿用配置键 totp.encryption_key）。
type TotpConfig struct {
	EncryptionKey string `json:"encryption_key" yaml:"encryption_key"`
}

// DatabaseConfig 是安装向导的数据库连接视图：JSON 面向向导表单，YAML 面向写出的 config.yaml。
// 连接规则（DSN 渲染、DATABASE_URL 解析）不在这里实现，统一通过 connection() 交给 config 包。
type DatabaseConfig struct {
	Host     string `json:"host" yaml:"host"`
	Port     int    `json:"port" yaml:"port"`
	User     string `json:"user" yaml:"user"`
	Password string `json:"password" yaml:"password"`
	DBName   string `json:"dbname" yaml:"dbname"`
	SSLMode  string `json:"sslmode" yaml:"sslmode"`
	// ExtraParams 是 DATABASE_URL 带来的其余 libpq 参数（如 sslrootcert）。写进 config.yaml 是为了
	// 环境变量撤掉之后连接参数仍然完整；它不是向导表单的一部分，所以不接受 JSON 输入。
	ExtraParams map[string]string `json:"-" yaml:"extra_params,omitempty"`
}

// connection 转成 config 包的连接描述：DSN 渲染与 URL 解析只在 config 包实现一次，安装流程不再自己拼 DSN。
func (c *DatabaseConfig) connection() *config.DatabaseConfig {
	return &config.DatabaseConfig{
		Host:        c.Host,
		Port:        c.Port,
		User:        c.User,
		Password:    c.Password,
		DBName:      c.DBName,
		SSLMode:     c.SSLMode,
		ExtraParams: c.ExtraParams,
	}
}

// resolve 让自动安装与主配置走同一条优先级规则（config.DatabaseConfig.Resolve）：
// rawURL 非空时覆盖离散字段，会话时区以 appTimezone 为准。
func (c *DatabaseConfig) resolve(rawURL, appTimezone string) error {
	conn := c.connection()
	conn.URL = rawURL
	if err := conn.Resolve(appTimezone); err != nil {
		return err
	}
	c.Host = conn.Host
	c.Port = conn.Port
	c.User = conn.User
	c.Password = conn.Password
	c.DBName = conn.DBName
	c.SSLMode = conn.SSLMode
	c.ExtraParams = conn.ExtraParams
	return nil
}

// RedisConfig 是安装向导的 Redis 连接视图。Sentinel/TLS 名称字段只来自环境变量（AutoSetupFromEnv），
// 向导表单不暴露它们，但要写进 config.yaml，环境变量撤掉之后拓扑信息才不会丢。
type RedisConfig struct {
	Host      string `json:"host" yaml:"host"`
	Port      int    `json:"port" yaml:"port"`
	Username  string `json:"username" yaml:"username"`
	Password  string `json:"password" yaml:"password"`
	DB        int    `json:"db" yaml:"db"`
	EnableTLS bool   `json:"enable_tls" yaml:"enable_tls"`

	TLSServerName    string   `json:"-" yaml:"tls_server_name,omitempty"`
	SentinelAddrs    []string `json:"-" yaml:"sentinel_addrs,omitempty"`
	MasterName       string   `json:"-" yaml:"master_name,omitempty"`
	SentinelUsername string   `json:"-" yaml:"sentinel_username,omitempty"`
	SentinelPassword string   `json:"-" yaml:"sentinel_password,omitempty"`
}

// connection 转成 config 包的连接描述，客户端由 repository.NewRedisClient 按同一规则构造。
func (c *RedisConfig) connection() *config.RedisConfig {
	return &config.RedisConfig{
		Host:             c.Host,
		Port:             c.Port,
		Username:         c.Username,
		Password:         c.Password,
		DB:               c.DB,
		EnableTLS:        c.EnableTLS,
		TLSServerName:    c.TLSServerName,
		SentinelAddrs:    c.SentinelAddrs,
		MasterName:       c.MasterName,
		SentinelUsername: c.SentinelUsername,
		SentinelPassword: c.SentinelPassword,
	}
}

// resolve 让自动安装与主配置走同一条规则（config.RedisConfig.Resolve）：
// rawURL 非空时覆盖离散字段，哨兵地址按逗号拆分并校验。
func (c *RedisConfig) resolve(rawURL string) error {
	conn := c.connection()
	conn.URL = rawURL
	if err := conn.Resolve(); err != nil {
		return err
	}
	c.Host = conn.Host
	c.Port = conn.Port
	c.Username = conn.Username
	c.Password = conn.Password
	c.DB = conn.DB
	c.EnableTLS = conn.EnableTLS
	c.TLSServerName = conn.TLSServerName
	c.SentinelAddrs = conn.SentinelAddrs
	c.MasterName = conn.MasterName
	c.SentinelUsername = conn.SentinelUsername
	c.SentinelPassword = conn.SentinelPassword
	return nil
}

type AdminConfig struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type ServerConfig struct {
	Host string `json:"host" yaml:"host"`
	Port int    `json:"port" yaml:"port"`
	Mode string `json:"mode" yaml:"mode"`
}

type JWTConfig struct {
	Secret     string `json:"secret" yaml:"secret"`
	ExpireHour int    `json:"expire_hour" yaml:"expire_hour"`
}

const (
	adminBootstrapReasonEmptyDatabase          = "empty_database"
	adminBootstrapReasonAdminExists            = "admin_exists"
	adminBootstrapReasonUsersExistWithoutAdmin = "users_exist_without_admin"
)

type adminBootstrapDecision struct {
	shouldCreate bool
	reason       string
}

func decideAdminBootstrap(totalUsers, adminUsers int64) adminBootstrapDecision {
	if adminUsers > 0 {
		return adminBootstrapDecision{
			shouldCreate: false,
			reason:       adminBootstrapReasonAdminExists,
		}
	}
	if totalUsers > 0 {
		return adminBootstrapDecision{
			shouldCreate: false,
			reason:       adminBootstrapReasonUsersExistWithoutAdmin,
		}
	}
	return adminBootstrapDecision{
		shouldCreate: true,
		reason:       adminBootstrapReasonEmptyDatabase,
	}
}

func skipSetupEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SKIP_SETUP"))) {
	case "true", "1", "yes":
		return true
	default:
		return false
	}
}

// NeedsSetup checks if the system needs initial setup
// Uses multiple checks to prevent attackers from forcing re-setup by deleting config
func NeedsSetup() bool {
	if skipSetupEnabled() {
		logger.L().Debug("setup.needs_setup_bypassed", zap.String("reason", "skip_setup_enabled"))
		return false
	}

	// Check 1: Config file must not exist
	if _, err := os.Stat(GetConfigFilePath()); !os.IsNotExist(err) {
		return false // Config exists, no setup needed
	}

	// Check 2: Installation lock file (harder to bypass)
	if _, err := os.Stat(GetInstallLockPath()); !os.IsNotExist(err) {
		return false // Lock file exists, already installed
	}

	return true
}

func buildDatabaseConnectionDSNs(cfg *DatabaseConfig) (bootstrapDSN, targetDSN string) {
	conn := cfg.connection()
	return conn.DSNForDatabase("postgres"), conn.DSN()
}

// TestDatabaseConnection tests the database connection and creates database if not exists
func TestDatabaseConnection(cfg *DatabaseConfig) error {
	// First, connect to the default 'postgres' database to check/create target database.
	// Connecting to cfg.DBName here fails when the target database has not been
	// created yet, so the bootstrap connection must use PostgreSQL's maintenance DB.
	defaultDSN, targetDSN := buildDatabaseConnectionDSNs(cfg)

	db, err := sql.Open("postgres", defaultDSN)
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}

	defer func() {
		if db == nil {
			return
		}
		if err := db.Close(); err != nil {
			logger.LegacyPrintf("setup", "failed to close postgres connection: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}

	// Check if target database exists
	var exists bool
	row := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", cfg.DBName)
	if err := row.Scan(&exists); err != nil {
		return fmt.Errorf("failed to check database existence: %w", err)
	}

	// Create database if not exists
	if !exists {
		// 注意：数据库名不能参数化，依赖前置输入校验保障安全。
		// Note: Database names cannot be parameterized, but we've already validated cfg.DBName
		// in the handler using validateDBName() which only allows [a-zA-Z][a-zA-Z0-9_]*
		_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", cfg.DBName))
		if err != nil {
			return fmt.Errorf("failed to create database '%s': %w", cfg.DBName, err)
		}
		logger.LegacyPrintf("setup", "Database '%s' created successfully", cfg.DBName)
	}

	// Now connect to the target database to verify
	if err := db.Close(); err != nil {
		logger.LegacyPrintf("setup", "failed to close postgres connection: %v", err)
	}
	db = nil

	targetDB, err := sql.Open("postgres", targetDSN)
	if err != nil {
		return fmt.Errorf("failed to connect to database '%s': %w", cfg.DBName, err)
	}

	defer func() {
		if err := targetDB.Close(); err != nil {
			logger.LegacyPrintf("setup", "failed to close postgres connection: %v", err)
		}
	}()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	if err := targetDB.PingContext(ctx2); err != nil {
		return fmt.Errorf("ping target database failed: %w", err)
	}

	return nil
}

// TestRedisConnection tests the Redis connection.
// 客户端由 repository.NewRedisClient 构造——与主服务同一套单机/Sentinel/TLS 规则，
// 向导测通的连接就是服务启动后实际使用的连接。
func TestRedisConnection(cfg *RedisConfig) error {
	rdb := repository.NewRedisClient(cfg.connection())
	defer func() {
		if err := rdb.Close(); err != nil {
			logger.LegacyPrintf("setup", "failed to close redis client: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}

	return nil
}

// Install performs the installation with the given configuration
func Install(cfg *SetupConfig) error {
	// Security check: prevent re-installation if already installed
	if !NeedsSetup() {
		return fmt.Errorf("system is already installed, re-installation is not allowed")
	}

	// Generate JWT secret if not provided
	if cfg.JWT.Secret == "" {
		secret, err := generateSecret(32)
		if err != nil {
			return fmt.Errorf("failed to generate jwt secret: %w", err)
		}
		cfg.JWT.Secret = secret
		logger.LegacyPrintf("setup", "%s", "Warning: JWT secret auto-generated. Consider setting a fixed secret for production.")
	}
	if err := ensureEncryptionKey(cfg); err != nil {
		return err
	}

	// Test connections
	if err := TestDatabaseConnection(&cfg.Database); err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}

	if err := TestRedisConnection(&cfg.Redis); err != nil {
		return fmt.Errorf("redis connection failed: %w", err)
	}

	// Initialize database
	if err := initializeDatabase(cfg); err != nil {
		return fmt.Errorf("database initialization failed: %w", err)
	}

	// Create admin user (only when database is empty and no admin exists).
	if _, _, err := createAdminUser(cfg); err != nil {
		return fmt.Errorf("admin user creation failed: %w", err)
	}

	// Write config file
	if err := writeConfigFile(cfg); err != nil {
		return fmt.Errorf("config file creation failed: %w", err)
	}

	// Create installation lock file to prevent re-setup attacks
	if err := createInstallLock(); err != nil {
		return fmt.Errorf("failed to create install lock: %w", err)
	}

	return nil
}

// createInstallLock creates a lock file to prevent re-installation attacks
func createInstallLock() error {
	content := fmt.Sprintf("installed_at=%s\n", time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(GetInstallLockPath(), []byte(content), 0400) // Read-only for owner
}

func initializeDatabase(cfg *SetupConfig) error {
	db, err := sql.Open("postgres", cfg.Database.connection().DSN())
	if err != nil {
		return err
	}

	defer func() {
		if err := db.Close(); err != nil {
			logger.LegacyPrintf("setup", "failed to close postgres connection: %v", err)
		}
	}()

	migrationCtx, cancel := context.WithTimeout(context.Background(), cfg.migrationTimeout())
	defer cancel()
	return repository.ApplyMigrations(migrationCtx, db)
}

func (cfg *SetupConfig) migrationTimeout() time.Duration {
	if cfg != nil && cfg.MigrationTimeoutSeconds > 0 {
		return time.Duration(cfg.MigrationTimeoutSeconds) * time.Second
	}
	return defaultMigrationTimeout
}

func createAdminUser(cfg *SetupConfig) (bool, string, error) {
	db, err := sql.Open("postgres", cfg.Database.connection().DSN())
	if err != nil {
		return false, "", err
	}

	defer func() {
		if err := db.Close(); err != nil {
			logger.LegacyPrintf("setup", "failed to close postgres connection: %v", err)
		}
	}()

	// 使用超时上下文避免安装流程因数据库异常而长时间阻塞。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var totalUsers int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM users").Scan(&totalUsers); err != nil {
		return false, "", err
	}
	var adminUsers int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM users WHERE role = $1", service.RoleAdmin).Scan(&adminUsers); err != nil {
		return false, "", err
	}
	decision := decideAdminBootstrap(totalUsers, adminUsers)
	if !decision.shouldCreate {
		return false, decision.reason, nil
	}

	if strings.TrimSpace(cfg.Admin.Password) == "" {
		password, genErr := generateSecret(16)
		if genErr != nil {
			return false, "", fmt.Errorf("failed to generate admin password: %w", genErr)
		}
		cfg.Admin.Password = password
		fmt.Printf("Generated admin password (one-time): %s\n", cfg.Admin.Password)
		fmt.Println("IMPORTANT: Save this password! It will not be shown again.")
	}

	admin := &service.User{
		Email:       cfg.Admin.Email,
		Role:        service.RoleAdmin,
		Status:      service.StatusActive,
		Balance:     0,
		Concurrency: setupDefaultAdminConcurrency(),
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	if err := admin.SetPassword(cfg.Admin.Password); err != nil {
		return false, "", err
	}

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO users (email, password_hash, role, balance, concurrency, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		admin.Email,
		admin.PasswordHash,
		admin.Role,
		admin.Balance,
		admin.Concurrency,
		admin.Status,
		admin.CreatedAt,
		admin.UpdatedAt,
	)
	if err != nil {
		return false, "", err
	}
	return true, decision.reason, nil
}

// ensureEncryptionKey 在初始化时为落库密文密钥补一个随机值并写进 config.yaml。
//
// 与 JWT secret 同一条规则：运维没给的长期密钥由向导生成一次并持久化，而不是
// 留空让服务每次启动各自生成。生成的密钥只对这一份 config.yaml 有效——再加
// 实例时必须把同一把密钥带过去（环境变量优先于 config.yaml），否则第二个
// 实例会在启动时被密钥指纹校验拒绝。
func ensureEncryptionKey(cfg *SetupConfig) error {
	cfg.Totp.EncryptionKey = strings.TrimSpace(cfg.Totp.EncryptionKey)
	if cfg.Totp.EncryptionKey != "" {
		return nil
	}
	key, err := generateSecret(32)
	if err != nil {
		return fmt.Errorf("failed to generate encryption key: %w", err)
	}
	cfg.Totp.EncryptionKey = key
	logger.LegacyPrintf("setup", "%s", "Warning: encryption key (totp.encryption_key) auto-generated and persisted to config.yaml. "+
		"Every additional instance must use this same key: set TOTP_ENCRYPTION_KEY explicitly for multi-instance deployments.")
	return nil
}

func writeConfigFile(cfg *SetupConfig) error {
	// Ensure timezone has a default value
	tz := cfg.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}

	// Prepare config for YAML (exclude sensitive data and admin config)
	yamlConfig := struct {
		Server   ServerConfig   `yaml:"server"`
		Database DatabaseConfig `yaml:"database"`
		Redis    RedisConfig    `yaml:"redis"`
		JWT      struct {
			Secret     string `yaml:"secret"`
			ExpireHour int    `yaml:"expire_hour"`
		} `yaml:"jwt"`
		Totp    TotpConfig `yaml:"totp"`
		Default struct {
			UserConcurrency int     `yaml:"user_concurrency"`
			UserBalance     float64 `yaml:"user_balance"`
			APIKeyPrefix    string  `yaml:"api_key_prefix"`
			RateMultiplier  float64 `yaml:"rate_multiplier"`
		} `yaml:"default"`
		RateLimit struct {
			RequestsPerMinute int `yaml:"requests_per_minute"`
			BurstSize         int `yaml:"burst_size"`
		} `yaml:"rate_limit"`
		Timezone string `yaml:"timezone"`
	}{
		Server:   cfg.Server,
		Database: cfg.Database,
		Redis:    cfg.Redis,
		JWT: struct {
			Secret     string `yaml:"secret"`
			ExpireHour int    `yaml:"expire_hour"`
		}{
			Secret:     cfg.JWT.Secret,
			ExpireHour: cfg.JWT.ExpireHour,
		},
		Totp: cfg.Totp,
		Default: struct {
			UserConcurrency int     `yaml:"user_concurrency"`
			UserBalance     float64 `yaml:"user_balance"`
			APIKeyPrefix    string  `yaml:"api_key_prefix"`
			RateMultiplier  float64 `yaml:"rate_multiplier"`
		}{
			UserConcurrency: defaultUserConcurrency,
			UserBalance:     0,
			APIKeyPrefix:    "sk-",
			RateMultiplier:  1.0,
		},
		RateLimit: struct {
			RequestsPerMinute int `yaml:"requests_per_minute"`
			BurstSize         int `yaml:"burst_size"`
		}{
			RequestsPerMinute: 60,
			BurstSize:         10,
		},
		Timezone: tz,
	}

	data, err := yaml.Marshal(&yamlConfig)
	if err != nil {
		return err
	}

	return os.WriteFile(GetConfigFilePath(), data, 0600)
}

func generateSecret(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// =============================================================================
// Auto Setup for Docker Deployment
// =============================================================================

// AutoSetupEnabled checks if auto setup is enabled via environment variable
func AutoSetupEnabled() bool {
	val := os.Getenv("AUTO_SETUP")
	return val == "true" || val == "1" || val == "yes"
}

// getEnvOrDefault gets environment variable or returns default value
func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

// getEnvIntOrDefault gets environment variable as int or returns default value
func getEnvIntOrDefault(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultValue
}

// setupConfigFromEnv builds the auto-setup configuration from environment variables.
//
// 离散变量（DATABASE_HOST、REDIS_HOST ...）在这里读取；DATABASE_URL / REDIS_URL 的优先级、
// REDIS_SENTINEL_ADDRS 的拆分校验、REDIS_CLUSTER_* 的拒绝都不在这里实现，而是交给 config 包里
// 主配置加载也在用的同一套规则——两条启动路径对同一组环境变量不能有两种解释。
func setupConfigFromEnv() (*SetupConfig, error) {
	if err := config.CheckRedisClusterUnsupported(nil, os.Environ()); err != nil {
		return nil, err
	}

	// Get timezone from TZ or TIMEZONE env var (TZ is standard for Docker)
	tz := getEnvOrDefault("TZ", "")
	if tz == "" {
		tz = getEnvOrDefault("TIMEZONE", "Asia/Shanghai")
	}

	cfg := &SetupConfig{
		Database: DatabaseConfig{
			Host:     getEnvOrDefault("DATABASE_HOST", "localhost"),
			Port:     getEnvIntOrDefault("DATABASE_PORT", 5432),
			User:     getEnvOrDefault("DATABASE_USER", "postgres"),
			Password: getEnvOrDefault("DATABASE_PASSWORD", ""),
			DBName:   getEnvOrDefault("DATABASE_DBNAME", "sub2api"),
			SSLMode:  getEnvOrDefault("DATABASE_SSLMODE", "disable"),
		},
		Redis: RedisConfig{
			Host:             getEnvOrDefault("REDIS_HOST", "localhost"),
			Port:             getEnvIntOrDefault("REDIS_PORT", 6379),
			Username:         getEnvOrDefault("REDIS_USERNAME", ""),
			Password:         getEnvOrDefault("REDIS_PASSWORD", ""),
			DB:               getEnvIntOrDefault("REDIS_DB", 0),
			EnableTLS:        getEnvOrDefault("REDIS_ENABLE_TLS", "false") == "true",
			TLSServerName:    getEnvOrDefault("REDIS_TLS_SERVER_NAME", ""),
			SentinelAddrs:    []string{getEnvOrDefault("REDIS_SENTINEL_ADDRS", "")},
			MasterName:       getEnvOrDefault("REDIS_MASTER_NAME", ""),
			SentinelUsername: getEnvOrDefault("REDIS_SENTINEL_USERNAME", ""),
			SentinelPassword: getEnvOrDefault("REDIS_SENTINEL_PASSWORD", ""),
		},
		Admin: AdminConfig{
			Email:    getEnvOrDefault("ADMIN_EMAIL", "admin@sub2api.local"),
			Password: getEnvOrDefault("ADMIN_PASSWORD", ""),
		},
		Server: ServerConfig{
			Host: getEnvOrDefault("SERVER_HOST", "0.0.0.0"),
			Port: getEnvIntOrDefault("SERVER_PORT", 8080),
			Mode: getEnvOrDefault("SERVER_MODE", "release"),
		},
		JWT: JWTConfig{
			Secret:     getEnvOrDefault("JWT_SECRET", ""),
			ExpireHour: getEnvIntOrDefault("JWT_EXPIRE_HOUR", 24),
		},
		Totp:                    TotpConfig{EncryptionKey: getEnvOrDefault("TOTP_ENCRYPTION_KEY", "")},
		Timezone:                tz,
		MigrationTimeoutSeconds: getEnvIntOrDefault("SETUP_MIGRATION_TIMEOUT_SECONDS", 0),
	}

	if err := cfg.Database.resolve(os.Getenv("DATABASE_URL"), tz); err != nil {
		return nil, err
	}
	if err := cfg.Redis.resolve(os.Getenv("REDIS_URL")); err != nil {
		return nil, err
	}
	return cfg, nil
}

// AutoSetupFromEnv performs automatic setup using environment variables
// This is designed for Docker deployment where all config is passed via env vars
func AutoSetupFromEnv() error {
	logger.LegacyPrintf("setup", "%s", "Auto setup enabled, configuring from environment variables...")
	logger.LegacyPrintf("setup", "Data directory: %s", GetDataDir())

	cfg, err := setupConfigFromEnv()
	if err != nil {
		return fmt.Errorf("invalid environment configuration: %w", err)
	}

	// Generate JWT secret if not provided
	if cfg.JWT.Secret == "" {
		secret, err := generateSecret(32)
		if err != nil {
			return fmt.Errorf("failed to generate jwt secret: %w", err)
		}
		cfg.JWT.Secret = secret
		logger.LegacyPrintf("setup", "%s", "Warning: JWT secret auto-generated. Consider setting a fixed secret for production.")
	}
	if err := ensureEncryptionKey(cfg); err != nil {
		return err
	}

	// Test database connection
	logger.LegacyPrintf("setup", "%s", "Testing database connection...")
	if err := TestDatabaseConnection(&cfg.Database); err != nil {
		return fmt.Errorf("database connection failed: %w", err)
	}
	logger.LegacyPrintf("setup", "%s", "Database connection successful")

	// Test Redis connection
	logger.LegacyPrintf("setup", "%s", "Testing Redis connection...")
	if err := TestRedisConnection(&cfg.Redis); err != nil {
		return fmt.Errorf("redis connection failed: %w", err)
	}
	logger.LegacyPrintf("setup", "%s", "Redis connection successful")

	// Initialize database
	logger.LegacyPrintf("setup", "%s", "Initializing database...")
	if err := initializeDatabase(cfg); err != nil {
		return fmt.Errorf("database initialization failed: %w", err)
	}
	logger.LegacyPrintf("setup", "%s", "Database initialized successfully")

	// Create admin user
	logger.LegacyPrintf("setup", "%s", "Creating admin user...")
	created, reason, err := createAdminUser(cfg)
	if err != nil {
		return fmt.Errorf("admin user creation failed: %w", err)
	}
	if created {
		logger.LegacyPrintf("setup", "Admin user created: %s", cfg.Admin.Email)
	} else {
		switch reason {
		case adminBootstrapReasonAdminExists:
			logger.LegacyPrintf("setup", "%s", "Admin user already exists, skipping admin bootstrap")
		case adminBootstrapReasonUsersExistWithoutAdmin:
			logger.LegacyPrintf("setup", "%s", "Database already has user data; skipping auto admin bootstrap to avoid password overwrite")
		default:
			logger.LegacyPrintf("setup", "%s", "Admin bootstrap skipped")
		}
	}

	// Write config file
	logger.LegacyPrintf("setup", "%s", "Writing configuration file...")
	if err := writeConfigFile(cfg); err != nil {
		return fmt.Errorf("config file creation failed: %w", err)
	}
	logger.LegacyPrintf("setup", "%s", "Configuration file created")

	// Create installation lock file
	if err := createInstallLock(); err != nil {
		return fmt.Errorf("failed to create install lock: %w", err)
	}
	logger.LegacyPrintf("setup", "%s", "Installation lock created")

	logger.LegacyPrintf("setup", "%s", "Auto setup completed successfully!")
	return nil
}
