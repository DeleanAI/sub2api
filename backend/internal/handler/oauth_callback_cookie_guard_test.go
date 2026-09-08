//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestOAuthCallbackClearsCookiesBeforeRedirect 钉死"清 cookie 必须发生在写响应之前"。
//
// 之前飞书与钉钉的回调都用 defer 清 cookie，而每条出口都以 c.Redirect 结束——Redirect
// 已经把响应头提交，defer 里的 SetCookie 一个都到不了浏览器（302 响应里看不到任何
// Set-Cookie）。结果是 state 在完整 TTL 内一直有效，"一次性 state"只存在于代码阅读中。
// 断言看的是真实响应头，而不是源码里有没有写清除调用——后者正是当初以为已经做了的事。
func TestOAuthCallbackClearsCookiesBeforeRedirect(t *testing.T) {
	cases := []struct {
		name        string
		stateCookie string
		clear       func(*gin.Context, string, bool)
	}{
		{"feishu", feishuOAuthStateCookieName, clearFeishuCookie},
		{"dingtalk", dingTalkOAuthStateCookieName, clearDingTalkCookie},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.GET("/cb", func(c *gin.Context) {
				// 与真实回调同样的顺序：先清，再重定向。
				tc.clear(c, tc.stateCookie, isRequestHTTPS(c))
				redirectOAuthError(c, "/login", "csrf", "state mismatch", "")
			})

			req := httptest.NewRequest(http.MethodGet, "/cb?code=c&state=s", nil)
			req.AddCookie(&http.Cookie{Name: tc.stateCookie, Value: "s"})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusFound, w.Code)
			cleared := false
			for _, cookie := range w.Result().Cookies() {
				if cookie.Name == tc.stateCookie && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			require.Truef(t, cleared,
				"302 响应里没有清除 %s 的 Set-Cookie：清除若发生在 Redirect 之后浏览器收不到，"+
					"state 会在整个 TTL 内保持有效", tc.stateCookie)
		})
	}
}
