//go:build unit

package readiness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const testTimeout = 200 * time.Millisecond

// dependencyFixture 用 sqlmock + miniredis 搭出真实的 *sql.DB / *redis.Client，
// 让 DependencyChecks 走和生产完全相同的代码路径。
type dependencyFixture struct {
	db   *sql.DB
	mock sqlmock.Sqlmock
	mr   *miniredis.Miniredis
	rdb  *redis.Client
}

func newDependencyFixture(t *testing.T) *dependencyFixture {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	return &dependencyFixture{db: db, mock: mock, mr: mr, rdb: rdb}
}

// arrange 按检查名声明「如何让这个依赖健康 / 故障」。测试遍历 Checker.Names()，声明
// 了检查却没有对应 arrange 的会直接失败：新增依赖的同时必须决定它的故障如何模拟。
func (f *dependencyFixture) arrange() map[string]func(t *testing.T, healthy bool) {
	return map[string]func(t *testing.T, healthy bool){
		"postgres": func(t *testing.T, healthy bool) {
			if healthy {
				f.mock.ExpectPing()
				return
			}
			f.mock.ExpectPing().WillReturnError(errors.New("dial tcp 10.0.0.5:5432: connect: connection refused"))
		},
		"redis": func(t *testing.T, healthy bool) {
			if healthy {
				f.mr.SetError("")
				return
			}
			f.mr.SetError("ERR redis is down")
		},
	}
}

func (f *dependencyFixture) checker() *Checker {
	return NewChecker(testTimeout, DependencyChecks(f.db, f.rdb)...)
}

func (f *dependencyFixture) arrangeAll(t *testing.T, c *Checker, broken map[string]bool) {
	t.Helper()
	arrange := f.arrange()
	for _, name := range c.Names() {
		setup, ok := arrange[name]
		require.True(t, ok, "declared check %q has no test arrangement; add one to dependencyFixture.arrange", name)
		setup(t, !broken[name])
	}
}

type readyzBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func serveReadyz(t *testing.T, c *Checker) (*httptest.ResponseRecorder, readyzBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/readyz", c.Handler())

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	var body readyzBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body=%s", rec.Body.String())
	return rec, body
}

func TestReadyzReportsReadyWhenEveryDependencyAnswers(t *testing.T) {
	f := newDependencyFixture(t)
	c := f.checker()
	f.arrangeAll(t, c, nil)

	rec, body := serveReadyz(t, c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, StatusReady, body.Status)
	require.Len(t, body.Checks, len(c.Names()))
	for _, name := range c.Names() {
		require.Equal(t, "ok", body.Checks[name], "check %q", name)
	}
	require.NoError(t, f.mock.ExpectationsWereMet())
}

// 任意一个依赖失败都必须让整个实例变为 not_ready，且其它依赖仍如实报告 ok。
func TestReadyzReportsNotReadyWhenAnySingleDependencyFails(t *testing.T) {
	names := newDependencyFixture(t).checker().Names()
	require.NotEmpty(t, names)

	for _, failing := range names {
		t.Run(failing+"_down", func(t *testing.T) {
			f := newDependencyFixture(t)
			c := f.checker()
			f.arrangeAll(t, c, map[string]bool{failing: true})

			rec, body := serveReadyz(t, c)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			require.Equal(t, StatusNotReady, body.Status)
			for _, name := range c.Names() {
				if name == failing {
					require.True(t, strings.HasPrefix(body.Checks[name], "error: "), "check %q = %q", name, body.Checks[name])
					continue
				}
				require.Equal(t, "ok", body.Checks[name], "healthy check %q must still report ok", name)
			}
		})
	}
}

func TestReadyzReportsEveryFailureWhenAllDependenciesAreDown(t *testing.T) {
	f := newDependencyFixture(t)
	c := f.checker()
	broken := make(map[string]bool, len(c.Names()))
	for _, name := range c.Names() {
		broken[name] = true
	}
	f.arrangeAll(t, c, broken)

	rec, body := serveReadyz(t, c)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, StatusNotReady, body.Status)
	for _, name := range c.Names() {
		require.True(t, strings.HasPrefix(body.Checks[name], "error: "), "check %q = %q", name, body.Checks[name])
	}
}

func TestReadyzNotReadyWhenDependenciesAreNotConfigured(t *testing.T) {
	c := NewChecker(testTimeout, DependencyChecks(nil, nil)...)

	rec, body := serveReadyz(t, c)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	for _, name := range c.Names() {
		require.Equal(t, "error: not configured", body.Checks[name], "check %q", name)
	}
}

// 真实依赖挂住（而不是报错）时，每项检查各自在超时内返回，并按超时归因。
func TestReadyzTimesOutHungDependencies(t *testing.T) {
	f := newDependencyFixture(t)
	// PostgreSQL：ping 的回复晚于超时。
	f.mock.ExpectPing().WillDelayFor(5 * testTimeout)

	// Redis：一个只接受连接、永远不回包的监听器。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
		}
	}()
	hung := redis.NewClient(&redis.Options{Addr: ln.Addr().String(), ReadTimeout: 10 * testTimeout, MaxRetries: -1})
	t.Cleanup(func() { _ = hung.Close() })

	c := NewChecker(testTimeout, DependencyChecks(f.db, hung)...)
	start := time.Now()
	rec, body := serveReadyz(t, c)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, StatusNotReady, body.Status)
	want := "error: timeout after " + testTimeout.String()
	for _, name := range c.Names() {
		require.Equal(t, want, body.Checks[name], "check %q", name)
	}
	// 两项检查并发执行：总耗时只能是一个超时，而不是两个叠加。
	require.Less(t, elapsed, 2*testTimeout, "checks must run concurrently, elapsed=%s", elapsed)
}

// 超时由 Checker 自己兜底：一个完全不看 ctx 的探测也只能拖住一个超时周期。
func TestCheckerBoundsProbesThatIgnoreContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := NewChecker(testTimeout, Check{Name: "stubborn", Probe: func(context.Context) error {
		<-release
		return nil
	}})

	report := c.Run(context.Background())

	require.False(t, report.Ready)
	require.Equal(t, "error: timeout after "+testTimeout.String(), report.Checks["stubborn"])
}

// 并发性的结构性证明：两项检查互相等待对方开始，串行执行时第一项会等到超时。
func TestCheckerRunsChecksConcurrently(t *testing.T) {
	var both sync.WaitGroup
	both.Add(2)
	meet := func(context.Context) error {
		both.Done()
		both.Wait()
		return nil
	}
	c := NewChecker(testTimeout, Check{Name: "a", Probe: meet}, Check{Name: "b", Probe: meet})

	report := c.Run(context.Background())

	require.True(t, report.Ready, "checks=%v", report.Checks)
}

// TestDescribeFailureExposesABoundedVocabulary 钉死 /readyz 对外只说有限的几种状态。
//
// /readyz 是匿名可读的，而驱动错误里带着内网地址与端口
// （dial tcp 10.0.0.5:5432: connect: connection refused）。探针需要知道的只是
// "这个依赖此刻不健康"；具体原因给运维看日志。之前这些原文是直接透出去的，
// 而且当时的测试还专门断言它原样通过。
func TestDescribeFailureExposesABoundedVocabulary(t *testing.T) {
	leaky := []string{
		"connect postgres://sub2api:s3cret@db.internal:5432/app: refused",
		"dial redis://:hunter2@cache:6379: timeout",
		"pq: password=hunter2 host=db user=app",
		"dial tcp 10.0.0.5:5432: connect: connection refused",
	}
	for _, in := range leaky {
		out := describeFailure(errors.New(in), testTimeout)
		require.Equal(t, "unavailable", out, "input %q", in)
		for _, secret := range []string{"s3cret", "hunter2", "10.0.0.5", "db.internal", "5432", "cache"} {
			require.NotContainsf(t, out, secret, "对外文本泄露了 %q（来自 %q）", secret, in)
		}
	}
	require.Equal(t, "timeout after "+testTimeout.String(), describeFailure(context.DeadlineExceeded, testTimeout))
	// "没配"不是运行期故障，也不含任何内部地址，保留它对排障有用。
	require.Equal(t, "not configured", describeFailure(ErrNotConfigured, testTimeout))
	require.Equal(t, "not configured", describeFailure(fmt.Errorf("redis: %w", ErrNotConfigured), testTimeout))
}

// 日志侧仍保留原文，只去掉凭据——排障要看的就是这些。
func TestDescribeFailureForLogHidesCredentialsButKeepsDetail(t *testing.T) {
	cases := map[string]string{
		"connect postgres://sub2api:s3cret@db.internal:5432/app: refused": "connect postgres://***@db.internal:5432/app: refused",
		"dial redis://:hunter2@cache:6379: timeout":                       "dial redis://***@cache:6379: timeout",
		"pq: password=hunter2 host=db user=app":                           "pq: password=*** host=db user=app",
		"dial tcp 10.0.0.5:5432: connect: connection refused":             "dial tcp 10.0.0.5:5432: connect: connection refused",
	}
	for in, want := range cases {
		require.Equal(t, want, describeFailureForLog(errors.New(in), testTimeout), "input %q", in)
	}
	require.Equal(t, "timeout after "+testTimeout.String(), describeFailureForLog(context.DeadlineExceeded, testTimeout))
}

// 状态翻转才记日志的前提是结果被正确记账：这里只验证记账，不依赖日志输出。
func TestCheckerTracksFailureTransitions(t *testing.T) {
	var fail bool
	c := NewChecker(testTimeout, Check{Name: "flaky", Probe: func(context.Context) error {
		if fail {
			return errors.New("boom")
		}
		return nil
	}})

	require.True(t, c.Run(context.Background()).Ready)
	fail = true
	require.False(t, c.Run(context.Background()).Ready)
	require.True(t, c.failing["flaky"])
	fail = false
	require.True(t, c.Run(context.Background()).Ready)
	require.False(t, c.failing["flaky"])
}
