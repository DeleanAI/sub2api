package service

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	SeedanceEndpointCreate           GrokMediaEndpoint        = "seedance_create"
	SeedanceEndpointStatus           GrokMediaEndpoint        = "seedance_status"
	SeedanceEndpointDelete           GrokMediaEndpoint        = "seedance_delete"
	OpenAIEndpointCapabilitySeedance OpenAIEndpointCapability = "seedance"
)

// seedanceTaskKeyPrefix 是 Seedance 任务在网关内部的命名空间（归属绑定、计费快照、
// 去重标记都用它做键），只在这里声明；构造与识别分别走 SeedanceTaskKey / IsSeedanceTaskKey。
const seedanceTaskKeyPrefix = "seedance:"

// seedanceTaskRetention 是网关为一个 Seedance 任务保留归属绑定与计费快照的时长。
//
// 取方舟的任务保留期：创建接口「任务 ID 仅保存 7 天（从 created_at 开始计算）」，查询接口
// 「仅支持查询最近 7 天的任务记录」。任务本身可以排队/运行到 execution_expires_after
// （默认 48h，上限 259200s = 72h），成功后 video_url 还有 24h 有效期——网关的记录若比
// 上游短（原先沿用 Grok 的 24h），上游还答得出的任务在网关这里先变成 404：调用方取不回
// 视频，网关也永远等不到计费的那次查询。
const seedanceTaskRetention = 7 * 24 * time.Hour

func (e GrokMediaEndpoint) IsSeedance() bool {
	return e == SeedanceEndpointCreate || e == SeedanceEndpointStatus || e == SeedanceEndpointDelete
}

// SeedanceTaskKey isolates ownership and billing keys from other video providers.
func SeedanceTaskKey(id string) string { return seedanceTaskKeyPrefix + strings.TrimSpace(id) }

// IsSeedanceTaskKey reports whether a gateway task key was built by SeedanceTaskKey.
func IsSeedanceTaskKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), seedanceTaskKeyPrefix)
}

// seedanceUpstreamTaskID 还原方舟侧的任务 ID（去掉网关命名空间）。
func seedanceUpstreamTaskID(key string) string {
	return strings.TrimPrefix(strings.TrimSpace(key), seedanceTaskKeyPrefix)
}

// errSeedanceCallbackUnsupported：方舟会把任务结果（与查询接口返回体一致，含 content.video_url
// 与 usage）直接 POST 给 callback_url。调用方因此不必再经网关查询，而网关只在查询看到
// succeeded 时按 usage.completion_tokens 计费——放行 callback_url 等于放行不计费的视频。
// 明确拒绝，而不是悄悄删掉字段：删掉的话调用方会一直等一个永远不来的回调。
var errSeedanceCallbackUnsupported = fmt.Errorf("callback_url is not supported by this gateway: poll GET /api/v3/contents/generations/tasks/{id} for the result instead")

func ParseSeedanceRequest(body []byte) (GrokMediaRequestInfo, error) {
	var info GrokMediaRequestInfo
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return info, fmt.Errorf("request body must be a JSON object")
	}
	model := gjson.GetBytes(body, "model")
	if model.Type != gjson.String || strings.TrimSpace(model.String()) == "" {
		return info, fmt.Errorf("model is required")
	}
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() || len(content.Array()) == 0 {
		return info, fmt.Errorf("content must be a non-empty array")
	}
	if callback := gjson.GetBytes(body, "callback_url"); callback.Exists() && callback.Type != gjson.Null &&
		strings.TrimSpace(callback.String()) != "" {
		return info, errSeedanceCallbackUnsupported
	}
	info.Model = strings.TrimSpace(model.String())
	var texts []string
	for _, item := range content.Array() {
		switch item.Get("type").String() {
		case "text":
			texts = append(texts, item.Get("text").String())
		case "image_url":
			info.InputImageURLs = append(info.InputImageURLs, item.Get("image_url.url").String())
		case "draft_task":
			// 基于样片（draft）生成正式视频：样片任务只存在于创建它的那个上游账号上。
			id := item.Get("draft_task.id")
			if id.Type != gjson.String || strings.TrimSpace(id.String()) == "" {
				return info, fmt.Errorf("draft_task.id is required")
			}
			info.ReferencedTaskKeys = append(info.ReferencedTaskKeys, SeedanceTaskKey(id.String()))
		}
	}
	info.Prompt = strings.Join(texts, "\n")
	return info, nil
}

func buildSeedanceURL(base string, endpoint GrokMediaEndpoint, taskID string) (string, error) {
	base = strings.TrimRight(base, "/")
	// Accept an origin, a proxy prefix, or the full Ark API base.
	if !strings.HasSuffix(base, "/api/v3") && !strings.HasSuffix(base, "/v3") {
		base += "/api/v3"
	}
	base += "/contents/generations/tasks"
	if endpoint != SeedanceEndpointCreate {
		if err := validateUpstreamPathSegment("Seedance task ID", taskID); err != nil || strings.TrimSpace(taskID) == "" {
			return "", fmt.Errorf("invalid Seedance task ID")
		}
		base += "/" + taskID
	}
	return base, nil
}

// ForwardSeedance preserves the Ark protocol, including multimodal content and
// future fields. Only model is rewritten using the account's configured mapping.
func (s *OpenAIGatewayService) ForwardSeedance(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, taskID string, body []byte) (*OpenAIForwardResult, error) {
	if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance) || !endpoint.IsSeedance() {
		return nil, fmt.Errorf("seedance requires an OpenAI API key account with a custom base URL")
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return nil, err
	}
	target, err := buildSeedanceURL(base, endpoint, seedanceUpstreamTaskID(taskID))
	if err != nil {
		return nil, err
	}
	model, upstreamModel := "", ""
	method := http.MethodGet
	switch endpoint {
	case SeedanceEndpointCreate:
		info, parseErr := ParseSeedanceRequest(body)
		if parseErr != nil {
			return nil, parseErr
		}
		model = info.Model
		upstreamModel = account.GetMappedModel(model)
		body, err = sjson.SetBytes(body, "model", upstreamModel)
		if err != nil {
			return nil, err
		}
		method = http.MethodPost
	case SeedanceEndpointDelete:
		method = http.MethodDelete
	}
	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		return nil, fmt.Errorf("seedance account missing api_key")
	}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(req.Header)
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	// Do not retry ambiguous asynchronous creates: the upstream may already have
	// accepted a billable job. Preserve native error codes and response bodies.
	if resp.StatusCode >= 300 {
		writeGrokMediaResponse(c, resp, responseBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("seedance upstream status %d", resp.StatusCode)
	}
	result := &OpenAIForwardResult{Model: model, BillingModel: model, UpstreamModel: upstreamModel, Duration: time.Since(started), ResponseHeaders: resp.Header.Clone()}
	if endpoint == SeedanceEndpointCreate {
		id := strings.TrimSpace(gjson.GetBytes(responseBody, "id").String())
		if id == "" {
			return nil, fmt.Errorf("seedance create response missing task ID")
		}
		result.ResponseID = SeedanceTaskKey(id)
	}
	if endpoint == SeedanceEndpointStatus {
		result.ResponseID = taskID
		result.UpstreamModel = gjson.GetBytes(responseBody, "model").String()
		if gjson.GetBytes(responseBody, "status").String() == "succeeded" {
			result.Usage.OutputTokens = max(0, int(gjson.GetBytes(responseBody, "usage.completion_tokens").Int()))
			// 开了 tools: web_search 的任务，实际联网搜索次数在 usage.tool_usage.web_search
			// （0 = 没搜）。与 Grok 原生搜索同口径：按分组的千次搜索单价叠加在 token 费用之上。
			result.SearchCount = max(0, int(gjson.GetBytes(responseBody, "usage.tool_usage.web_search").Int()))
		}
	}
	writeGrokMediaResponse(c, resp, responseBody, s.responseHeaderFilter)
	return result, nil
}
