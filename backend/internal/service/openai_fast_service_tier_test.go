package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// 请求侧：service_tier 校验（fast/priority 等价、非法值拒绝、省略保持现状）
// ---------------------------------------------------------------------------

func TestValidateOpenAIServiceTierField(t *testing.T) {
	t.Parallel()

	t.Run("fast normalizes to priority", func(t *testing.T) {
		norm, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":"fast"}`))
		require.NoError(t, err)
		require.Equal(t, "priority", norm)
	})

	t.Run("priority passes through", func(t *testing.T) {
		norm, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":"priority"}`))
		require.NoError(t, err)
		require.Equal(t, "priority", norm)
	})

	t.Run("case and whitespace insensitive", func(t *testing.T) {
		norm, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":"  FAST "}`))
		require.NoError(t, err)
		require.Equal(t, "priority", norm)
	})

	t.Run("official tiers pass through", func(t *testing.T) {
		for _, tier := range []string{"flex", "auto", "default", "scale", "ultrafast"} {
			norm, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":"` + tier + `"}`))
			require.NoError(t, err, "tier %q must be accepted", tier)
			require.Equal(t, tier, norm)
		}
	})

	t.Run("invalid tier rejected", func(t *testing.T) {
		_, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":"turbo"}`))
		require.Error(t, err)
		var invalid *ErrInvalidOpenAIServiceTier
		require.True(t, errors.As(err, &invalid))
		require.Equal(t, "turbo", invalid.Value)
		require.Contains(t, err.Error(), "invalid service_tier")
		require.Contains(t, err.Error(), "fast", "allowed-value hint must mention fast")
	})

	t.Run("omitted field stays valid", func(t *testing.T) {
		norm, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","input":"hi"}`))
		require.NoError(t, err)
		require.Empty(t, norm)
	})

	t.Run("null value keeps omission semantics", func(t *testing.T) {
		norm, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":null}`))
		require.NoError(t, err)
		require.Empty(t, norm)
	})

	t.Run("explicit empty string rejected as invalid enum value", func(t *testing.T) {
		_, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":""}`))
		require.Error(t, err)
		var invalid *ErrInvalidOpenAIServiceTier
		require.True(t, errors.As(err, &invalid))
	})

	t.Run("non-string service_tier rejected", func(t *testing.T) {
		// service_tier 必须为字符串；数字/布尔/对象/数组等类型同样按非法值拒绝。
		for _, raw := range []string{
			`{"model":"gpt-5.5","service_tier":123}`,
			`{"model":"gpt-5.5","service_tier":true}`,
			`{"model":"gpt-5.5","service_tier":{}}`,
			`{"model":"gpt-5.5","service_tier":["priority"]}`,
		} {
			_, err := ValidateOpenAIServiceTierField([]byte(raw))
			require.Error(t, err, "raw=%s must be rejected", raw)
			var invalid *ErrInvalidOpenAIServiceTier
			require.True(t, errors.As(err, &invalid), "raw=%s", raw)
			require.Equal(t, "<non-string>", invalid.Value, "raw=%s", raw)
			require.Contains(t, err.Error(), "invalid service_tier")
		}
	})

	t.Run("oversized unknown string is truncated", func(t *testing.T) {
		blob := strings.Repeat("z", 4096)
		_, err := ValidateOpenAIServiceTierField([]byte(`{"model":"gpt-5.5","service_tier":"` + blob + `"}`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid service_tier")
		require.NotContains(t, err.Error(), blob)
		require.Less(t, len(err.Error()), 200)
		var invalid *ErrInvalidOpenAIServiceTier
		require.True(t, errors.As(err, &invalid))
		require.Equal(t, strings.Repeat("z", 64)+"...", invalid.Value)
	})

	t.Run("non-string large object/array is not echoed", func(t *testing.T) {
		blob := strings.Repeat("x", 4096)
		payloads := []string{
			`{"model":"gpt-5.5","service_tier":{"blob":"` + blob + `"}}`,
			`{"model":"gpt-5.5","service_tier":["` + blob + `"]}`,
		}
		for _, raw := range payloads {
			_, err := ValidateOpenAIServiceTierField([]byte(raw))
			require.Error(t, err)
			require.Contains(t, err.Error(), "invalid service_tier")
			require.NotContains(t, err.Error(), blob)
			require.Less(t, len(err.Error()), 200)
			var invalid *ErrInvalidOpenAIServiceTier
			require.True(t, errors.As(err, &invalid))
			require.Equal(t, "<non-string>", invalid.Value)
		}
	})
}

// ---------------------------------------------------------------------------
// 计费：Fast 按官方价目录声明的倍数（gpt-5.5 2.5x，其余 2x）；渠道显式 Fast 倍率优先
// ---------------------------------------------------------------------------

func TestOpenAIFastBilling_UsesOfficialTierAndChannelMultiplierWins(t *testing.T) {
	t.Parallel()

	billing := newOfficialBillingService(t, "")
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}
	for model, ratio := range map[string]float64{"gpt-5.4": 2, "gpt-5.5": 2.5, "gpt-5.6-sol": 2, "gpt-5.6-terra": 2, "gpt-5.6-luna": 2} {
		pricing, err := billing.GetModelPricing(model)
		require.NoError(t, err)
		standard := billing.computeTokenBreakdown(pricing, tokens, 1, "", false)
		for _, tier := range []string{"fast", "priority"} {
			fast := billing.computeTokenBreakdown(pricing, tokens, 1, tier, false)
			require.InDelta(t, standard.TotalCost*ratio, fast.TotalCost, 1e-9, "%s %s", model, tier)
			require.Empty(t, fast.ServiceTierNotOffered)
		}
		defaultTier := billing.computeTokenBreakdown(pricing, tokens, 1, "default", false)
		require.InDelta(t, standard.TotalCost, defaultTier.TotalCost, 1e-12, model)

		override := *pricing
		multiplier := 1.7
		override.FastMultiplier = &multiplier
		overridden := billing.computeTokenBreakdown(&override, tokens, 1, "fast", false)
		require.InDelta(t, standard.TotalCost*1.7, overridden.TotalCost, 1e-9, "%s channel fast multiplier", model)
	}
}

// ---------------------------------------------------------------------------
// 上游 payload：fast 归一化为 priority 并确实到达上游
// ---------------------------------------------------------------------------

func TestForwardAsChatCompletions_ServiceTierFastNormalizedToPriorityUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"service_tier":"fast","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-chat-st"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop"}}`)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          21,
		Name:        "openai-compatible",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-compatible"},
		Extra:       map[string]any{"openai_responses_supported": true},
	}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.5")
	require.Error(t, err) // upstream 400 → 错误返回，但请求体已被 recorder 捕获
	require.NotNil(t, upstream.lastBody)
	require.Equal(t, "priority", gjson.GetBytes(upstream.lastBody, "service_tier").String(),
		"client alias fast must reach upstream as priority")
}

func TestForwardAsChatCompletions_ServiceTierPriorityPreservedUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"service_tier":"priority","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-chat-st2"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"stop"}}`)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          2,
		Name:        "openai-compatible",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-compatible"},
		Extra:       map[string]any{"openai_responses_supported": true},
	}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.5")
	require.Error(t, err)
	require.NotNil(t, upstream.lastBody)
	require.Equal(t, "priority", gjson.GetBytes(upstream.lastBody, "service_tier").String())
}

func TestForward_ResponsesServiceTierFastNormalizedToPriorityUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","service_tier":"fast","input":"hello","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-resp-st"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          7,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastBody)
	require.Equal(t, "priority", gjson.GetBytes(upstream.lastBody, "service_tier").String(),
		"client alias fast must reach the upstream as priority")
	// 计费上下文：result 携带归一化后的 tier。
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier)
}

func TestForward_ResponsesServiceTierOmittedStaysOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","input":"hello","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-resp-st2"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_2","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          7,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastBody)
	require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists(),
		"omitted service_tier must stay omitted")
	require.Nil(t, result.ServiceTier)
}

// ---------------------------------------------------------------------------
// 流式计费上下文：service_tier 需要从请求体传到 usage 计费
// ---------------------------------------------------------------------------

func TestForwardStreaming_ServiceTierPropagatedToResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","service_tier":"fast","input":"hello","stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	streamPayload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s1\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"it_1\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s1\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-resp-stream-st"}},
		Body:       io.NopCloser(strings.NewReader(streamPayload)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          7,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier, "streaming billing context must carry the normalized tier")
	// /v1/responses 流是上游 SSE 原样透传：上游没回 service_tier 就不该出现；
	// 网关只在计费结果里携带请求侧 tier，不往下游流里注入。
	require.Contains(t, rec.Body.String(), `"delta":"hi"`, "streamed content must reach the client")
	require.NotContains(t, rec.Body.String(), `"service_tier"`, "upstream did not return service_tier, client stream must stay untouched")
}

// ---------------------------------------------------------------------------
// 转发阶段分别保留最终出站 tier 与上游回显 tier
// ---------------------------------------------------------------------------

func TestForward_ResponsesKeepsOutboundAndObservedServiceTiersSeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","service_tier":"fast","input":"hello","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 上游回显 service_tier=default（例如请求实际被降级）。
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-resp-echo"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","service_tier":"default","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          7,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	// 非流式响应原样透传：客户端同样看到 default。
	require.Contains(t, rec.Body.String(), `"service_tier":"default"`)
	require.NotContains(t, rec.Body.String(), `"service_tier":"priority"`)
}

func TestForwardStreaming_KeepsOutboundAndObservedServiceTiersSeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","service_tier":"fast","input":"hello","stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	streamPayload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s1\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s1\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"service_tier\":\"default\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-resp-echo-stream"}},
		Body:       io.NopCloser(strings.NewReader(streamPayload)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          7,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	// 流式原样透传：客户端在终止事件里看到 default。
	require.Contains(t, rec.Body.String(), `"service_tier":"default"`)
}

func TestForwardAsChatCompletions_KeepsOutboundAndObservedServiceTiersSeparate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"service_tier":"fast","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	streamPayload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_c1\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"it_1\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_c1\",\"object\":\"response\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"service_tier\":\"default\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"data: [DONE]\n\n"

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-chat-echo"}},
		Body:       io.NopCloser(strings.NewReader(streamPayload)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, &config.Config{}),
	}
	account := &Account{
		ID:          21,
		Name:        "openai-compatible",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-compatible"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.ServiceTier)
	require.Equal(t, "priority", *result.ServiceTier)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	// 缓冲转回 Chat Completions：客户端响应里如实回显 default。
	require.Contains(t, rec.Body.String(), `"service_tier":"default"`)
	require.NotContains(t, rec.Body.String(), `"service_tier":"priority"`)
}

// ---------------------------------------------------------------------------
// policy filter：删除 service_tier 后不得再按原请求 Fast 计费
// ---------------------------------------------------------------------------

func TestForward_ServiceTierFilteredByPolicyBillsStandard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","service_tier":"priority","input":"hello","stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 管理员配置 priority → filter：字段在出站前被删除。
	settings := &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
		ServiceTier: OpenAIFastTierPriority,
		Action:      BetaPolicyActionFilter,
		Scope:       BetaPolicyScopeAll,
	}}}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	repo := &openAIFastPolicyRepoStub{values: map[string]string{SettingKeyOpenAIFastPolicySettings: string(raw)}}

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-resp-filter"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		)),
	}}

	svc := &OpenAIGatewayService{
		cfg:            &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream:   upstream,
		settingService: NewSettingService(repo, &config.Config{}),
	}
	account := &Account{
		ID:          7,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive,
		Schedulable: true,
	}

	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	// 出站 body 已剥离 service_tier、上游也未回显 → 无 tier → 按标准价计费。
	require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists(),
		"policy filter must strip service_tier from the outbound body")
	require.Nil(t, result.ServiceTier, "filtered request must not bill as fast")
}

// ---------------------------------------------------------------------------
// 上游回显观察与解析器单测
// ---------------------------------------------------------------------------

func TestUpstreamResponseModelObserver_ObservesServiceTier(t *testing.T) {
	t.Parallel()

	observer := &upstreamResponseModelObserver{}
	// 上游约束：非终止且有类型的事件（response.created）回显的是请求档位而非
	// 实际处理档位，忽略。
	observer.ObserveOpenAI([]byte(`{"type":"response.created","response":{"model":"gpt-5.5","service_tier":"flex"}}`), "response.created")
	require.Empty(t, observer.ServiceTier())

	// terminal 声明（带 model 帧）优先。
	observer.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.5","service_tier":"default"}}`), "response.completed")
	require.Equal(t, "default", observer.ServiceTier())

	// Chat Completions 顶层 service_tier 按 untyped payload 观察（无 type 字段）。
	ccObserver := &upstreamResponseModelObserver{}
	ccObserver.ObserveOpenAI([]byte(`{"id":"chatcmpl-1","model":"gpt-5.5","service_tier":"priority","choices":[]}`), "")
	require.Equal(t, "priority", ccObserver.ServiceTier())

	// 无 model 的帧不触发 tier 观察（上游约束：tier 声明必带 model）。
	modelFree := &upstreamResponseModelObserver{}
	modelFree.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"service_tier":"default"}}`), "response.completed")
	require.Empty(t, modelFree.ServiceTier())
}

func TestResolvedOpenAIUpstreamServiceTier(t *testing.T) {
	t.Parallel()

	priority := func() *string { v := "priority"; return &v }()

	t.Run("upstream echo stays separate from outbound tier", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(nil)
		observer := beginUpstreamResponseModelObservation(c)
		observer.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.5","service_tier":"default"}}`), "response.completed")

		got := resolvedOpenAIUpstreamServiceTier(c, priority)
		require.NotNil(t, got)
		require.Equal(t, "priority", *got)
		require.Equal(t, "default", observedUpstreamResponseServiceTier(c))
	})

	t.Run("no upstream echo falls back to outbound tier", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(nil)
		beginUpstreamResponseModelObservation(c)

		got := resolvedOpenAIUpstreamServiceTier(c, priority)
		require.NotNil(t, got)
		require.Equal(t, "priority", *got)
	})

	t.Run("observed tier never promotes an untiered request", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(nil)
		observer := beginUpstreamResponseModelObservation(c)
		observer.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.5","service_tier":"fast"}}`), "response.completed")

		got := resolvedOpenAIUpstreamServiceTier(c, nil)
		require.Nil(t, got)
		require.Equal(t, "priority", observedUpstreamResponseServiceTier(c))
	})

	t.Run("no observer keeps outbound tier", func(t *testing.T) {
		got := resolvedOpenAIUpstreamServiceTier(nil, priority)
		require.NotNil(t, got)
		require.Equal(t, "priority", *got)
	})

	t.Run("no observer and no outbound tier stays nil", func(t *testing.T) {
		require.Nil(t, resolvedOpenAIUpstreamServiceTier(nil, nil))
	})

	t.Run("local observer stays separate from outbound tier", func(t *testing.T) {
		observer := &upstreamResponseModelObserver{}
		observer.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.5","service_tier":"default"}}`), "response.completed")

		got := resolvedOpenAIUpstreamServiceTierFromObserver(observer, priority)
		require.NotNil(t, got)
		require.Equal(t, "priority", *got)
	})

	t.Run("nil local observer falls back to outbound tier", func(t *testing.T) {
		got := resolvedOpenAIUpstreamServiceTierFromObserver(nil, priority)
		require.NotNil(t, got)
		require.Equal(t, "priority", *got)
	})
}
