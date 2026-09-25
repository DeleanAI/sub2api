//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func seedanceTestAccount() *Account {
	return &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "ark-secret", "base_url": "https://ark.cn-beijing.volces.com/api/v3",
		"openai_capabilities": []string{"seedance"},
		"model_mapping":       map[string]any{"video": "ep-seedance"},
	}}
}

func TestSeedanceNativeForwarding(t *testing.T) {
	body := []byte(`{"model":"video","content":[{"type":"text","text":"waves"},{"type":"image_url","image_url":{"url":"https://example.com/first.png"},"role":"first_frame"},{"type":"audio_url","audio_url":{"url":"https://example.com/audio.mp3"}}],"duration":-1,"generate_audio":true,"future_field":{"keep":true}}`)
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	result, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointCreate, "", body)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"task-1"}`, w.Body.String())
	require.Equal(t, "seedance:task-1", result.ResponseID)
	require.Zero(t, result.Usage.OutputTokens)
	require.Equal(t, "video", result.BillingModel)
	require.Equal(t, "ep-seedance", result.UpstreamModel)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/v3/contents/generations/tasks", upstream.request.URL.String())
	require.Equal(t, "Bearer ark-secret", upstream.request.Header.Get("Authorization"))
	forwarded, err := io.ReadAll(upstream.request.Body)
	require.NoError(t, err)
	require.Equal(t, "ep-seedance", gjson.GetBytes(forwarded, "model").String())
	for _, field := range []string{"content", "duration", "generate_audio", "future_field"} {
		require.Equal(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(forwarded, field).Raw)
	}
}

func TestSeedanceStatusAndDelete(t *testing.T) {
	for _, status := range []string{"queued", "running", "failed", "cancelled", "expired", "succeeded"} {
		t.Run(status, func(t *testing.T) {
			body := `{"id":"task-1","status":"` + status + `","model":"ep-seedance","content":{"video_url":"https://cdn.example/video.mp4"},"usage":{"completion_tokens":12345}}`
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(body)}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			c, w := grokMediaContentTestContext(http.MethodGet, "/api/v3/contents/generations/tasks/task-1", nil)
			result, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointStatus, "seedance:task-1", nil)
			require.NoError(t, err)
			require.JSONEq(t, body, w.Body.String())
			require.Equal(t, "/api/v3/contents/generations/tasks/task-1", upstream.request.URL.Path)
			if status == "succeeded" {
				require.Equal(t, 12345, result.Usage.OutputTokens)
			} else {
				require.Zero(t, result.Usage.OutputTokens)
			}
			require.Zero(t, result.VideoCount, "must use token billing, not Grok seconds")
		})
	}
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse("")}
	upstream.response.StatusCode = http.StatusNoContent
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodDelete, "/api/v3/contents/generations/tasks/task-1", nil)
	_, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointDelete, "seedance:task-1", nil)
	require.NoError(t, err)
	require.Equal(t, http.MethodDelete, upstream.request.Method)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestSeedanceValidationAndCapability(t *testing.T) {
	for _, body := range []string{`{`, `[]`, `{}`, `{"model":12,"content":[{}]}`, `{"model":"x","content":[]}`} {
		_, err := ParseSeedanceRequest([]byte(body))
		require.Error(t, err)
	}
	info, err := ParseSeedanceRequest([]byte(`{"model":"x","content":[{"type":"text","text":"first"},{"type":"text","text":"second"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}`))
	require.NoError(t, err)
	require.Contains(t, string(info.ModerationBody()), "first")
	require.Contains(t, string(info.ModerationBody()), "second")
	require.Contains(t, string(info.ModerationBody()), "https://example.com/a.png")
	for _, id := range []string{"", "..", "a/b", "a?b", "a#b", "%2e%2e"} {
		_, err := buildSeedanceURL("https://example.com", SeedanceEndpointStatus, id)
		require.Error(t, err, id)
	}
	for _, base := range []string{"https://example.com", "https://example.com/api/v3/", "https://example.com/v3"} {
		url, err := buildSeedanceURL(base, SeedanceEndpointCreate, "")
		require.NoError(t, err)
		require.NotContains(t, url, "/v3/api/v3")
	}
	a := seedanceTestAccount()
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Type = AccountTypeOAuth
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
	a.Type = AccountTypeAPIKey
	delete(a.Credentials, "openai_capabilities")
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance))
}

func TestSeedancePreservesUpstreamErrorsWithoutRetry(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"error":{"code":"QuotaExceeded","message":"quota exhausted"}}`)}
	upstream.response.StatusCode = 429
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	_, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointCreate, "", []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))
	require.Error(t, err)
	require.Equal(t, 429, w.Code)
	require.Contains(t, w.Body.String(), "QuotaExceeded")
	require.Len(t, upstream.requests, 1)
}

// Seedance 走原生透传：请求体除 model 按账号映射改写外原样转发，callback_url 也不例外
// （回调直达调用方时的计费由网关的后台结算兜底，不靠改写或拒绝请求）。
func TestSeedanceForwardsCallbackURLUntouched(t *testing.T) {
	body := []byte(`{"model":"video","content":[{"type":"text","text":"waves"}],"callback_url":"https://client.example/hook","execution_expires_after":3600}`)
	_, err := ParseSeedanceRequest(body)
	require.NoError(t, err)
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, _ := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)
	_, err = svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointCreate, "", body)
	require.NoError(t, err)
	forwarded, err := io.ReadAll(upstream.request.Body)
	require.NoError(t, err)
	for _, field := range []string{"content", "callback_url", "execution_expires_after"} {
		require.Equal(t, gjson.GetBytes(body, field).Raw, gjson.GetBytes(forwarded, field).Raw, field)
	}
}

func TestSeedanceDraftTaskReferences(t *testing.T) {
	info, err := ParseSeedanceRequest([]byte(`{"model":"x","content":[{"type":"draft_task","draft_task":{"id":"cgt-draft-1"}}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{SeedanceTaskKey("cgt-draft-1")}, info.ReferencedTaskKeys)
	require.True(t, IsSeedanceTaskKey(info.ReferencedTaskKeys[0]))
	require.Equal(t, "cgt-draft-1", seedanceUpstreamTaskID(info.ReferencedTaskKeys[0]))
	for _, body := range []string{
		`{"model":"x","content":[{"type":"draft_task","draft_task":{}}]}`,
		`{"model":"x","content":[{"type":"draft_task","draft_task":{"id":""}}]}`,
		`{"model":"x","content":[{"type":"draft_task","draft_task":{"id":7}}]}`,
	} {
		_, err := ParseSeedanceRequest([]byte(body))
		require.Error(t, err, body)
	}
}

type ttlRecordingCache struct {
	GatewayCache
	ttls map[string]time.Duration
}

func (c *ttlRecordingCache) SetSessionAccountID(_ context.Context, _ int64, _ string, _ int64, ttl time.Duration) error {
	c.ttls["binding"] = ttl
	return nil
}

func (c *ttlRecordingCache) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, ttl time.Duration) error {
	c.ttls["pending"] = ttl
	return nil
}

func (c *ttlRecordingCache) ClaimGrokVideoBilled(_ context.Context, _ string, ttl time.Duration) (bool, error) {
	c.ttls["claim"] = ttl
	return true, nil
}

// 归属绑定与计费快照必须活得不短于上游还能回答查询的时长：方舟任务最长排队/运行
// execution_expires_after 上限 259200s，成功后 video_url 有效 24h，任务记录保存 7 天。
func TestAsyncVideoTaskRetentionFollowsProvider(t *testing.T) {
	require.GreaterOrEqual(t, seedanceTaskRetention, 259200*time.Second+24*time.Hour)
	for _, tc := range []struct {
		key                     string
		binding, pending, claim time.Duration
	}{
		{SeedanceTaskKey("cgt-1"), 7 * 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour},
		{"grok-request-1", 24 * time.Hour, 24 * time.Hour, 48 * time.Hour}, // Grok 保持原值
	} {
		cache := &ttlRecordingCache{ttls: map[string]time.Duration{}}
		svc := &OpenAIGatewayService{cache: cache}
		groupID := int64(24)
		require.NoError(t, svc.BindGrokMediaVideoRequestAccount(context.Background(), &groupID, tc.key, 10, 20, 1))
		require.NoError(t, svc.StoreGrokVideoPendingBilling(context.Background(), tc.key, 10, 20, GrokVideoPendingBilling{Model: "m"}))
		claimed, err := svc.ClaimGrokVideoBilling(context.Background(), tc.key, 10, 20)
		require.NoError(t, err)
		require.True(t, claimed)
		require.Equal(t, tc.binding, cache.ttls["binding"], tc.key)
		require.Equal(t, tc.pending, cache.ttls["pending"], tc.key)
		require.Equal(t, tc.claim, cache.ttls["claim"], tc.key)
		require.Greater(t, cache.ttls["claim"], cache.ttls["binding"], "已计费标记必须比归属绑定活得久，否则绑定还在时标记先过期会重复计费")
	}
}
