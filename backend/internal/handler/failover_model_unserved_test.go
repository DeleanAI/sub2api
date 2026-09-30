//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 故障转移用尽时最后一个上游回答是「模型不支持」：三个入口（OpenAI 兼容、Anthropic、Gemini）都回 404，
// 不再是 502「Upstream request failed」；其他上游错误照旧走默认映射。
func TestFailoverExhaustedRelaysUpstreamModelUnservedAs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	unserved := &service.UpstreamFailoverError{StatusCode: http.StatusNotFound,
		// 线上原文（toooooken 2026-09-30，上游是另一个 sub2api）。
		ResponseBody: []byte(`{"error":{"message":"Model \"gpt-5.4\" is not supported by any configured account in this group","type":"model_not_found"}}`)}
	outage := &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway, ResponseBody: []byte(`{"error":{"message":"bad gateway"}}`)}
	mappers := map[string]func(c *gin.Context, err *service.UpstreamFailoverError){
		"openai": func(c *gin.Context, err *service.UpstreamFailoverError) {
			(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, err, false)
		},
		"anthropic": func(c *gin.Context, err *service.UpstreamFailoverError) {
			(&GatewayHandler{}).handleFailoverExhausted(c, err, service.PlatformAnthropic, false)
		},
		"gemini": func(c *gin.Context, err *service.UpstreamFailoverError) {
			(&GatewayHandler{}).handleGeminiFailoverExhausted(c, err)
		},
	}
	for name, mapper := range mappers {
		for _, tc := range []struct {
			err    *service.UpstreamFailoverError
			status int
		}{{unserved, http.StatusNotFound}, {outage, http.StatusBadGateway}} {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			mapper(c, tc.err)
			require.Equal(t, tc.status, w.Code, "%s %d: %s", name, tc.err.StatusCode, w.Body.String())
			if tc.status == http.StatusNotFound {
				var body map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
				require.Contains(t, w.Body.String(), "upstream reported it as unsupported", name)
			}
		}
	}
}
