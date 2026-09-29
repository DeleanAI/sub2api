//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// 开放范围由声明决定：声明里的每个 Action 在规定版本下放行，换版本或不在声明里的一律拒绝。
func TestSeedanceAssetActionsAreTheDeclaredSetAtTheDeclaredVersion(t *testing.T) {
	require.Len(t, seedanceAssetActions, 10, "方舟私域虚拟人像库 API 参考（2024-01-01）共 10 个接口")
	for action := range seedanceAssetActions {
		require.Nil(t, LookupSeedanceAssetAction(action, SeedanceAssetAPIVersion), action)
		err := LookupSeedanceAssetAction(action, "2023-01-01")
		require.NotNil(t, err, action)
		require.Equal(t, http.StatusBadRequest, err.Status)
	}
	for _, action := range []string{"", "ListFooBar", "listassetgroups", "CreateRealPersonAuthorization"} {
		require.NotNil(t, LookupSeedanceAssetAction(action, SeedanceAssetAPIVersion), action)
	}
}

// 素材库挂在方舟 API 的根上：base_url 不论写成源站、代理前缀还是带版本段，都落到同一个根。
func TestSeedanceAssetURLUsesTheAPIRoot(t *testing.T) {
	for base, want := range map[string]string{
		"https://ai-gateway.baidubce.com":           "https://ai-gateway.baidubce.com/?Action=ListAssets&Version=2024-01-01",
		"https://ark.cn-beijing.volces.com/api/v3/": "https://ark.cn-beijing.volces.com/?Action=ListAssets&Version=2024-01-01",
		"https://proxy.example/ark/v3":              "https://proxy.example/ark/?Action=ListAssets&Version=2024-01-01",
		"https://proxy.example/ark":                 "https://proxy.example/ark/?Action=ListAssets&Version=2024-01-01",
	} {
		require.Equal(t, want, buildSeedanceAssetURL(base, "ListAssets", SeedanceAssetAPIVersion), base)
	}
}

// 请求体与上游的回答原样透传，鉴权换成账号的 Key；上游的错误（含状态码）同样原样回给调用方。
func TestForwardSeedanceAssetPassesThrough(t *testing.T) {
	body := []byte(`{"Filter":{"GroupType":"AIGC","GroupIds":["group-1"]},"PageNumber":2,"PageSize":5,"future_field":{"keep":true}}`)
	for _, upstreamStatus := range []int{http.StatusOK, http.StatusNotFound} {
		reply := `{"ResponseMetadata":{"Action":"ListAssets"},"Result":{"Items":[{"Id":"asset-1","GroupId":"group-1"}],"TotalCount":1}}`
		if upstreamStatus != http.StatusOK {
			reply = `{"ResponseMetadata":{"Action":"ListAssets","Error":{"Code":"NotFound","Message":"asset group not found"}}}`
		}
		upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(reply)}
		upstream.response.StatusCode = upstreamStatus
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		c, w := grokMediaContentTestContext(http.MethodPost, "/?Action=ListAssets&Version=2024-01-01", nil)
		account := seedanceTestAccount()
		status, err := svc.ForwardSeedanceAsset(context.Background(), c, account, "ListAssets", SeedanceAssetAPIVersion, body)
		require.NoError(t, err)
		require.Equal(t, upstreamStatus, status)
		require.Equal(t, upstreamStatus, w.Code)
		require.JSONEq(t, reply, w.Body.String())
		require.Len(t, upstream.requests, 1)
		require.Equal(t, http.MethodPost, upstream.request.Method)
		require.Equal(t, "https://ark.cn-beijing.volces.com/?Action=ListAssets&Version=2024-01-01", upstream.request.URL.String())
		require.Equal(t, "Bearer ark-secret", upstream.request.Header.Get("Authorization"))
		forwarded, err := io.ReadAll(upstream.request.Body)
		require.NoError(t, err)
		require.Equal(t, string(body), string(forwarded))
	}
}
