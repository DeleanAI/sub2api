package config

import (
	"errors"
	"fmt"
	"strings"
)

// 飞书（Lark）OAuth 登录配置校验。
//
// 这是唯一一处飞书配置规则：启动时 Config.Validate 用它校验 env/config.yaml，
// SettingService.GetFeishuConnectOAuthConfig 用它校验合并了后台覆盖之后的最终配置，
// 后台 PUT /admin/settings 同样用它校验写入值。三处共用，规则不会分叉。
var (
	ErrFeishuAppIDRequired            = errors.New("feishu: app_id is required when enabled")
	ErrFeishuAppSecretRequired        = errors.New("feishu: app_secret is required when enabled")
	ErrFeishuBypassRequiresTenantKeys = errors.New("feishu: bypass_registration requires allowed_tenant_keys (otherwise any Feishu account could register while public registration is closed)")
)

// ValidateFeishuConfig 校验飞书登录配置。Enabled=false 时不校验任何字段。
func ValidateFeishuConfig(cfg FeishuConnectConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.AppID) == "" {
		return ErrFeishuAppIDRequired
	}
	if strings.TrimSpace(cfg.AppSecret) == "" {
		return ErrFeishuAppSecretRequired
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{"authorize_url", cfg.AuthorizeURL},
		{"token_url", cfg.TokenURL},
		{"userinfo_url", cfg.UserInfoURL},
	} {
		if err := ValidateAbsoluteHTTPURL(item.value); err != nil {
			return fmt.Errorf("feishu: %s invalid: %w", item.name, err)
		}
	}
	// redirect_url 允许为空：为空时按站点 api_base_url / 请求 Host 推导（见 handler.resolveOAuthAbsoluteCallbackURL）。
	if strings.TrimSpace(cfg.RedirectURL) != "" {
		if err := ValidateAbsoluteHTTPURL(cfg.RedirectURL); err != nil {
			return fmt.Errorf("feishu: redirect_url invalid: %w", err)
		}
	}
	// frontend_redirect_url 只能是站内路径。共用的 ValidateFrontendRedirectURL 也接受
	// 绝对 URL（历史 provider 需要跨站回跳），但飞书这条链上它只是本站的一个前端路由：
	// 允许绝对地址等于给自己留了一个"配错就变成开放重定向"的口子，登录结果会被带到
	// 配置里写的任何站点去。
	if err := ValidateFrontendRedirectURL(cfg.FrontendRedirectURL); err != nil {
		return fmt.Errorf("feishu: frontend_redirect_url invalid: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(cfg.FrontendRedirectURL), "/") {
		return fmt.Errorf("feishu: frontend_redirect_url invalid: must be a site-relative path such as /auth/feishu/callback")
	}
	if cfg.BypassRegistration && len(cfg.AllowedTenantKeyList()) == 0 {
		return ErrFeishuBypassRequiresTenantKeys
	}
	return nil
}

// AllowedTenantKeyList 把逗号分隔的 allowed_tenant_keys 解析成去空白、去重后的列表；空串 → 空列表（不限租户）。
func (c FeishuConnectConfig) AllowedTenantKeyList() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, raw := range strings.Split(c.AllowedTenantKeys, ",") {
		key := strings.TrimSpace(raw)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// TenantAllowed 是租户限制的唯一判定点：登录、注册、绑定都经过 OAuth 回调，回调只调用这一处。
// allowed_tenant_keys 为空 = 不限租户；非空时 tenant_key 必须精确命中其中之一
// （飞书 tenant_key 是不透明标识，不做大小写折叠）。
func (c FeishuConnectConfig) TenantAllowed(tenantKey string) bool {
	allowed := c.AllowedTenantKeyList()
	if len(allowed) == 0 {
		return true
	}
	tenantKey = strings.TrimSpace(tenantKey)
	if tenantKey == "" {
		return false
	}
	for _, key := range allowed {
		if key == tenantKey {
			return true
		}
	}
	return false
}
