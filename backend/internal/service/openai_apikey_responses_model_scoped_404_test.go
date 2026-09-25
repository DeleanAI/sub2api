package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 模型级 404 的护栏：判据遍历 openAIUpstreamModelScopedErrorMarkers 这份声明，
// 而不是点名今天遇到的那几个上游文案。新增一条判据词当天就被本测试覆盖。
func TestModelScopedMarkersAreAllRecognized(t *testing.T) {
	require.NotEmpty(t, openAIUpstreamModelScopedErrorMarkers, "词表为空会让模型级 404 判定整体失效")

	for _, marker := range openAIUpstreamModelScopedErrorMarkers {
		marker := marker
		t.Run(marker, func(t *testing.T) {
			// 词表每一项都必须在每个承载位置上被识别。位置集合本身也是声明：
			// isOpenAIUpstreamModelScoped404 查这些字段，测试就覆盖这些字段。
			carriers := []struct {
				name string
				body []byte
			}{
				{"error.type", mustMarshalJSON(map[string]any{"error": map[string]any{"type": marker}})},
				{"error.code", mustMarshalJSON(map[string]any{"error": map[string]any{"code": marker}})},
				{"error.message", mustMarshalJSON(map[string]any{"error": map[string]any{"message": "Model \"gpt-x\" " + marker}})},
				{"top-level code", mustMarshalJSON(map[string]any{"code": marker})},
				{"top-level message", mustMarshalJSON(map[string]any{"message": marker})},
				{"non-json body", []byte("upstream says: " + marker)},
				{"uppercased", mustMarshalJSON(map[string]any{"error": map[string]any{"type": strings.ToUpper(marker)}})},
			}
			for _, carrier := range carriers {
				require.Truef(t, isOpenAIUpstreamModelScoped404(carrier.body),
					"%s 位置上的 %q 未被识别为模型级错误", carrier.name, marker)

				// 端点判据必须据此认定「端点存在、本次为模型级」，对 404 与 405 都成立。
				for _, status := range []int{404, 405} {
					endpointAbsent, modelScoped := responsesEndpointVerdictFromResponse(status, carrier.body)
					require.Falsef(t, endpointAbsent,
						"status=%d %s 位置的 %q 被误判为端点缺失", status, carrier.name, marker)
					require.Truef(t, modelScoped,
						"status=%d %s 位置的 %q 未被判为模型级", status, carrier.name, marker)
				}

				// 落标判据绝不能因为模型级 404 写出 false —— 那正是账号被永久钉死在
				// /v1/chat/completions 的原因。
				require.Truef(t, decideResponsesProbeSupport(404, carrier.body),
					"%s 位置的 %q 导致落标 supported=false", carrier.name, marker)
			}
		})
	}
}

// 端点级 404/405（body 里没有任何模型维度信号）必须仍然判为端点缺失，
// 否则本次修复会把「上游真的没有 /v1/responses」也一起放过。
func TestEndpointScoped404StillMarksUnsupported(t *testing.T) {
	endpointBodies := [][]byte{
		nil,
		[]byte(``),
		[]byte(`Not Found`),
		[]byte(`<html><head><title>404 Not Found</title></head></html>`),
		mustMarshalJSON(map[string]any{"error": map[string]any{"message": "Unrecognized request URL"}}),
		mustMarshalJSON(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Unknown endpoint"}}),
		mustMarshalJSON(map[string]any{"detail": "Not Found"}),
	}
	for i, body := range endpointBodies {
		t.Run(fmt.Sprintf("body_%d", i), func(t *testing.T) {
			for _, status := range []int{404, 405} {
				endpointAbsent, modelScoped := responsesEndpointVerdictFromResponse(status, body)
				require.Truef(t, endpointAbsent, "status=%d 端点级响应未判为端点缺失: %s", status, body)
				require.False(t, modelScoped, "端点级响应不应判为模型级")
				require.Falsef(t, decideResponsesProbeSupport(status, body),
					"status=%d 端点级 404/405 应落标 supported=false", status)
			}
		})
	}
}

// 非 404/405 一律不进模型级判定：它们的处置由既有分支决定（保守 true / 看 function_call）。
func TestNonNotFoundStatusesNeverModelScoped(t *testing.T) {
	modelScopedBody := mustMarshalJSON(map[string]any{"error": map[string]any{"type": "model_not_found"}})
	for _, status := range []int{200, 400, 401, 403, 422, 429, 500, 503} {
		endpointAbsent, modelScoped := responsesEndpointVerdictFromResponse(status, modelScopedBody)
		require.Falsef(t, endpointAbsent, "status=%d 不应判为端点缺失", status)
		require.Falsef(t, modelScoped, "status=%d 不应进入模型级判定", status)
	}
}

// 实测复现：allincoding.cc / api.axis.fan 对 gpt-5.4 回的就是这个响应体，
// 而它们的 /v1/responses 带工具调用完全正常（2026-09-18 实测）。
func TestRealWorldAggregatorModelNotFoundBody(t *testing.T) {
	body := []byte(`{"error":{"message":"Model \"gpt-5.4\" is not supported by any configured account in this group","type":"model_not_found"}}`)
	endpointAbsent, modelScoped := responsesEndpointVerdictFromResponse(404, body)
	require.False(t, endpointAbsent)
	require.True(t, modelScoped)
	require.True(t, decideResponsesProbeSupport(404, body))
}

// 候选模型：compact 专用模型必须排在普通模型之后，且候选有上限。
func TestSelectResponsesProbeModelsOrderAndCap(t *testing.T) {
	// 实测账号的真实映射：字典序首个是 gpt-5.4（该上游不提供），
	// 修复后仍从它开始，但后面必须跟着可用候选。
	acct := &Account{Credentials: map[string]any{
		"model_mapping": map[string]any{
			"a": "gpt-5.4",
			"b": "gpt-5.4-mini",
			"c": "gpt-5.4-openai-compact",
			"d": "gpt-5.5",
			"e": "gpt-5.6-sol",
			"f": "gpt-6-astra",
		},
	}}
	models := selectResponsesProbeModels(acct)
	require.LessOrEqual(t, len(models), responsesProbeMaxModelAttempts)
	require.Greater(t, len(models), 1, "必须有多个候选，否则模型级 404 无从重试")
	for _, m := range models {
		require.False(t, isOpenAICompactOnlyProbeModel(m),
			"普通模型足够时不应把 compact 专用模型排进候选: %s", m)
	}

	// 只有 compact 映射时仍须给出候选，不能返回空。
	compactOnly := &Account{Credentials: map[string]any{
		"model_mapping": map[string]any{"a": "gpt-5.5-openai-compact"},
	}}
	require.Equal(t, []string{"gpt-5.5-openai-compact"}, selectResponsesProbeModels(compactOnly))

	// selectResponsesProbeModel 与候选列表首项保持一致（同一判据的单值视图）。
	require.Equal(t, selectResponsesProbeModels(acct)[0], selectResponsesProbeModel(acct))
}

func mustMarshalJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
