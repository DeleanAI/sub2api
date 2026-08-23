//go:build unit

package service

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/schema"

	"github.com/stretchr/testify/require"
)

// 加一个登录 provider 要改的地方散在十几个文件里，漏掉任何一处都不会编译失败——
// 症状是用户点了登录之后某一步静默走空（身份摘要缺一格、解绑按钮不出现、
// 绑定入口 404、signup_source 被回落成 email 导致渠道授权错读）。
//
// 这些测试遍历 ent/schema 里的 provider 声明，而不是点名今天这几个 provider：
// 新增一行声明、忘了接线，当场失败并指出缺的是哪个 provider、哪一处。

func TestEveryDeclaredProviderIsAValidSignupSource(t *testing.T) {
	for _, provider := range schema.AuthProviderTypes() {
		t.Run(provider, func(t *testing.T) {
			// signup_source 的归一化：认得的 provider 必须原样返回。
			// 落到默认分支会被写成 "email"，此后该用户的渠道默认授权全部按邮箱注册算。
			require.Equal(t, provider, normalizeOAuthSignupSource(provider),
				"normalizeOAuthSignupSource 不认识 %q：internal/service/auth_oauth_email_flow.go 的 case 列表漏了它", provider)
		})
	}
}

func TestEveryIdentityBindingProviderIsFullyWired(t *testing.T) {
	summaryFields := map[string]bool{}
	summaryType := reflect.TypeOf(UserIdentitySummarySet{})
	for i := 0; i < summaryType.NumField(); i++ {
		tag := summaryType.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name != "" && name != "-" {
			summaryFields[name] = true
		}
	}

	for _, provider := range schema.IdentityBindingProviders() {
		t.Run(provider, func(t *testing.T) {
			// 1. 个人页身份摘要：少一格 = 用户看不到这个绑定，也解绑不了。
			require.True(t, summaryFields[provider],
				"UserIdentitySummarySet 没有 json 标签为 %q 的字段：internal/service/user_service.go 漏了它", provider)

			// 2. provider 归一化：解绑/查询走它，落到默认分支等于该 provider 不存在。
			require.Equal(t, provider, normalizeUserIdentityProvider(provider),
				"normalizeUserIdentityProvider 不认识 %q", provider)

			// 3. 绑定入口：个人页"去绑定"按钮跳的地址。
			authorizeURL, err := buildUserIdentityBindAuthorizeURL(provider, "/profile")
			require.NoErrorf(t, err, "buildUserIdentityBindAuthorizeURL 不认识 %q", provider)
			require.Containsf(t, authorizeURL, "/api/v1/auth/oauth/"+provider+"/bind/start",
				"%q 的绑定入口地址不对：%s", provider, authorizeURL)

			// 4. 注册渠道默认授权：查不到设置项时新用户拿不到该渠道的默认额度/分组。
			defaults := &AuthSourceDefaultSettings{}
			_, ok := authSourceSignupSettings(defaults, provider)
			require.Truef(t, ok, "authSourceSignupSettings 不认识 %q：internal/service/auth_service.go 的 switch 漏了它", provider)
		})
	}
}

// 合成邮箱域名是"这个 provider 拿不到真实邮箱时怎么建号"的唯一依据，
// 同时也是反推 signup_source 的依据（inferLegacySignupSource）。两边必须一致。
func TestSyntheticEmailDomainsRoundTripToTheirProvider(t *testing.T) {
	domains := map[string]string{
		"linuxdo":  LinuxDoConnectSyntheticEmailDomain,
		"oidc":     OIDCConnectSyntheticEmailDomain,
		"wechat":   WeChatConnectSyntheticEmailDomain,
		"dingtalk": DingTalkConnectSyntheticEmailDomain,
		"feishu":   FeishuConnectSyntheticEmailDomain,
	}
	for provider, domain := range domains {
		t.Run(provider, func(t *testing.T) {
			require.Truef(t, schema.IsAuthProviderType(provider), "%q 不是已声明的 provider", provider)
			require.Equalf(t, provider, inferLegacySignupSource("someone"+domain),
				"合成邮箱域名 %s 没有映射回 %q", domain, provider)
		})
	}
	// 反向：每个以身份绑定的 provider 都应该有合成邮箱域名，否则 require_email=false
	// 的部署下它根本建不了号。
	for _, provider := range schema.IdentityBindingProviders() {
		_, ok := domains[provider]
		require.Truef(t, ok, "provider %q 没有合成邮箱域名（internal/service/domain_constants.go）", provider)
	}
}
