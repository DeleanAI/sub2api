package web

import (
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/probe"
	"github.com/gin-gonic/gin"
)

// RouteTable 是前端兜底判定"这条路径归不归 API"时读取的路由表；*gin.Engine 满足它。
type RouteTable interface {
	Routes() gin.RoutesInfo
}

// SPAFallback 把"返回前端页面或静态文件"的处理器 serve 包成只接路由表不要的请求。
// 两个前端入口（FrontendServer.Middleware 与安装向导用的 ServeEmbeddedFrontend）都建立在它上面。
//
// 以下请求一律交给后面的处理链（路由自己的处理器，或 gin 的 404），serve 不会被调用：
//
//   - 路径属于路由表，不论方法。主服务把前端中间件挂在 registerRoutes 之前，于是它出现在每一条
//     已注册路由的处理链里；过去只靠手写名单放行，名单外的已注册路由（POST /chat/completions、
//     /v3/contents/generations/tasks 等）拿到的是 200 + index.html。gin 已为请求匹配到路由时
//     c.FullPath() 非空，直接放行（API 请求走这条，不必再查表）；方法不对的（如方舟 SDK 的
//     GET /v3/contents/generations/tasks 任务列表，本网关不开放）得到 404 而不是一个"成功"的页面。
//   - 路径落在保留给 API 的命名空间里（shouldBypassEmbeddedFrontend）：命名空间里的未知路径同样 404。
//
// 第一条以路由表本身为准，新注册的路由当天生效，不需要再往名单里补。
func SPAFallback(table RouteTable, serve gin.HandlerFunc) gin.HandlerFunc {
	routed := &routedPaths{table: table}
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if c.FullPath() != "" || routed.match(path) || shouldBypassEmbeddedFrontend(path) {
			c.Next()
			return
		}
		serve(c)
	}
}

// routedPaths 回答"路由表里有没有哪条路由（不论方法）匹配这条路径"，规则与 gin 的路由树一致：
// 静态段逐字相等，:param 匹配一个非空段，*wildcard 匹配其后的全部剩余（它前面的斜杠必须在）。
// 路由表在第一次查询时读取一次：两个服务入口都在开始监听之前注册完全部路由。
type routedPaths struct {
	table    RouteTable
	once     sync.Once
	byHead   map[string][][]string // 首段是静态段的路由模式，按首段分桶
	wildHead [][]string            // 首段就是 :param / *wildcard 的路由模式
}

func (p *routedPaths) match(path string) bool {
	p.once.Do(p.load)
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, bucket := range [][][]string{p.byHead[segments[0]], p.wildHead} {
		for _, pattern := range bucket {
			if routePatternMatches(pattern, segments) {
				return true
			}
		}
	}
	return false
}

func (p *routedPaths) load() {
	p.byHead = map[string][][]string{}
	seen := map[string]bool{}
	for _, route := range p.table.Routes() {
		if seen[route.Path] {
			continue
		}
		seen[route.Path] = true
		pattern := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
		if isRouteWildcard(pattern[0]) {
			p.wildHead = append(p.wildHead, pattern)
			continue
		}
		p.byHead[pattern[0]] = append(p.byHead[pattern[0]], pattern)
	}
}

func isRouteWildcard(segment string) bool {
	return strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*")
}

func routePatternMatches(pattern, segments []string) bool {
	for i, want := range pattern {
		if i >= len(segments) {
			return false
		}
		switch {
		case strings.HasPrefix(want, "*"):
			return true
		case strings.HasPrefix(want, ":"):
			if segments[i] == "" {
				return false
			}
		case want != segments[i]:
			return false
		}
	}
	return len(pattern) == len(segments)
}

// shouldBypassEmbeddedFrontend 声明保留给 API 的命名空间：路由表没认领的路径落在这里也不许由页面应答。
// 已注册的路由不必列在这里（SPAFallback 按路由表放行，不论方法）。
func shouldBypassEmbeddedFrontend(path string) bool {
	trimmed := strings.TrimSpace(path)
	// 探针路径（/health、/readyz）必须绕过 SPA 兜底：编排器要的是状态码，不是 index.html。
	// 它们本身是已注册路由；这里再按声明保留一次，不依赖探针与前端挂在同一张路由表上。
	return probe.IsPath(trimmed) ||
		strings.HasPrefix(trimmed, "/api/") ||
		strings.HasPrefix(trimmed, "/v1/") ||
		strings.HasPrefix(trimmed, "/v1beta/") ||
		strings.HasPrefix(trimmed, "/v3/") ||
		strings.HasPrefix(trimmed, "/backend-api/") ||
		strings.HasPrefix(trimmed, "/antigravity/") ||
		strings.HasPrefix(trimmed, "/setup/") ||
		strings.HasPrefix(trimmed, "/images/") ||
		strings.HasPrefix(trimmed, "/videos/")
}
