//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSeedanceAssetsForwardsWithAccountKeyAndReleasesSlots(t *testing.T) {
	h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	body := `{"Filter":{"GroupType":"AIGC"},"PageNumber":1,"PageSize":10}`
	reply := `{"ResponseMetadata":{"Action":"ListAssetGroups"},"Result":{"Items":[{"Id":"group-1"}],"TotalCount":1}}`
	var forwarded *http.Request
	var forwardedBody string
	upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
		forwarded = req
		raw, _ := io.ReadAll(req.Body)
		forwardedBody = string(raw)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
	}
	c, w := grokMediaSlotContext(context.Background(), true)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(http.MethodPost, "/?Action=ListAssetGroups&Version=2024-01-01", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer customer-key")
	h.SeedanceAssets(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, reply, w.Body.String())
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, "https://ark.cn-beijing.volces.com/?Action=ListAssetGroups&Version=2024-01-01", forwarded.URL.String())
	require.Equal(t, "Bearer test-key", forwarded.Header.Get("Authorization"), "鉴权必须换成账号的 Key")
	require.Equal(t, body, forwardedBody, "请求体原样透传")
	slots.assertReleased(t)
}

// 不在开放范围内的调用与不具备 Seedance 资格的分组在碰上游之前就被拒绝，回答用方舟 OpenAPI 的错误格式。
func TestSeedanceAssetsRejectsBeforeReachingUpstream(t *testing.T) {
	for name, tc := range map[string]struct {
		target, platform string
		allowImages      bool
		status           int
		code             string
	}{
		"undeclared action": {"/?Action=CreateRealPersonAuthorization&Version=2024-01-01", service.PlatformOpenAI, true, http.StatusBadRequest, "InvalidActionOrVersion"},
		"wrong version":     {"/?Action=ListAssets&Version=2025-01-01", service.PlatformOpenAI, true, http.StatusBadRequest, "InvalidActionOrVersion"},
		"no action":         {"/", service.PlatformOpenAI, true, http.StatusBadRequest, "InvalidActionOrVersion"},
		"grok group":        {"/?Action=ListAssets&Version=2024-01-01", service.PlatformGrok, true, http.StatusForbidden, "AccessDenied"},
		"media disallowed":  {"/?Action=ListAssets&Version=2024-01-01", service.PlatformOpenAI, false, http.StatusForbidden, "AccessDenied"},
	} {
		t.Run(name, func(t *testing.T) {
			h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
			c, w := grokMediaSlotContext(context.Background(), true)
			key, _ := middleware.GetAPIKeyFromContext(c)
			key.Group.Platform = tc.platform
			key.Group.AllowImageGeneration = tc.allowImages
			c.Request = httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader(`{"Filter":{"GroupType":"AIGC"}}`))
			h.SeedanceAssets(c)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Equal(t, tc.code, gjson.Get(w.Body.String(), "ResponseMetadata.Error.Code").String(), w.Body.String())
			require.Zero(t, upstream.calls)
			slots.assertReleased(t)
		})
	}
}
