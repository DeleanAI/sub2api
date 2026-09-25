//go:build unit

package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/probe"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const spaFallbackTestPage = "<!doctype html>spa"

func spaFallbackTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(SPAFallback(router, func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(spaFallbackTestPage))
		c.Abort()
	}))
	return router
}

// 四种结局：路由表认领 → 交给路由；路径属于路由表但方法不对 → 404；API 命名空间里的未知路径 → 404；其余 → 页面。
func TestSPAFallbackDecidesByRouteTableThenReservedNamespaces(t *testing.T) {
	router := spaFallbackTestRouter()
	reached := ""
	handler := func(c *gin.Context) {
		reached = c.FullPath()
		c.Status(http.StatusNoContent)
	}
	router.POST("/chat/completions", handler)
	router.GET("/custom-voices/:voice_id", handler)
	router.POST("/responses/*subpath", handler)

	cases := []struct {
		method, path, wantReached string
		wantStatus                int
		wantPage                  bool
	}{
		{method: http.MethodPost, path: "/chat/completions", wantReached: "/chat/completions", wantStatus: http.StatusNoContent},
		{method: http.MethodGet, path: "/custom-voices/v1", wantReached: "/custom-voices/:voice_id", wantStatus: http.StatusNoContent},
		{method: http.MethodPost, path: "/responses/compact", wantReached: "/responses/*subpath", wantStatus: http.StatusNoContent},
		// 路径属于路由表、方法不对：404，不是页面。
		{method: http.MethodGet, path: "/chat/completions", wantStatus: http.StatusNotFound},
		{method: http.MethodDelete, path: "/custom-voices/v1", wantStatus: http.StatusNotFound},
		{method: http.MethodGet, path: "/responses/compact/deep", wantStatus: http.StatusNotFound},
		// 命名空间里没被认领的：404。
		{method: http.MethodPost, path: "/v1/chat/completion", wantStatus: http.StatusNotFound},
		{method: http.MethodPost, path: "/v3/contents/generation/tasks", wantStatus: http.StatusNotFound},
		{method: http.MethodHead, path: probe.ReadinessPath, wantStatus: http.StatusNotFound},
		// 其余才归页面。
		{method: http.MethodGet, path: "/dashboard", wantStatus: http.StatusOK, wantPage: true},
		{method: http.MethodGet, path: "/custom-voices", wantStatus: http.StatusOK, wantPage: true},
		{method: http.MethodGet, path: "/", wantStatus: http.StatusOK, wantPage: true},
	}
	for _, tc := range cases {
		reached = ""
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, tc.wantStatus, w.Code, "%s %s", tc.method, tc.path)
		require.Equal(t, tc.wantReached, reached, "%s %s", tc.method, tc.path)
		require.Equal(t, tc.wantPage, w.Body.String() == spaFallbackTestPage, "%s %s", tc.method, tc.path)
	}
}

// "路径属于路由表"必须和 gin 自己的匹配一致：同一组模式注册进 gin，拿 gin 的裁决当对照。
func TestRoutedPathsAgreeWithGinMatching(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oracle := gin.New()
	claimed := ""
	oracle.Use(func(c *gin.Context) {
		claimed = c.FullPath()
		c.AbortWithStatus(http.StatusNoContent)
	})
	for _, pattern := range []string{
		"/",
		"/a/b",
		"/users/:id",
		"/users/:id/posts",
		"/files/*rest",
		"/v1/models",
		"/v1/models/:model",
		"/:tenant/dashboard",
	} {
		oracle.GET(pattern, func(*gin.Context) {})
	}
	routed := &routedPaths{table: oracle}

	for _, path := range []string{
		"/", "/a", "/a/b", "/a/b/", "/a/b/c", "/b",
		"/users", "/users/", "/users/1", "/users/1/", "/users/1/posts", "/users/1/posts/2",
		"/files", "/files/", "/files/x", "/files/x/y/z",
		"/v1/models", "/v1/models/", "/v1/models/gpt", "/v1/models/gpt/x",
		"/assets/index-abcdef12.js", "/dashboard", "/acme/dashboard", "/acme/dashboard/x", "/a/dashboard",
	} {
		claimed = ""
		oracle.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, claimed != "", routed.match(path), "path=%s gin 认领=%q", path, claimed)
	}
}

// 路由表只在第一次查询时读取；读到的是查询那一刻的整张表（注册晚于中间件创建的路由也在内）。
func TestRoutedPathsReadTheTableRegisteredAfterTheMiddleware(t *testing.T) {
	router := spaFallbackTestRouter()
	router.POST("/embeddings", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/embeddings", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	require.NotEqual(t, spaFallbackTestPage, w.Body.String())
}

// 探针路径被 SPA 兜底吞掉的话，编排器拿到的是 200 + HTML，故障实例永远摘不掉——哪怕方法不对也不行。
func TestEmbeddedFrontendBypassesEveryProbePath(t *testing.T) {
	for _, path := range probe.Paths() {
		require.True(t, shouldBypassEmbeddedFrontend(path), "probe path=%s", path)
	}
}

func TestEmbeddedFrontendBypassesBareVideoAPIRoutes(t *testing.T) {
	for _, path := range []string{
		"/videos/generations",
		"/videos/edits",
		"/videos/extensions",
		"/videos/request-123",
	} {
		require.True(t, shouldBypassEmbeddedFrontend(path), "path=%s", path)
	}
}
