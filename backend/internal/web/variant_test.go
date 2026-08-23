//go:build embed

package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/frontendvariant"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这一组护栏遍历"嵌进二进制的东西"本身（AvailableVariants），不点名任何变体：
// 明天新增一个 frontend-variants/acme，只要它进了产物，当天就被这些用例覆盖。

func TestAvailableVariantsAreValidAndSorted(t *testing.T) {
	available := AvailableVariants()
	require.NotEmpty(t, available, "embed 构建里一个可服务的变体都没有；先跑 pnpm run build:all")

	for i, name := range available {
		require.NoError(t, frontendvariant.ValidateName(name), "变体名 %q 不合法", name)
		if i > 0 {
			assert.Less(t, available[i-1], name, "AvailableVariants 必须有序，否则启动日志里的 available 每次都不一样")
		}
	}
}

// 默认配置（server.frontend_variant=default）必须能启动：dist/default 缺席等于所有没设环境变量的部署全挂。
func TestDefaultVariantIsAlwaysEmbedded(t *testing.T) {
	assert.Contains(t, AvailableVariants(), frontendvariant.Default)
}

// 每个嵌入的变体都必须能被完整地服务出来：index.html 存在、能注入公开配置、
// 注入的 <script> 带 CSP nonce 占位并在响应前被替换成真实 nonce。
// 少了 </head> 或少了 nonce 的变体会在浏览器里表现为"页面能开但配置读不到 / 脚本被 CSP 拦掉"，
// 不做这层遍历就只能等线上发现。
func TestEveryEmbeddedVariantServesInjectableIndexHTML(t *testing.T) {
	for _, name := range AvailableVariants() {
		t.Run(name, func(t *testing.T) {
			distFS, err := VariantFS(name)
			require.NoError(t, err)
			indexFile, err := distFS.Open(indexHTMLName)
			require.NoError(t, err, "变体 %s 必须有 index.html", name)
			require.NoError(t, indexFile.Close())

			provider := &mockSettingsProvider{settings: map[string]string{"site_name": "VariantGuard"}}
			server, err := NewFrontendServer(provider, "", name)
			require.NoError(t, err)

			// 注入后的 HTML（替换 nonce 之前）必须带占位符，否则请求期无从注入真实 nonce。
			injected := server.injectSettings([]byte(`{"site_name":"VariantGuard"}`))
			assert.Contains(t, string(injected), NonceHTMLPlaceholder,
				"变体 %s 的 index.html 注入后没有 nonce 占位符（多半是缺 </head>）", name)

			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(middleware.CSPNonceKey, "test-nonce-value")
				c.Next()
			})
			router.Use(server.Middleware())

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			require.Equal(t, http.StatusOK, w.Code)
			body := w.Body.String()
			assert.Contains(t, body, "window.__APP_CONFIG__", "变体 %s 没有注入公开配置", name)
			assert.Contains(t, body, `nonce="test-nonce-value"`, "变体 %s 的注入脚本没拿到真实 nonce", name)
			assert.NotContains(t, body, NonceHTMLPlaceholder, "变体 %s 的响应里还留着 nonce 占位符", name)
		})
	}
}

// 选不中必须是错误而不是回落，且错误里要列出可用变体——否则运维只能去镜像里翻目录。
func TestVariantFSRejectsUnknownNamesAndListsAvailable(t *testing.T) {
	available := AvailableVariants()
	require.NotEmpty(t, available)

	for _, name := range []string{"", "does-not-exist", "Bad_Name", "../dist", "."} {
		t.Run("name="+name, func(t *testing.T) {
			distFS, err := VariantFS(name)
			require.Error(t, err)
			assert.Nil(t, distFS)
			assert.ErrorIs(t, err, ErrVariantNotFound)
			for _, candidate := range available {
				assert.Contains(t, err.Error(), candidate,
					"错误信息必须列出可用变体 %q，否则配错了也不知道该填什么", candidate)
			}
		})
	}
}

// 两条服务路径（带设置注入的 FrontendServer 与安装向导用的 legacy 中间件）必须共用同一个裁决点：
// 任何一条自己另开一套判断，都会出现"向导页是 A、主服务是 B"。
func TestBothServingPathsRejectTheSameUnknownVariant(t *testing.T) {
	provider := &mockSettingsProvider{settings: map[string]string{}}

	server, err := NewFrontendServer(provider, "", "no-such-variant")
	assert.Nil(t, server)
	require.ErrorIs(t, err, ErrVariantNotFound)

	handler, err := ServeEmbeddedFrontend("", "no-such-variant")
	assert.Nil(t, handler)
	require.ErrorIs(t, err, ErrVariantNotFound)
}

// 变体之间必须真的是两套产物：同名路径由各自的 dist 目录提供，不会串。
func TestVariantsServeTheirOwnIndexHTML(t *testing.T) {
	available := AvailableVariants()
	if len(available) < 2 {
		t.Skip("只嵌入了一个变体，无法比较隔离性")
	}

	bodies := make(map[string]string, len(available))
	for _, name := range available {
		distFS, err := VariantFS(name)
		require.NoError(t, err)
		raw, err := fs.ReadFile(distFS, indexHTMLName)
		require.NoError(t, err)
		bodies[name] = string(raw)
	}
	for name, body := range bodies {
		for other, otherBody := range bodies {
			if name == other {
				continue
			}
			assert.NotEqual(t, body, otherBody,
				"变体 %s 与 %s 的 index.html 完全相同；覆盖层没生效或产物串了", name, other)
		}
	}
	require.Contains(t, strings.ToLower(bodies[frontendvariant.Default]), "<!doctype html>")
}
