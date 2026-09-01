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
