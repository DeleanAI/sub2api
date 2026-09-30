package service

import (
	"net/http"
	"strings"
)

var upstreamModelNotFoundKeywords = []string{"model not found", "unknown model", "not found"}

func isUpstreamModelNotFoundError(statusCode int, body []byte) bool {
	if statusCode != http.StatusNotFound {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" || !strings.Contains(normalized, "model") {
		return false
	}
	return containsModelNotFoundKeyword(normalized)
}

// upstreamModelUnservedKind 判断上游这次回答是不是「服务不了该模型」，返回对应的冷却原因
// （见 upstreamModelUnservedReasons）；不是则返回空串。写模型冷却（HandleUpstreamModelNotFound，另按账号类型收窄）
// 与故障转移用尽时给客户端的回答（UpstreamModelUnservedClientError）共用这一个判定。
func upstreamModelUnservedKind(statusCode int, body []byte) string {
	switch {
	case isUpstreamModelNotFoundError(statusCode, body):
		return upstreamModelNotFoundReason
	case statusCode == http.StatusUnauthorized && isOpenAICompatibleModelNotFoundBody(body):
		return upstreamModelNotFound401Reason
	case isOpenAICodexPlanGatedModelError(statusCode, body):
		return upstreamCodexPlanGatedModelReason
	}
	return ""
}

// UpstreamModelUnservedClientError 给出故障转移用尽时应转给客户端的回答：最后一个上游回答若是「服务不了该模型」，
// 应答 404 model_not_found——官方 API 对不存在或无权使用的模型都回 404——而不是把它说成 502「上游失败」或
// 「上游认证失败」。冷却期内的后续请求由模型可用性诊断给出同样的 404（ModelAvailabilityDiagnosis.UpstreamUnservedUntil）。
func UpstreamModelUnservedClientError(statusCode int, body []byte) (status int, errType, message string, ok bool) {
	if upstreamModelUnservedKind(statusCode, body) == "" {
		return 0, "", "", false
	}
	return http.StatusNotFound, "model_not_found", "The requested model is not available in this group: the upstream reported it as unsupported", true
}

func isModelNotFoundError(statusCode int, body []byte) bool {
	return isUpstreamModelNotFoundError(statusCode, body) || statusCode == http.StatusNotFound
}

// openAICodexPlanGatedModelPhrase matches the deterministic Codex 400 returned
// when a ChatGPT OAuth account's plan cannot serve the requested model, e.g.
// {"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}
// The phrase is compared against the normalized body (lowercased, "_"/"-"
// folded to spaces), so it also matches the same message embedded in
// error.message-style payloads.
const openAICodexPlanGatedModelPhrase = "model is not supported when using codex"

// isOpenAICodexPlanGatedModelError reports whether the upstream response is the
// deterministic Codex rejection of a plan-gated model on a ChatGPT account.
// Unlike transient failures, retrying the same account cannot succeed until the
// account's plan changes, so callers should treat it like model-not-found and
// cool the (account, model) pair down instead of re-selecting the account.
func isOpenAICodexPlanGatedModelError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, openAICodexPlanGatedModelPhrase)
}

func containsModelNotFoundKeyword(normalizedBody string) bool {
	if normalizedBody == "" {
		return false
	}
	for _, keyword := range upstreamModelNotFoundKeywords {
		if strings.Contains(normalizedBody, keyword) {
			return true
		}
	}
	return false
}

func normalizeModelNotFoundBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	normalized := strings.ToLower(string(body))
	normalized = strings.NewReplacer("_", " ", "-", " ", "\n", " ", "\r", " ", "\t", " ").Replace(normalized)
	return strings.Join(strings.Fields(normalized), " ")
}
