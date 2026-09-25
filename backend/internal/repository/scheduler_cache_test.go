package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsSubscriptionPlanType(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"plan_type":     "plus",
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
	})

	require.Equal(t, "plus", filtered["plan_type"])
	require.NotContains(t, filtered, "access_token")
	require.NotContains(t, filtered, "refresh_token")
}

func TestSchedulerMetadataAccountKeepsOpenAISubscriptionIdentity(t *testing.T) {
	account := service.Account{
		ID:       24,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type":    "plus",
			"access_token": "secret-access-token",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.True(t, metadata.IsOpenAIChatGPTSubscription())
	require.Empty(t, metadata.GetCredential("access_token"))
}

// 调度快照的投影必须保留端点能力判定要读的一切：预筛选按投影判，判错了就进不了后面按完整账号的那一步。
// 遍历声明——会进快照的每个平台 × API Key / OAuth × 能力配置（未配置、每种可配置能力单独开启、全部
// 开启）——比较投影前后对每种可配置能力的判定。新增平台或可配置能力当天就被覆盖。Seedance 账号曾因
// 投影丢了 openai_capabilities 与 base_url 而永远选不上（503 No eligible Seedance accounts）。
func TestSchedulerMetadataProjectionPreservesEndpointCapabilityDecisions(t *testing.T) {
	configs := [][]any{nil}
	all := make([]any, 0, len(service.ConfigurableOpenAIEndpointCapabilities))
	for _, capability := range service.ConfigurableOpenAIEndpointCapabilities {
		configs = append(configs, []any{string(capability)})
		all = append(all, string(capability))
	}
	configs = append(configs, all)

	for _, platform := range service.SchedulerSnapshotPlatforms() {
		for _, accountType := range []string{service.AccountTypeAPIKey, service.AccountTypeOAuth} {
			for _, config := range configs {
				account := service.Account{ID: 31, Platform: platform, Type: accountType, Status: service.StatusActive, Schedulable: true,
					Credentials: map[string]any{
						"api_key":       "sk-test",
						"access_token":  "at-test",
						"base_url":      "https://upstream.example.com/api/v3",
						"model_mapping": map[string]any{"m": "m"},
					}}
				if config != nil {
					account.Credentials["openai_capabilities"] = config
				}
				meta := buildSchedulerMetadataAccount(account)
				for _, capability := range service.ConfigurableOpenAIEndpointCapabilities {
					require.Equal(t, account.SupportsOpenAIEndpointCapability(capability), meta.SupportsOpenAIEndpointCapability(capability),
						"%s/%s openai_capabilities=%v capability=%s", platform, accountType, config, capability)
				}
				require.Equal(t, account.IsModelSupported("m"), meta.IsModelSupported("m"))
				require.Empty(t, meta.GetCredential("access_token"), "密钥类凭据不进快照投影")
			}
		}
	}
}

func TestSchedulerMetadataAccountProjectsUpstreamBillingProbe(t *testing.T) {
	lastError := strings.Repeat("upstream diagnostic ", 512)
	probe := map[string]any{
		"status": "ok",
		"data": map[string]any{
			"billing_scope":             "token",
			"resolved_rate_multiplier":  0.03,
			"peak_rate_enabled":         true,
			"peak_start":                "09:00",
			"peak_end":                  "18:00",
			"peak_rate_multiplier":      2.0,
			"timezone":                  "Asia/Shanghai",
			"effective_rate_multiplier": 0.03,
			"remote_diagnostic":         lastError,
		},
		"received_at":   "2026-07-13T10:00:00Z",
		"fresh_until":   "2026-07-13T11:00:00Z",
		"next_probe_at": "2026-07-13T10:30:00Z",
		"http_status":   502,
		"last_error":    lastError,
	}
	account := service.Account{
		ID: 42,
		Extra: map[string]any{
			"upstream_billing_probe": probe,
			"unused_large_field":     "drop-me",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	filtered, ok := metadata.Extra["upstream_billing_probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", filtered["status"])
	require.Equal(t, "2026-07-13T10:00:00Z", filtered["received_at"])
	require.Equal(t, "2026-07-13T11:00:00Z", filtered["fresh_until"])
	require.Equal(t, "2026-07-13T10:30:00Z", filtered["next_probe_at"])
	require.NotContains(t, filtered, "http_status")
	require.NotContains(t, filtered, "last_error")
	filteredData, ok := filtered["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", filteredData["billing_scope"])
	require.Equal(t, 0.03, filteredData["resolved_rate_multiplier"])
	require.Equal(t, true, filteredData["peak_rate_enabled"])
	require.Equal(t, "09:00", filteredData["peak_start"])
	require.Equal(t, "18:00", filteredData["peak_end"])
	require.Equal(t, 2.0, filteredData["peak_rate_multiplier"])
	require.Equal(t, "Asia/Shanghai", filteredData["timezone"])
	require.NotContains(t, filteredData, "effective_rate_multiplier")
	require.NotContains(t, filteredData, "remote_diagnostic")
	require.NotContains(t, metadata.Extra, "unused_large_field")
	require.Contains(t, string(fullPayload), lastError)
	require.NotContains(t, string(metaPayload), "last_error")
	require.Less(t, len(metaPayload)*4, len(fullPayload))
}

func TestSchedulerMetadataAccountDropsInvalidUpstreamBillingProbe(t *testing.T) {
	for _, probe := range []any{
		"invalid",
		map[string]any{},
		map[string]any{"status": ""},
	} {
		metadata := buildSchedulerMetadataAccount(service.Account{
			Extra: map[string]any{service.UpstreamBillingProbeExtraKey: probe},
		})

		require.NotContains(t, metadata.Extra, service.UpstreamBillingProbeExtraKey)
	}
}
