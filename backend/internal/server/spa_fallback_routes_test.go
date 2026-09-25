//go:build unit

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/web"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const spaFallbackRoutesTestPage = "<!doctype html>spa"

// spaFallbackRouter 按 SetupRouter 的顺序组装：前端兜底先挂，再注册生产的整张路由表。
// 兜底之后紧跟一个记录点：请求穿过兜底就记下 gin 匹配到的路由并结束，不去执行真实处理器。
func spaFallbackRouter(t *testing.T) (*gin.Engine, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(web.SPAFallback(r, func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(spaFallbackRoutesTestPage))
		c.Abort()
	}))
	reached := new(string)
	r.Use(func(c *gin.Context) {
		*reached = c.FullPath()
		c.AbortWithStatus(http.StatusNoContent)
	})
	pass := func(c *gin.Context) { c.Next() }
	registerRoutes(r, &handler.Handlers{Admin: &handler.AdminHandlers{}}, pass, pass, pass, pass, pass, pass,
		nil, nil, nil, nil, nil, &config.Config{}, nil, nil)
	return r, reached
}

// concreteRoutePath 把 gin/vue-router 的路由模式换成一条能被它匹配的具体路径（:param 与 *wildcard 各填一段）。
func concreteRoutePath(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") {
			segments[i] = "p1"
		}
	}
	return strings.Join(segments, "/")
}

var spaFallbackProbeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// 前端中间件在每一条已注册路由的处理链里（它先于 registerRoutes 挂载）。曾有 22 条已注册路由（方法+路径）
// 因为不在手写放行名单里被当成页面返回 200 HTML（POST /chat/completions、/v3/contents/generations/tasks 等）。
// 遍历生产的整张路由表：每条路由都必须穿过兜底到达自己的处理链；同一路径换任何方法也不许由页面应答。
// 新加的路由当天就被覆盖。
func TestSPAFallbackYieldsEveryRegisteredRoute(t *testing.T) {
	r, reached := spaFallbackRouter(t)
	routes := r.Routes()
	require.Greater(t, len(routes), 100, "路由表没注册上，遍历就没有意义")
	for _, route := range routes {
		path := concreteRoutePath(route.Path)
		for _, method := range spaFallbackProbeMethods {
			*reached = ""
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			require.NotEqual(t, spaFallbackRoutesTestPage, w.Body.String(), "%s %s（路由 %s %s）被当成页面应答", method, path, route.Method, route.Path)
			if method == route.Method {
				require.Equal(t, route.Path, *reached, "%s %s 没有到达自己的处理链", route.Method, route.Path)
			}
		}
	}
}

// 反方向：前端路由表（frontend/src/router/index.ts）声明的每个页面都必须仍由兜底应答。
// 两张表一旦重叠，浏览器刷新或直接打开那个页面会打到 API 上。由页面声明驱动，新页面当天被覆盖。
func TestFrontendPagesAreNotClaimedByBackendRoutes(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "frontend", "src", "router", "index.ts"))
	require.NoError(t, err)
	var pages []string
	for _, m := range regexp.MustCompile(`path:\s*['"](/[^'"]*)['"]`).FindAllSubmatch(source, -1) {
		pages = append(pages, string(m[1]))
	}
	require.Greater(t, len(pages), 10, "没从前端路由表里解析出页面声明")

	r, reached := spaFallbackRouter(t)
	for _, page := range pages {
		*reached = ""
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, concreteRoutePath(page), nil))
		require.Empty(t, *reached, "前端页面 %s 被后端路由认领", page)
		require.Equal(t, spaFallbackRoutesTestPage, w.Body.String(), "前端页面 %s 没有由兜底应答（status=%d）", page, w.Code)
	}
}
