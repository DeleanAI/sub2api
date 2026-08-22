//go:build unit

package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/probe"
	"github.com/Wei-Shaw/sub2api/internal/server/readiness"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCommonRoutesRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 没有任何依赖：liveness 仍然必须 200，readiness 必须 503。
	RegisterCommonRoutes(r, readiness.NewChecker(100*time.Millisecond, readiness.DependencyChecks(nil, nil)...))
	return r
}

// 每一个声明的探针路径都必须真的被路由到，而不是掉进 404。
func TestCommonRoutesServeEveryDeclaredProbe(t *testing.T) {
	r := newCommonRoutesRouter(t)
	for _, path := range probe.Paths() {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.NotEqual(t, http.StatusNotFound, rec.Code, "probe path %s is declared but not routed", path)
		require.Contains(t, rec.Header().Get("Content-Type"), "application/json", "probe path %s", path)
	}
}

// liveness 契约：不依赖任何外部组件，响应体保持 {"status":"ok"}。
func TestLivenessStaysDependencyFree(t *testing.T) {
	r := newCommonRoutesRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, probe.LivenessPath, nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"status":"ok"}`, rec.Body.String())
}

// readiness 契约：依赖不可用时 503，且每个声明的检查都出现在响应里。
func TestReadinessReflectsDependencies(t *testing.T) {
	r := newCommonRoutesRouter(t)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, probe.ReadinessPath, nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, readiness.StatusNotReady, body.Status)
	for _, name := range readiness.NewChecker(time.Second, readiness.DependencyChecks(nil, nil)...).Names() {
		require.Contains(t, body.Checks, name)
	}
}
