//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 遍历可配置能力的每个非空子集：批量编辑写下去的值，必须让账号恰好具备所选的能力。
// 走声明而不是点名——新增一种可配置能力，这里当天就覆盖到（Seedance 曾因批量校验只认
// chat_completions / embeddings 而一律 400）。
func TestBulkOpenAIEndpointCapabilitiesStoreExactlyTheSelection(t *testing.T) {
	all := ConfigurableOpenAIEndpointCapabilities
	for mask := 1; mask < 1<<len(all); mask++ {
		var selection []any
		want := map[OpenAIEndpointCapability]bool{}
		for i, capability := range all {
			if mask&(1<<i) != 0 {
				selection = append(selection, string(capability))
				want[capability] = true
			}
		}
		stored, includeChat, err := normalizeBulkOpenAIEndpointCapabilities(selection)
		require.NoError(t, err, selection)
		require.Equal(t, want[OpenAIEndpointCapabilityChatCompletions], includeChat, selection)

		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
			"api_key": "sk-test", "base_url": "https://ark.example.com/api/v3",
		}}
		if stored != nil {
			account.Credentials[openAIEndpointCapabilitiesCredentialKey] = stored
		}
		for _, capability := range all {
			require.Equal(t, want[capability], account.SupportsOpenAIEndpointCapability(capability),
				"selection %v stored %v: capability %s", selection, stored, capability)
		}
	}

	_, _, err := normalizeBulkOpenAIEndpointCapabilities([]any{"responses"})
	require.Error(t, err, "不可配置的能力必须被拒绝")
}
