package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validFeishuConfig() FeishuConnectConfig {
	return FeishuConnectConfig{
		Enabled:             true,
		AppID:               "cli_a1b2c3",
		AppSecret:           "secret",
		AuthorizeURL:        "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
		TokenURL:            "https://accounts.feishu.cn/oauth/v3/token",
		UserInfoURL:         "https://open.feishu.cn/open-apis/authen/v1/user_info",
		FrontendRedirectURL: "/auth/feishu/callback",
	}
}

func TestValidateFeishuConfig_Disabled_Skip(t *testing.T) {
	require.NoError(t, ValidateFeishuConfig(FeishuConnectConfig{Enabled: false}))
}

func TestValidateFeishuConfig_HappyPath(t *testing.T) {
	require.NoError(t, ValidateFeishuConfig(validFeishuConfig()))
}

func TestValidateFeishuConfig_RequiresCredentials(t *testing.T) {
	cfg := validFeishuConfig()
	cfg.AppID = " "
	require.ErrorIs(t, ValidateFeishuConfig(cfg), ErrFeishuAppIDRequired)

	cfg = validFeishuConfig()
	cfg.AppSecret = ""
	require.ErrorIs(t, ValidateFeishuConfig(cfg), ErrFeishuAppSecretRequired)
}

func TestValidateFeishuConfig_URLs(t *testing.T) {
	cfg := validFeishuConfig()
	cfg.TokenURL = "accounts.feishu.cn/oauth/v3/token"
	require.ErrorContains(t, ValidateFeishuConfig(cfg), "token_url")

	cfg = validFeishuConfig()
	cfg.RedirectURL = "/api/v1/auth/oauth/feishu/callback"
	require.ErrorContains(t, ValidateFeishuConfig(cfg), "redirect_url")

	// 空 redirect_url 合法：运行时按站点地址推导。
	cfg = validFeishuConfig()
	cfg.RedirectURL = ""
	require.NoError(t, ValidateFeishuConfig(cfg))

	cfg = validFeishuConfig()
	cfg.FrontendRedirectURL = "https://evil.example.com/callback"
	require.ErrorContains(t, ValidateFeishuConfig(cfg), "frontend_redirect_url")
}

func TestValidateFeishuConfig_BypassRequiresTenantKeys(t *testing.T) {
	cfg := validFeishuConfig()
	cfg.BypassRegistration = true
	require.ErrorIs(t, ValidateFeishuConfig(cfg), ErrFeishuBypassRequiresTenantKeys)

	cfg.AllowedTenantKeys = " , tenant_a ,"
	require.NoError(t, ValidateFeishuConfig(cfg))
}

func TestFeishuConnectConfig_AllowedTenantKeyList(t *testing.T) {
	cfg := FeishuConnectConfig{AllowedTenantKeys: " a, b ,, a ,c"}
	require.Equal(t, []string{"a", "b", "c"}, cfg.AllowedTenantKeyList())
	require.Empty(t, FeishuConnectConfig{AllowedTenantKeys: " , "}.AllowedTenantKeyList())
}

func TestFeishuConnectConfig_TenantAllowed(t *testing.T) {
	unrestricted := FeishuConnectConfig{}
	require.True(t, unrestricted.TenantAllowed("anything"))
	require.True(t, unrestricted.TenantAllowed(""))

	restricted := FeishuConnectConfig{AllowedTenantKeys: "tenant_a,tenant_b"}
	require.True(t, restricted.TenantAllowed("tenant_a"))
	require.True(t, restricted.TenantAllowed(" tenant_b "))
	require.False(t, restricted.TenantAllowed("tenant_c"))
	require.False(t, restricted.TenantAllowed("TENANT_A"), "tenant_key is opaque, no case folding")
	require.False(t, restricted.TenantAllowed(""), "restricted deployments must not accept a missing tenant_key")
}
