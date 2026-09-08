// Package readiness 实现依赖感知的就绪探针（GET /readyz）。
//
// 与 /health（liveness）的分工：liveness 只回答「进程活着吗」，永远是 200；readiness 回答
// 「这个实例现在能不能服务」，任一依赖不可达就返回 503，让编排器把实例摘出负载均衡而
// 不是重启它。两者必须独立失败，否则数据库抖动会演变成全副本 crash loop。
package readiness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	// StatusReady / StatusNotReady 是响应体 status 字段的两个取值。
	StatusReady    = "ready"
	StatusNotReady = "not_ready"

	// resultOK 是单项检查通过时的取值；失败时为 "error: <原因>"。
	resultOK          = "ok"
	resultErrorPrefix = "error: "
)

// Check 是一个具名的依赖探测：Probe 在给定 ctx 内返回 nil 表示依赖可用。
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// DependencyChecks 是「就绪需要哪些依赖」的唯一声明处。
//
// 新增一个依赖只在这里加一项：并发执行、独立超时、响应体字段、状态翻转日志和遍历式
// 测试都会自动覆盖它。
func DependencyChecks(db *sql.DB, rdb *redis.Client) []Check {
	return []Check{
		{Name: "postgres", Probe: pingPostgres(db)},
		{Name: "redis", Probe: pingRedis(rdb)},
	}
}

func pingPostgres(db *sql.DB) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if db == nil {
			return ErrNotConfigured
		}
		return db.PingContext(ctx)
	}
}

func pingRedis(rdb *redis.Client) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if rdb == nil {
			return ErrNotConfigured
		}
		return rdb.Ping(ctx).Err()
	}
}

// Report 是一次就绪评估的结果。
type Report struct {
	Ready bool
	// Checks 以检查名为键，值为 "ok" 或 "error: <原因>"。
	Checks map[string]string
}

// Checker 并发执行一组具名检查，每项检查拥有独立的超时。
type Checker struct {
	timeout time.Duration
	checks  []Check

	// failing 记录每项检查上一次的结果，只在状态翻转时写日志：编排器每几秒探测一次，
	// 故障期间逐次记录只会刷屏，翻转点才是运维需要的信息。
	mu      sync.Mutex
	failing map[string]bool
}

// NewChecker 构造 Checker。timeout 是单项检查的上限，checks 通常来自 DependencyChecks。
func NewChecker(timeout time.Duration, checks ...Check) *Checker {
	return &Checker{
		timeout: timeout,
		checks:  checks,
		failing: make(map[string]bool, len(checks)),
	}
}

// Names 按声明顺序返回全部检查名，供响应消费方与遍历式测试使用。
func (c *Checker) Names() []string {
	names := make([]string, len(c.checks))
	for i, chk := range c.checks {
		names[i] = chk.Name
	}
	return names
}

// Run 并发执行全部检查并汇总。
func (c *Checker) Run(ctx context.Context) Report {
	results := make([]string, len(c.checks))
	var wg sync.WaitGroup
	for i, chk := range c.checks {
		wg.Add(1)
		go func(i int, chk Check) {
			defer wg.Done()
			results[i] = c.runOne(ctx, chk)
		}(i, chk)
	}
	wg.Wait()

	report := Report{Ready: true, Checks: make(map[string]string, len(c.checks))}
	for i, chk := range c.checks {
		report.Checks[chk.Name] = results[i]
		if results[i] != resultOK {
			report.Ready = false
		}
	}
	return report
}

// runOne 在独立超时内执行单项检查。超时由 Checker 自己用 select 兜底，而不是信任
// Probe 会遵守 ctx：一个不看 ctx 的探测最多让我们等 timeout，而不是无限期挂住探针。
func (c *Checker) runOne(parent context.Context, chk Check) string {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- chk.Probe(ctx) }()

	var err error
	select {
	case err = <-done:
		// 探测在超时之后才带着错误返回（驱动把 ctx 取消翻译成了自己的错误）：按超时归因，
		// 否则同一种故障会随 select 的随机选择报出两种不同的文案。
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	case <-ctx.Done():
		err = ctx.Err()
	}

	result := resultOK
	detail := ""
	if err != nil {
		result = resultErrorPrefix + describeFailure(err, c.timeout)
		detail = describeFailureForLog(err, c.timeout)
	}
	c.recordTransition(chk.Name, result, detail)
	return result
}

// recordTransition 只在 ok<->failed 翻转时记录日志。detail 是不对外暴露的错误原文
// （已去凭据）：响应体只说 unavailable，排障需要的具体原因在这里。
func (c *Checker) recordTransition(name, result, detail string) {
	failed := result != resultOK

	c.mu.Lock()
	wasFailing := c.failing[name]
	c.failing[name] = failed
	c.mu.Unlock()

	switch {
	case failed && !wasFailing:
		slog.Warn("readiness: dependency check failed; instance reports not ready until it recovers",
			"check", name, "result", result, "detail", detail)
	case !failed && wasFailing:
		slog.Info("readiness: dependency check recovered", "check", name)
	}
}

// ErrNotConfigured 是"这个依赖压根没配"的哨兵错误：它不是运行期故障，也不含任何
// 内部地址，因此可以原样对外暴露——探针据此能区分"配错了"和"连不上"。
var ErrNotConfigured = errors.New("not configured")

// describeFailure 生成对外暴露的失败文本。
//
// 只区分"超时"和"不可达"两种，不透出驱动原文：/readyz 是匿名可读的，
// 而驱动错误里带着内网地址与端口（dial tcp 10.0.0.5:5432: connect: connection refused）。
// 探针需要知道的只是"这个依赖此刻不健康"，具体原因给运维看日志（logFailure 打全文，
// 经 sanitizeError 去掉凭据）。
func describeFailure(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timeout after %s", timeout)
	case errors.Is(err, ErrNotConfigured):
		return ErrNotConfigured.Error()
	default:
		return "unavailable"
	}
}

// describeFailureForLog 是给日志用的版本：保留原文，只去掉凭据。
func describeFailureForLog(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("timeout after %s", timeout)
	}
	return sanitizeError(err.Error())
}

// reURLUserinfo 匹配 scheme://user:password@ 形式的凭据段。
var reURLUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s@]+@`)

// sanitizeError 去掉错误文本里可能出现的凭据：DSN 里的 user:password@、
// key=value 形式的 password/token 等。
func sanitizeError(msg string) string {
	msg = reURLUserinfo.ReplaceAllString(msg, "${1}***@")
	return logredact.RedactText(msg)
}

// response 是 /readyz 的响应体。
type response struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Handler 返回 /readyz 的 gin handler：200 ready / 503 not_ready，禁止缓存。
func (c *Checker) Handler() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		report := c.Run(ctx.Request.Context())

		ctx.Header("Cache-Control", "no-store")
		status := http.StatusOK
		body := response{Status: StatusReady, Checks: report.Checks}
		if !report.Ready {
			status = http.StatusServiceUnavailable
			body.Status = StatusNotReady
		}
		ctx.JSON(status, body)
	}
}
