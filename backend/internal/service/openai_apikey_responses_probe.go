package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/tidwall/gjson"
)

// openaiResponsesProbeTimeout 是探测请求的超时时长。
// 探测在后台 goroutine 中异步执行,不阻塞账号创建/更新;留出余量给推理型模型
// 先思考再产出 function_call 的往返。超时则保持 unknown,不下结论。
const openaiResponsesProbeTimeout = 15 * time.Second

// responsesProbeMaxBodyBytes 限制读取探测响应体的字节数,够判定 output 项类型即可。
const responsesProbeMaxBodyBytes = 256 * 1024

// openaiResponsesProbeMaxOutputTokens 是探测请求的输出预算。
// 推理型模型可能把预算全烧在 reasoning 上,还没轮到 function_call 就被截断——
// 那种响应不能用来判定工具能力,见 responsesProbeVerdictIsConclusive。
const openaiResponsesProbeMaxOutputTokens = 512

// openaiResponsesProbePayload 构造探测用的 Responses 请求体。
//
// 关键设计:请求携带一个工具并以 tool_choice=required 强制模型调用它。这样
// 一个真正支持 Responses 工具调用的上游必须在响应里产出 function_call 输出项;
// 而"端点存在、基础补全可用、但工具调用坏掉"的上游(如火山方舟 coding/v3 ×
// kimi-k2.6,只回 reasoning、不产出 function_call)会被这一步暴露出来。
//
// Stream=false 便于一次性读取 output 数组判定;不带 instructions 以免干扰。
func openaiResponsesProbePayload(modelID string) []byte {
	if strings.TrimSpace(modelID) == "" {
		modelID = openai.DefaultTestModel
	}
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"input": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "Call the probe_ping function with ok=true to acknowledge readiness. You must use the tool."},
				},
			},
		},
		"tools": []map[string]any{
			{
				"type":        "function",
				"name":        "probe_ping",
				"description": "Capability probe. Call to acknowledge.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ok": map[string]any{"type": "boolean"},
					},
					"required": []string{"ok"},
				},
			},
		},
		"tool_choice":       "required",
		"max_output_tokens": openaiResponsesProbeMaxOutputTokens,
		"stream":            false,
	})
	return body
}

// responsesProbeMaxModelAttempts 限制一次探测最多尝试多少个候选模型。
//
// 聚合型上游的 model_mapping 常有 20+ 条，而「模型不存在」可能连续命中好几个。
// 逐个试到底会把一次账号保存变成一串上游请求，因此设一个小上限：够跨过前几个不
// 存在的模型，又不至于把 model_mapping 当成压测清单。用尽仍无结论则保持 unknown。
const responsesProbeMaxModelAttempts = 4

// selectResponsesProbeModels 按优先级列出用于探测的上游模型候选。
//
// 工具能力探测必须用上游真实存在的模型——用占位模型(DefaultTestModel)打第三方
// 上游只会拿到 400 model-not-found,无从判定工具能力。取账号 model_mapping 的上游
// 模型(值)并按字典序排序以保证可复现;无映射时回退 DefaultTestModel(适配 OpenAI
// 官方 APIKey 账号)。
//
// 返回多个候选而非单个：字典序首个模型完全可能正是该上游不提供的那个。实测
// 2026-09-18：allincoding.cc 与 api.axis.fan 的映射里首个都是 gpt-5.4，而两家都不
// 提供它（回 404 model_not_found），但 gpt-5.5 / gpt-5.6-sol 的 /v1/responses 带
// 工具调用完全正常。只探首个模型会让探测拿不到任何工具能力证据，却仍对能力下结论。
//
// compact 专用模型(*-openai-compact)排到最后：compact 端点 schema 比 /responses 窄，
// 用它探工具能力不具代表性。只调整顺序、不剔除，避免只有 compact 映射的账号无候选。
func selectResponsesProbeModels(account *Account) []string {
	mapping := account.GetModelMapping()
	candidates := make([]string, 0, len(mapping))
	for _, upstream := range mapping {
		upstream = strings.TrimSpace(upstream)
		if upstream == "" || strings.Contains(upstream, "*") {
			continue
		}
		candidates = append(candidates, upstream)
	}
	if len(candidates) == 0 {
		return []string{openai.DefaultTestModel}
	}
	sort.Strings(candidates)
	sort.SliceStable(candidates, func(i, j int) bool {
		return !isOpenAICompactOnlyProbeModel(candidates[i]) && isOpenAICompactOnlyProbeModel(candidates[j])
	})
	if len(candidates) > responsesProbeMaxModelAttempts {
		candidates = candidates[:responsesProbeMaxModelAttempts]
	}
	return candidates
}

// isOpenAICompactOnlyProbeModel 判断模型是否只服务 compact 端点。
func isOpenAICompactOnlyProbeModel(model string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), "-openai-compact")
}

// selectResponsesProbeModel 返回首个探测候选，供只需单个模型的调用方使用。
func selectResponsesProbeModel(account *Account) string {
	return selectResponsesProbeModels(account)[0]
}

// ProbeOpenAIAPIKeyResponsesSupport 探测 OpenAI APIKey 账号上游是否支持
// /v1/responses 端点，并将结果持久化到 accounts.extra.openai_responses_supported。
//
// 调用时机：账号创建/更新后，且仅当 platform=openai && type=apikey 时。
//
// 探测策略（参见包文档 internal/pkg/openai_compat）：
//   - 上游 404 / 405 → 端点不存在,写 false
//   - 上游 2xx → 端点存在,进一步看工具能力:响应含 function_call 输出项才写 true;
//     仅 reasoning / 无 function_call(如火山方舟 coding/v3 × kimi-k2.6)写 false
//   - 其他非 2xx（401/422/400/5xx 等）→ 端点存在但无法判定工具能力,保守写 true
//   - 网络层失败（连接错误、超时）→ 不写标记，保持 unknown
//     （后续请求仍按"现状即证据"默认走 Responses）
//
// 该方法是幂等的：重复调用会以最新探测结果覆盖标记。
//
// 关于失败处理：探测本身的失败不应阻塞账号创建——账号能创建/更新成功就够了，
// 探测结果只影响后续路由优化。所有错误都仅记录日志，不向调用方传播。
func (s *AccountTestService) ProbeOpenAIAPIKeyResponsesSupport(ctx context.Context, accountID int64) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_load_account_failed: account_id=%d err=%v", accountID, err)
		return
	}
	if account.Type != AccountTypeAPIKey {
		return
	}
	if account.IsCNProvider() {
		// 国产 OpenAI 兼容上游默认仅支持 /v1/chat/completions。直接落标 false
		// 走 Chat Completions 直转，跳过网络探测。
		// 例外：deepseek / kimi 的固定 responses 和 adaptive 账号使用官方原生
		// Responses 端点，落标 force_responses；其余协议显式重置为 auto，避免
		// 切换后残留强制模式。
		if account.UsesNativeCNResponses() {
			_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
				openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeForceResponses),
				openai_compat.ExtraKeyResponsesSupported: true,
			})
			return
		}
		_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
			openai_compat.ExtraKeyResponsesSupported: false,
		})
		return
	}
	if account.Platform != PlatformOpenAI {
		// 仅 OpenAI APIKey 账号需要探测；其他账号类型无能力差异。
		return
	}

	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		logger.LegacyPrintf("service.openai_probe", "probe_skip_no_apikey: account_id=%d", accountID)
		return
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_invalid_baseurl: account_id=%d base_url=%q err=%v", accountID, baseURL, err)
		return
	}

	probeURL := buildOpenAIResponsesURL(normalizedBaseURL)

	// 逐个候选模型探测：模型级 404/405（上游不提供该模型）不构成端点能力证据，
	// 换下一个候选继续；端点级 404/405 与任何可下结论的响应都当场定论。
	var (
		attempted   []string
		lastAttempt *openAIResponsesProbeAttempt
	)
	for _, probeModel := range selectResponsesProbeModels(account) {
		attempt := s.doResponsesProbeAttempt(ctx, account, probeURL, apiKey, probeModel)
		attempted = append(attempted, probeModel)
		if attempt == nil {
			// 网络层失败 / 响应体读取失败：不写标记，保持 unknown。
			return
		}
		lastAttempt = attempt

		if _, modelScoped := responsesEndpointVerdictFromResponse(attempt.status, attempt.body); modelScoped {
			logger.LegacyPrintf("service.openai_probe",
				"probe_model_not_available_try_next: account_id=%d base_url=%s probe_model=%s status=%d",
				accountID, normalizedBaseURL, probeModel, attempt.status,
			)
			continue
		}
		if !responsesProbeVerdictIsConclusive(attempt.status, attempt.body) {
			// 本次响应不足以下结论时保持 unknown，与网络层失败、响应体读取失败一致：
			// 标记一旦写成 false 就会一直粘住（只有下次账号创建/更新才重探），网关会静默
			// 改走 /v1/chat/completions —— 对 Codex 客户端意味着 prompt 缓存前缀被打散。
			// 宁可不写，让请求继续走既有的 Responses 路径。
			logger.LegacyPrintf("service.openai_probe",
				"probe_inconclusive_keep_unknown: account_id=%d base_url=%s probe_model=%s status=%d response_status=%s reason=%s",
				accountID, normalizedBaseURL, probeModel, attempt.status,
				gjson.GetBytes(attempt.body, "status").String(),
				gjson.GetBytes(attempt.body, "incomplete_details.reason").String(),
			)
			return
		}

		s.persistResponsesProbeVerdict(ctx, account, normalizedBaseURL, probeModel, attempt)
		return
	}

	// 所有候选都是「上游不提供这个模型」：端点是活的，只是没有一个候选能用来验证
	// 工具能力。此时绝不能落标——保持 unknown 让请求继续走 Responses。
	status := 0
	if lastAttempt != nil {
		status = lastAttempt.status
	}
	logger.LegacyPrintf("service.openai_probe",
		"probe_all_models_unavailable_keep_unknown: account_id=%d base_url=%s tried=%s last_status=%d",
		accountID, normalizedBaseURL, strings.Join(attempted, ","), status,
	)
}

// openAIResponsesProbeAttempt 是一次探测请求的结果。
type openAIResponsesProbeAttempt struct {
	status int
	body   []byte
}

// doResponsesProbeAttempt 用指定模型打一次探测请求。
// 返回 nil 表示网络层失败或响应体读取失败——调用方应保持 unknown，不写标记。
func (s *AccountTestService) doResponsesProbeAttempt(
	ctx context.Context,
	account *Account,
	probeURL string,
	apiKey string,
	probeModel string,
) *openAIResponsesProbeAttempt {
	probeCtx, cancel := context.WithTimeout(ctx, openaiResponsesProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, probeURL, bytes.NewReader(openaiResponsesProbePayload(probeModel)))
	if err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_build_request_failed: account_id=%d err=%v", account.ID, err)
		return nil
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	applyOpenAICodexProbeHeaders(req.Header)

	// 账号级请求头覆写：能力探测与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		// 网络层失败：不写标记，保持 unknown，下次重试或由网关 fallback 处理
		logger.LegacyPrintf("service.openai_probe", "probe_request_failed: account_id=%d url=%s err=%v", account.ID, probeURL, err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, responsesProbeMaxBodyBytes))
	// 有界排空剩余响应体:既帮助连接复用,又避免行为异常的上游用超大响应体拖住探测。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responsesProbeMaxBodyBytes))
	if readErr != nil {
		// 响应体读取失败(部分读取/传输错误):按网络层失败处理,保持 unknown,
		// 不写标记——否则可能给一个 2xx 响应误写 supported=false。
		logger.LegacyPrintf("service.openai_probe", "probe_read_body_failed: account_id=%d url=%s err=%v", account.ID, probeURL, readErr)
		return nil
	}
	return &openAIResponsesProbeAttempt{status: resp.StatusCode, body: bodyBytes}
}

// persistResponsesProbeVerdict 落标一次可下结论的探测结果。
func (s *AccountTestService) persistResponsesProbeVerdict(
	ctx context.Context,
	account *Account,
	normalizedBaseURL string,
	probeModel string,
	attempt *openAIResponsesProbeAttempt,
) {
	supported := decideResponsesProbeSupport(attempt.status, attempt.body)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openai_compat.ExtraKeyResponsesSupported: supported,
	}); err != nil {
		logger.LegacyPrintf("service.openai_probe", "probe_persist_failed: account_id=%d supported=%v err=%v", account.ID, supported, err)
		return
	}

	if !supported {
		// 落标为不支持等于把该账号长期钉在 /v1/chat/completions 上，成本与缓存命中率
		// 都会变化，且不会自动恢复。这条必须能被运维看到（#5371）。
		slog.Warn(
			"openai_responses_probe_marked_unsupported",
			"account_id", account.ID,
			"account_name", account.Name,
			"base_url", normalizedBaseURL,
			"probe_model", probeModel,
			"upstream_status", attempt.status,
		)
	}

	logger.LegacyPrintf("service.openai_probe",
		"probe_done: account_id=%d base_url=%s probe_model=%s status=%d supported=%v",
		account.ID, normalizedBaseURL, probeModel, attempt.status, supported,
	)
}

// responsesProbeVerdictIsConclusive 判断本次探测响应是否足以对「上游是否支持带工具的
// Responses 调用」下结论。
//
// 2xx 分支靠「output 里有没有 function_call」下结论，但这只在响应真的跑完时成立：
//
//   - status=incomplete 且 incomplete_details.reason=max_output_tokens：探测请求自己
//     只给了 openaiResponsesProbeMaxOutputTokens 的预算，推理型模型可能把预算全烧在
//     reasoning 上，还没轮到 function_call 就被截断。此时「没有 function_call」是探测
//     预算不足造成的，不是上游能力缺失。
//   - status=failed：HTTP 200 携带的失败响应（上游瞬时故障）同样不构成能力证据。
//
// 其余 2xx 一律可下结论——尤其 status=completed 却只回 reasoning 的上游（火山方舟
// coding/v3 × kimi-k2.6），仍按原逻辑判为不支持。
//
// 非 2xx 的结论只看状态码、不依赖响应内容，恒可下结论。
// 缺少 status 字段的响应体（含非 JSON）也按可下结论处理，保持既有行为。
func responsesProbeVerdictIsConclusive(status int, body []byte) bool {
	if status < 200 || status >= 300 {
		return true
	}
	switch strings.TrimSpace(gjson.GetBytes(body, "status").String()) {
	case "failed":
		return false
	case "incomplete":
		return strings.TrimSpace(gjson.GetBytes(body, "incomplete_details.reason").String()) != "max_output_tokens"
	default:
		return true
	}
}

// isResponsesEndpointSupportedByStatus 根据探测响应的 HTTP 状态码判定上游
// 是否暴露 /v1/responses 端点。
//
// 关键观察：第三方 OpenAI 兼容上游（DeepSeek/Kimi 等）对未知端点统一返回 404
// 或 405；而 OpenAI 官方/有 Responses 实现的上游会因为请求体最简（缺字段）
// 返回 400/422 等业务错误，但端点本身存在。
//
// 因此：仅 404 和 405 视为"端点不存在"，其他 status 视为"端点存在"。
//
// 5xx 也视为"端点存在"——上游偶发故障不应误判为不支持。
//
// 注意：状态码本身分不清 404 指向端点还是指向模型。聚合型上游对不在其模型表里的
// 模型同样回 404（body 里 type=model_not_found），此时端点是活的。调用方必须先用
// isOpenAIUpstreamModelScoped404 把模型级错误摘出去，否则会把「这次选错了模型」
// 误判成「上游没有 Responses」——见 responsesProbeStatusIsModelScoped 的用法。
func isResponsesEndpointSupportedByStatus(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return false
	}
	return true
}

// openAIUpstreamModelScopedErrorMarkers 是「错误指向模型、而非指向端点」的判据词表。
//
// 这是一份声明式注册表：新增上游的措辞只需在此登记一次，探测落标（decide）与运行时
// 降级（forwardAsChatCompletions）两个入口自动一起生效，护栏测试也遍历本表。
// 词表只收「模型」维度的信号，不收鉴权/配额/限流——那些由各自的分支处理。
var openAIUpstreamModelScopedErrorMarkers = []string{
	// OpenAI 官方与绝大多数兼容层的规范错误类型/码。
	"model_not_found",
	"model_not_supported",
	"invalid_model",
	"unknown_model",
	"unsupported_model",
	// 聚合/中转类上游的自由文案（实测：allincoding.cc、api.axis.fan 回
	// `Model "gpt-5.4" is not supported by any configured account in this group`）。
	"is not supported by any configured account",
	"no configured account",
	"model does not exist",
	"model not exist",
	"does not exist or you do not have access",
	"no such model",
}

// isOpenAIUpstreamModelScoped404 判断一个 404/405 响应体是否在说「这个模型我没有」
// 而不是「这个端点我没有」。
//
// 为什么必须区分：聚合型上游（如 allincoding.cc / api.axis.fan）把多个后端账号拼成
// 一个 OpenAI 兼容面，对不在模型表里的模型回 404 + type=model_not_found。而探测选
// 模型是「按 model_mapping 字典序取首个」，选中的模型完全可能正是该上游不提供的那个
// （实测 2026-09-18：这两个上游都不提供 gpt-5.4，但 gpt-5.5 / gpt-5.6-sol 的
// /v1/responses 带工具调用完全正常）。把这种 404 当成端点缺失，会给账号永久落标
// openai_responses_supported=false，此后 Codex 客户端被静默降级到
// /v1/chat/completions —— 表现为「这个上游用 Codex 调不了工具」。
//
// 判据只看响应体的模型维度信号（见 openAIUpstreamModelScopedErrorMarkers），
// 不看状态码之外的其他条件：端点级 404 的 body 里不会出现这些词。
func isOpenAIUpstreamModelScoped404(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// 规范字段优先：error.type / error.code / 顶层 code 常直接就是判据词。
	candidates := []string{
		gjson.GetBytes(body, "error.type").String(),
		gjson.GetBytes(body, "error.code").String(),
		gjson.GetBytes(body, "code").String(),
		gjson.GetBytes(body, "type").String(),
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "message").String(),
	}
	for _, candidate := range candidates {
		if matchesOpenAIUpstreamModelScopedMarker(candidate) {
			return true
		}
	}
	// 非 JSON 或字段位置未知的上游：退回整体文本匹配，仍只认词表。
	return matchesOpenAIUpstreamModelScopedMarker(string(body))
}

func matchesOpenAIUpstreamModelScopedMarker(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return false
	}
	for _, marker := range openAIUpstreamModelScopedErrorMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// responsesEndpointVerdictFromResponse 是「上游是否暴露 Responses 端点」的唯一判据，
// 供探测落标与运行时降级共用（规则只写一处）。
//
// 返回 (endpointAbsent, modelScoped)：
//   - endpointAbsent=true  端点确实不存在，可据此落标 / 降级
//   - modelScoped=true     404/405 指向模型，端点是活的，本次不构成任何能力证据
func responsesEndpointVerdictFromResponse(status int, body []byte) (endpointAbsent bool, modelScoped bool) {
	if isResponsesEndpointSupportedByStatus(status) {
		return false, false
	}
	if isOpenAIUpstreamModelScoped404(body) {
		return false, true
	}
	return true, false
}

// decideResponsesProbeSupport 依据探测响应判定上游 /v1/responses 是否真正可用于
// 携带工具的请求。
//
//   - 404 / 405 且错误指向端点：端点不存在 → false
//   - 404 / 405 但错误指向模型（type=model_not_found 等）：端点存在、只是本次选错
//     模型 → 保守按 true。这类响应不含任何工具能力证据，调用方应优先换模型重探
//     （见 responsesProbeVerdictIsConclusive）；真落到这里也不能落 false。
//   - 其他非 2xx（401/403/422/5xx 等）：端点存在,但本次无法判定工具能力
//     （鉴权/校验/瞬时故障）→ 保守按 true,保持既有"端点存在即支持"行为
//   - 2xx：探测以 tool_choice=required 强制工具调用,响应必须含 function_call
//     输出项才算真正可用;否则(如火山方舟 coding/v3 × kimi-k2.6 仅回 reasoning)
//     判为 false,使网关改走 /v1/chat/completions 直转路径。
func decideResponsesProbeSupport(status int, body []byte) bool {
	if endpointAbsent, _ := responsesEndpointVerdictFromResponse(status, body); endpointAbsent {
		return false
	}
	if status < 200 || status >= 300 {
		return true
	}
	return responsesProbeBodyHasFunctionCall(body)
}

// responsesProbeBodyHasFunctionCall 判断非流式 Responses 响应体的 output 数组里
// 是否存在 function_call 输出项。
func responsesProbeBodyHasFunctionCall(body []byte) bool {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return false
	}
	for _, item := range output.Array() {
		if strings.TrimSpace(item.Get("type").String()) == "function_call" {
			return true
		}
	}
	return false
}
