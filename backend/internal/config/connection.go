package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// 连接目标（PostgreSQL / Redis）的解析规则只写在这个文件里。主配置加载（load）和容器自动安装
// （setup.AutoSetupFromEnv）各自从不同的地方拿到原始值，但都交给这里落成最终字段：
//   - database.url / redis.url 非空时就是连接目标的唯一来源，离散字段被覆盖，并在启动日志里说明；
//   - URL 之外的 libpq 参数（如 sslrootcert）进入 DatabaseConfig.ExtraParams，由 DSN 渲染统一带上。

// envKeyReplacer 是 viper 键 → 环境变量名的唯一映射规则：load() 用它注册 SetEnvKeyReplacer，
// EnvName 用它反推文档/安装流程里的变量名，两边不会各自维护一套拼写。
var envKeyReplacer = strings.NewReplacer(".", "_")

// EnvName 返回 viper 键对应的环境变量名，例如 "redis.sentinel_addrs" → "REDIS_SENTINEL_ADDRS"。
func EnvName(key string) string {
	return strings.ToUpper(envKeyReplacer.Replace(key))
}

// redactURLError 去掉 url.Error 里的完整 URL：它会把 user:password@ 一起拼进错误文本，
// 而这个错误最终会进启动日志。
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

// databaseURLReservedParams 必须写在 URL 主体（user:password@host:port/dbname），不能再以查询参数出现，
// 否则同一个值有两个来源，优先级说不清。
var databaseURLReservedParams = []string{"host", "hostaddr", "port", "user", "password", "dbname"}

// databaseExtraParams 是 database.url 查询串 / database.extra_params 允许的参数名（小写 → 写入 DSN 的写法）。
// 范围是 lib/pq 自己消费的驱动参数（lib/pq conn.go isDriverSetting）加上它会原样转发给服务器、
// 连接串里常见的会话参数。名字不在表内直接报错：lib/pq 会把任何未知键当作会话参数发给服务器，
// 所以 sslmod=disable 这类拼写错误不会在客户端被发现，只会在很远的地方表现成"怎么没走 TLS"。
// sslmode 不在这里：它是 DatabaseConfig 的一等字段，由 ApplyDatabaseURL 直接赋值。
var databaseExtraParams = map[string]string{
	"sslcert":                             "sslcert",
	"sslkey":                              "sslkey",
	"sslrootcert":                         "sslrootcert",
	"sslinline":                           "sslinline",
	"sslsni":                              "sslsni",
	"connect_timeout":                     "connect_timeout",
	"application_name":                    "application_name",
	"fallback_application_name":           "fallback_application_name",
	"client_encoding":                     "client_encoding",
	"datestyle":                           "datestyle",
	"options":                             "options",
	"krbsrvname":                          "krbsrvname",
	"krbspn":                              "krbspn",
	"disable_prepared_binary_result":      "disable_prepared_binary_result",
	"binary_parameters":                   "binary_parameters",
	"timezone":                            "TimeZone",
	"search_path":                         "search_path",
	"statement_timeout":                   "statement_timeout",
	"lock_timeout":                        "lock_timeout",
	"idle_in_transaction_session_timeout": "idle_in_transaction_session_timeout",
}

// databaseSSLModes 是 libpq 定义的 sslmode 取值（PostgreSQL 文档 §34.1.2）。
var databaseSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// databaseExtraParamNames 按字母序列出允许的额外参数，用于报错提示。
func databaseExtraParamNames() []string {
	names := make([]string, 0, len(databaseExtraParams))
	for _, canonical := range databaseExtraParams {
		names = append(names, canonical)
	}
	sort.Strings(names)
	return names
}

// normalizeDatabaseExtraParams 把参数名归一到表内写法，未知名字或保留名字报错并指名道姓。
// URL 查询串与 config.yaml 的 extra_params 都经过这里，所以"哪些名字合法"只有一张表。
func normalizeDatabaseExtraParams(params map[string]string) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	normalized := make(map[string]string, len(params))
	for name, value := range params {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "sslmode" || slices.Contains(databaseURLReservedParams, lower) {
			return nil, fmt.Errorf("%q must be set through its own field (host/port/user/password/dbname/sslmode), not as an extra parameter", name)
		}
		canonical, ok := databaseExtraParams[lower]
		if !ok {
			return nil, fmt.Errorf("unknown parameter %q (accepted: %s)", name, strings.Join(databaseExtraParamNames(), ", "))
		}
		if _, dup := normalized[canonical]; dup {
			return nil, fmt.Errorf("parameter %q given more than once", canonical)
		}
		normalized[canonical] = value
	}
	return normalized, nil
}

// ApplyDatabaseURL 把 postgres://user:pass@host:port/dbname?sslmode=...&sslrootcert=... 落到 dst 的离散字段。
// URL 是连接目标的唯一来源：它没写的部分取 URL 语义下的默认值（端口 5432、无鉴权、sslmode 交给驱动默认），
// 而不是悄悄沿用离散字段——否则"我明明改了 URL 为什么还连着旧库"没人能解释。
// raw 为空表示没有 URL，dst 原样不动。
func ApplyDatabaseURL(dst *DatabaseConfig, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("database.url: %w", redactURLError(err))
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
	default:
		return fmt.Errorf("database.url: scheme must be postgres:// or postgresql://, got %q", u.Scheme)
	}
	host := u.Hostname()
	if u.Opaque != "" || host == "" {
		return fmt.Errorf("database.url: host is required (postgres://user:password@host:port/dbname)")
	}
	port := 5432
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("database.url: invalid port %q", p)
		}
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" || strings.Contains(dbName, "/") {
		return fmt.Errorf("database.url: path must be the database name (/dbname), got %q", u.Path)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("database.url: invalid query string: %w", err)
	}
	sslMode := ""
	extras := make(map[string]string, len(query))
	for name, values := range query {
		if len(values) != 1 {
			return fmt.Errorf("database.url: parameter %q given %d times", name, len(values))
		}
		lower := strings.ToLower(name)
		if slices.Contains(databaseURLReservedParams, lower) {
			return fmt.Errorf("database.url: %q must be written in the URL itself (user:password@host:port/dbname), not as a query parameter", name)
		}
		if lower == "sslmode" {
			sslMode = values[0]
			continue
		}
		extras[name] = values[0]
	}
	extras, err = normalizeDatabaseExtraParams(extras)
	if err != nil {
		return fmt.Errorf("database.url: %w", err)
	}
	user, password := "", ""
	if u.User != nil {
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	dst.Host = host
	dst.Port = port
	dst.User = user
	dst.Password = password
	dst.DBName = dbName
	dst.SSLMode = sslMode
	dst.ExtraParams = extras
	return nil
}

// Resolve 是数据库连接配置的收口：应用 database.url 的优先级、归一额外参数、统一会话时区来源，
// 最后做连接字段校验。load() 与 setup.AutoSetupFromEnv 都必须调用它。
//
// 会话时区：timezone（TZ）是整个应用"今天从几点开始"的依据，数据库会话必须与之一致，
// 所以 DSN 里的 TimeZone 永远由应用时区生成。URL/extra_params 里再写一个 TimeZone，
// 相同是多余、不同是冲突，后者直接报错而不是让其中一个悄悄赢。
func (d *DatabaseConfig) Resolve(appTimezone string) error {
	urlUsed := strings.TrimSpace(d.URL) != ""
	if err := ApplyDatabaseURL(d, d.URL); err != nil {
		return err
	}
	extras, err := normalizeDatabaseExtraParams(d.ExtraParams)
	if err != nil {
		return fmt.Errorf("database.extra_params: %w", err)
	}
	if tz, ok := extras["TimeZone"]; ok {
		if appTimezone != "" && !strings.EqualFold(strings.TrimSpace(tz), strings.TrimSpace(appTimezone)) {
			return fmt.Errorf("database: TimeZone=%q in the connection parameters conflicts with timezone=%q (TZ); the application timezone governs the database session, remove TimeZone from the URL", tz, appTimezone)
		}
		delete(extras, "TimeZone")
		if len(extras) == 0 {
			extras = nil
		}
	}
	d.ExtraParams = extras
	if urlUsed {
		slog.Info("database connection configured from database.url; discrete database.host/port/user/password/dbname/sslmode are ignored",
			"host", d.Host,
			"port", d.Port,
			"user", d.User,
			"dbname", d.DBName,
			"sslmode", d.sslModeForLog(),
			"extra_params", sortedKeys(d.ExtraParams),
		)
	}
	return d.ValidateConnection()
}

func (d *DatabaseConfig) sslModeForLog() string {
	if d.SSLMode == "" {
		return "(driver default)"
	}
	return d.SSLMode
}

// ValidateConnection 校验连接字段（与连接池参数无关），Config.Validate 与安装流程共用。
func (d *DatabaseConfig) ValidateConnection() error {
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("database.port must be between 1 and 65535")
	}
	if d.SSLMode != "" && !slices.Contains(databaseSSLModes, d.SSLMode) {
		return fmt.Errorf("database.sslmode must be one of: %s", strings.Join(databaseSSLModes, ", "))
	}
	if _, err := normalizeDatabaseExtraParams(d.ExtraParams); err != nil {
		return fmt.Errorf("database.extra_params: %w", err)
	}
	return nil
}

// dsnParams 渲染 libpq 的 key=value 列表：固定字段在前、额外参数按名排序在后，输出是确定的。
// 空值省略，交给 libpq 自己的默认值（和以前"密码为空就不写 password="是同一条规则，推广到所有字段）。
func (d *DatabaseConfig) dsnParams(dbName string) []string {
	params := make([]string, 0, 7+len(d.ExtraParams))
	add := func(key, value string) {
		if value == "" {
			return
		}
		params = append(params, key+"="+quoteDSNValue(value))
	}
	add("host", d.Host)
	if d.Port > 0 {
		add("port", strconv.Itoa(d.Port))
	}
	add("user", d.User)
	add("password", d.Password)
	add("dbname", dbName)
	add("sslmode", d.SSLMode)
	for _, key := range sortedKeys(d.ExtraParams) {
		add(key, d.ExtraParams[key])
	}
	return params
}

// DSN 返回 lib/pq 的 key=value 连接串。
func (d *DatabaseConfig) DSN() string {
	return strings.Join(d.dsnParams(d.DBName), " ")
}

// DSNForDatabase 用同一套连接参数连到另一个库——安装阶段要先连 postgres 维护库才能创建目标库。
func (d *DatabaseConfig) DSNForDatabase(dbName string) string {
	return strings.Join(d.dsnParams(dbName), " ")
}

// DSNWithTimezone 在 DSN 末尾带上会话时区；tz 为空时用应用默认时区。
func (d *DatabaseConfig) DSNWithTimezone(tz string) string {
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	return strings.Join(append(d.dsnParams(d.DBName), "TimeZone="+quoteDSNValue(tz)), " ")
}

// quoteDSNValue 按 libpq 连接串规则引用值：含空白、单引号或反斜杠时用单引号包裹并转义，其余原样。
// URL 解码后的密码很容易带这些字符，不引用就会被解析成下一个 key。
func quoteDSNValue(value string) string {
	if !strings.ContainsAny(value, " \t\r\n'\\") {
		return value
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Redis
// ---------------------------------------------------------------------------

// ApplyRedisURL 把 redis://user:pass@host:port/db 或 rediss://... 落到 dst 的离散字段。
// URL 只描述"连哪里、怎么鉴权、哪个库、是否 TLS"；连接池与超时仍由离散的 redis.* 配置控制，
// 所以除 db 外的查询参数一律拒绝——go-redis 的 ParseURL 会接受 pool_size 之类的参数，
// 放进来就会出现同一项配置有两个来源。
func ApplyRedisURL(dst *RedisConfig, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redis.url: %w", redactURLError(err))
	}
	switch u.Scheme {
	case "redis", "rediss":
	default:
		return fmt.Errorf("redis.url: scheme must be redis:// or rediss://, got %q", u.Scheme)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("redis.url: invalid query string: %w", err)
	}
	for name := range query {
		if name != "db" {
			return fmt.Errorf("redis.url: query parameter %q is not supported; the URL carries scheme, credentials, host, port and database index only, tune the client with the discrete redis.* settings", name)
		}
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		return fmt.Errorf("redis.url: %w", redactURLError(err))
	}
	host, portText, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return fmt.Errorf("redis.url: invalid address %q: %w", opts.Addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("redis.url: invalid port %q", portText)
	}
	dst.Host = host
	dst.Port = port
	dst.Username = opts.Username
	dst.Password = opts.Password
	dst.DB = opts.DB
	dst.EnableTLS = opts.TLSConfig != nil // rediss://
	return nil
}

// Resolve 是 Redis 连接配置的收口：应用 redis.url 的优先级，最后做连接字段校验。
// load() 与 setup.AutoSetupFromEnv 都必须调用它。
func (r *RedisConfig) Resolve() error {
	urlUsed := strings.TrimSpace(r.URL) != ""
	if err := ApplyRedisURL(r, r.URL); err != nil {
		return err
	}
	r.Host = strings.TrimSpace(r.Host)
	if urlUsed {
		slog.Info("redis connection configured from redis.url; discrete redis.host/port/username/password/db/enable_tls are ignored",
			"addr", r.Address(),
			"db", r.DB,
			"tls", r.EnableTLS,
		)
	}
	return r.ValidateConnection()
}

// ValidateConnection 校验连接字段（与连接池参数无关），Config.Validate 与安装流程共用。
func (r *RedisConfig) ValidateConnection() error {
	if r.Host == "" {
		return fmt.Errorf("redis.host is required unless redis.url is set")
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("redis.port must be between 1 and 65535")
	}
	if r.DB < 0 {
		return fmt.Errorf("redis.db must be non-negative")
	}
	return nil
}
