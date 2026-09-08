//go:build unit

package service

import (
	"os"
	"reflect"
	"regexp"
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

// TestEverySyntheticEmailDomainIsRegistered 遍历源码里声明的每个合成邮箱后缀常量，
// 要求它出现在 syntheticEmailDomainByProvider 里。
//
// 这条护栏针对的是真实发生过的形状：飞书的 FeishuConnectSyntheticEmailDomain 常量
// 建好了、也被用来生成邮箱，却没进任何一条"这是保留邮箱"的判定链——于是
// feishu-<受害者 union_id>@feishu-connect.invalid 可以走普通邮箱注册。常量存在但
// 没人消费不会编译失败，只能靠遍历声明来发现。
func TestEverySyntheticEmailDomainIsRegistered(t *testing.T) {
	source, err := os.ReadFile("domain_constants.go")
	require.NoError(t, err)

	declared := regexp.MustCompile(`(?m)^const (\w+SyntheticEmailDomain) = "([^"]+)"`).FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, declared, "没扫到任何合成邮箱后缀常量，正则和源码脱节了")

	registered := map[string]string{}
	for provider, domain := range syntheticEmailDomainByProvider {
		registered[domain] = provider
	}
	for _, match := range declared {
		constName, domain := match[1], match[2]
		provider, ok := registered[domain]
		require.Truef(t, ok,
			"%s = %q 没有登记进 syntheticEmailDomainByProvider：这个后缀不会被任何"+
				"保留邮箱判定认出来，别人可以拿它注册/找回，顶掉对应 OAuth 用户", constName, domain)

		// 登记了就必须真的被三处消费方认出来。
		email := "someone" + domain
		require.Truef(t, isReservedEmail(email), "%s 登记了却没被 isReservedEmail 拒绝", constName)
		require.Truef(t, IsSyntheticOAuthEmail(email), "%s 登记了却没被 IsSyntheticOAuthEmail 认出", constName)
		require.Equalf(t, provider, inferLegacySignupSource(email),
			"%s 反查 signup_source 应为 %q", constName, provider)
		require.Equalf(t, provider, NormalizeOAuthSignupSource(provider),
			"provider %q 不是合法的 signup_source：合成邮箱域名登记了，但归一化列表漏了它", provider)
	}
}

// TestEveryBypassRegistrationSwitchIsWired 遍历源码里声明的每一个
// "*BypassRegistration" 配置开关，要求对应 provider 已在
// oauthRegistrationBypassChecks 里登记。
//
// 针对的形状：飞书的 FEISHU_CONNECT_BYPASS_REGISTRATION 建好了、校验了、文档写了，
// 但放行判断写死 signupSource != "dingtalk"，于是它是个从不执行的开关——白名单内的
// 员工被展示"创建账号"表单，提交后收到 REGISTRATION_DISABLED。写下来却没接线的
// 规则比没有更糟：它给的是假信心。
func TestEveryBypassRegistrationSwitchIsWired(t *testing.T) {
	declared := map[string]string{} // provider -> 声明位置
	for _, spec := range []struct {
		provider string
		file     string
		pattern  string
	}{
		{"dingtalk", "../config/config.go", `DingTalkConnectBypassRegistration|dingtalk_connect_bypass_registration`},
		{"feishu", "../config/validate_feishu.go", `BypassRegistration`},
	} {
		source, err := os.ReadFile(spec.file)
		require.NoError(t, err, spec.file)
		if regexp.MustCompile(spec.pattern).Match(source) {
			declared[spec.provider] = spec.file
		}
	}
	require.NotEmpty(t, declared, "没扫到任何 BypassRegistration 开关，护栏和源码脱节了")

	for provider, where := range declared {
		require.Containsf(t, oauthRegistrationBypassChecks, provider,
			"%s 声明了 BypassRegistration 开关（%s），但 oauthRegistrationBypassChecks 里没有它："+
				"这个开关打开后不会有任何效果，用户会在提交注册表单时收到 REGISTRATION_DISABLED",
			provider, where)
	}
	// 反向：登记了的必须是合法 signup_source，否则归一化之后永远查不到。
	for provider := range oauthRegistrationBypassChecks {
		require.Equalf(t, provider, NormalizeOAuthSignupSource(provider),
			"oauthRegistrationBypassChecks 的键 %q 不是归一化后的 signup_source，永远匹配不上", provider)
	}
}
