package middleware

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedactPayloadRedactsNestedSensitiveJSONValues(t *testing.T) {
	input := `{"model":"gpt-test","nested":{"api_key":"secret-key","items":[{"refresh_token":"refresh-secret"}]},"authorization":"Bearer token"}`

	got := redactPayload(input)

	for _, secret := range []string{"secret-key", "refresh-secret", "Bearer token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted payload still contains %q: %s", secret, got)
		}
	}
	if strings.Count(got, "[REDACTED]") != 3 {
		t.Fatalf("redacted payload = %s, want three redactions", got)
	}
}

func TestRedactPayloadStoresCompactJSON(t *testing.T) {
	got := redactPayload("{\n  \"model\": \"gpt-test\",\n  \"messages\": []\n}")
	if got != `{"messages":[],"model":"gpt-test"}` {
		t.Fatalf("redactPayload() = %q, want compact JSON", got)
	}
}

func TestSanitizeQueryRedactsSensitiveValues(t *testing.T) {
	got := sanitizeQuery(url.Values{
		"cursor":      {"page-2"},
		"api_key":     {"should-not-appear"},
		"accessToken": {"also-secret"},
	})

	if got["cursor"][0] != "page-2" {
		t.Fatalf("cursor = %q, want preserved value", got["cursor"][0])
	}
	for _, key := range []string{"api_key", "accessToken"} {
		if got[key][0] != "[REDACTED]" {
			t.Fatalf("%s = %q, want redacted", key, got[key][0])
		}
	}
}

// TestSensitiveQueryParamsAreRedacted 钉死"凭据形态的查询参数不得明文入库"。
//
// 这条护栏是必要的：Gemini / Google AI Studio 把 API Key 放在查询串里（?key=AIza…），
// 而原来的名单里没有 key，后缀规则也匹配不上——密钥会明文进 metadata.query，
// 并由用户端和管理端两个接口原样返回。
func TestSensitiveQueryParamsAreRedacted(t *testing.T) {
	mustRedact := []string{
		"key", "Key", "KEY", "api_key", "api-key", "apiKey",
		"access_key", "secret_key", "session_key", "private_key",
		"authorization", "token", "access_token", "refresh_token",
		"secret", "client_secret", "password", "passwd", "pwd",
		"signature", "sign", "auth", "cookie",
	}
	for _, name := range mustRedact {
		if !isSensitiveField(name) {
			t.Fatalf("%q 必须被打码：它承载凭据", name)
		}
	}

	// 正常参数不能被误伤，否则审计记录失去排障价值。
	for _, name := range []string{"model", "stream", "page", "limit", "user_id", "request_id", "monkey", "keyboard"} {
		if isSensitiveField(name) {
			t.Fatalf("%q 不是凭据，不该被打码", name)
		}
	}

	out := sanitizeQuery(url.Values{"key": {"AIzaSyRealLookingSecretValue"}, "model": {"gemini-3-pro"}})
	if len(out["key"]) != 1 || out["key"][0] != "[REDACTED]" {
		t.Fatalf("?key= 未被打码：%v", out["key"])
	}
	if len(out["model"]) != 1 || out["model"][0] != "gemini-3-pro" {
		t.Fatalf("正常参数被改坏了：%v", out["model"])
	}
}
