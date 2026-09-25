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

// seedanceUpstreamCall 执行一次方舟任务接口调用。地址、鉴权、代理与读响应体只写这一处，
// 请求路径（ForwardSeedance）与后台结算（querySeedanceTask）共用；c 为 nil 时不记 ops 指标。
func (s *OpenAIGatewayService) seedanceUpstreamCall(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, taskKey string, body []byte) (*http.Response, []byte, time.Duration, error) {
	if !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance) || !endpoint.IsSeedance() {
		return nil, nil, 0, fmt.Errorf("seedance requires an OpenAI API key account with a custom base URL")
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return nil, nil, 0, err
	}
	target, err := buildSeedanceURL(base, endpoint, seedanceUpstreamTaskID(taskKey))
	if err != nil {
		return nil, nil, 0, err
	}
	method := http.MethodGet
	switch endpoint {
	case SeedanceEndpointCreate:
		method = http.MethodPost
	case SeedanceEndpointDelete:
		method = http.MethodDelete
	}
	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		return nil, nil, 0, fmt.Errorf("seedance account missing api_key")
	}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, err
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
	elapsed := time.Since(started)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, elapsed.Milliseconds())
	if err != nil {
		return nil, nil, elapsed, err
	}
	defer func() { _ = resp.Body.Close() }()
	var onTooLarge TooLargeWriter
	if c != nil {
		onTooLarge = openAITooLargeError
	}
	responseBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, onTooLarge)
	if err != nil {
		return nil, nil, elapsed, err
	}
	return resp, responseBody, elapsed, nil
}

// seedanceTaskStatus 是一次状态查询里网关计费关心的部分；响应本身原样透传给调用方。
type seedanceTaskStatus struct {
	Status           string
	Model            string
	CompletionTokens int
}

func parseSeedanceTaskStatus(body []byte) seedanceTaskStatus {
	return seedanceTaskStatus{
		Status:           gjson.GetBytes(body, "status").String(),
		Model:            gjson.GetBytes(body, "model").String(),
		CompletionTokens: max(0, int(gjson.GetBytes(body, "usage.completion_tokens").Int())),
	}
}

// seedanceTaskTerminal：方舟任务的终态（官方：succeeded / failed / cancelled / expired），此后状态不再变化。
func seedanceTaskTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "expired":
		return true
	}
	return false
}

// ForwardSeedance preserves the Ark protocol, including multimodal content and
// future fields. Only model is rewritten using the account's configured mapping.
func (s *OpenAIGatewayService) ForwardSeedance(ctx context.Context, c *gin.Context, account *Account, endpoint GrokMediaEndpoint, taskID string, body []byte) (*OpenAIForwardResult, error) {
	model, upstreamModel := "", ""
	if endpoint == SeedanceEndpointCreate {
		info, parseErr := ParseSeedanceRequest(body)
		if parseErr != nil {
			return nil, parseErr
		}
		model = info.Model
		upstreamModel = account.GetMappedModel(model)
		var err error
		body, err = sjson.SetBytes(body, "model", upstreamModel)
		if err != nil {
			return nil, err
		}
	}
	resp, responseBody, elapsed, err := s.seedanceUpstreamCall(ctx, c, account, endpoint, taskID, body)
	if err != nil {
		return nil, err
	}
	// Do not retry ambiguous asynchronous creates: the upstream may already have
	// accepted a billable job. Preserve native error codes and response bodies.
	if resp.StatusCode >= 300 {
		writeGrokMediaResponse(c, resp, responseBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("seedance upstream status %d", resp.StatusCode)
	}
	result := &OpenAIForwardResult{Model: model, BillingModel: model, UpstreamModel: upstreamModel, Duration: elapsed, ResponseHeaders: resp.Header.Clone()}
	if endpoint == SeedanceEndpointCreate {
		id := strings.TrimSpace(gjson.GetBytes(responseBody, "id").String())
		if id == "" {
			return nil, fmt.Errorf("seedance create response missing task ID")
		}
		result.ResponseID = SeedanceTaskKey(id)
	}
	if endpoint == SeedanceEndpointStatus {
		status := parseSeedanceTaskStatus(responseBody)
		result.ResponseID = taskID
		result.UpstreamModel = status.Model
		result.UpstreamTaskStatus = status.Status
		if status.Status == "succeeded" {
			result.Usage.OutputTokens = status.CompletionTokens
		}
	}
	writeGrokMediaResponse(c, resp, responseBody, s.responseHeaderFilter)
	return result, nil
}

// querySeedanceTask 供后台结算查询任务状态：与 ForwardSeedance 同一次上游调用，但不写任何 HTTP 响应。
func (s *OpenAIGatewayService) querySeedanceTask(ctx context.Context, account *Account, taskKey string) (int, seedanceTaskStatus, error) {
	resp, body, _, err := s.seedanceUpstreamCall(ctx, nil, account, SeedanceEndpointStatus, taskKey, nil)
	if err != nil {
		return 0, seedanceTaskStatus{}, err
	}
	return resp.StatusCode, parseSeedanceTaskStatus(body), nil
}
