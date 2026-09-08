//go:build unit

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/schema"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestUserSignupSourceOrDefaultMatchesTheSingleNormalizer 遍历 ent/schema 里声明的
// provider，要求建号路径上的归一化与 signup_source 的唯一实现逐值一致。
//
// 这里曾经是第二份手写列表，比第一份少了 github 与 google：唯一的建号路径
// （userRepository.Create）走它，于是 GitHub/Google 注册被静默落成
// signup_source='email'，尽管迁移 231 的 CHECK 明确允许这两个值，而 service 层
// 那份"遍历声明"的护栏完全看不到这一份。两份实现只要并存就会分叉。
func TestUserSignupSourceOrDefaultMatchesTheSingleNormalizer(t *testing.T) {
	providers := append([]string{"", "email", "UNKNOWN-PROVIDER", "  LinuxDo  "}, schema.AuthProviderTypes()...)
	for _, provider := range providers {
		require.Equalf(t, service.NormalizeOAuthSignupSource(provider), userSignupSourceOrDefault(provider),
			"userSignupSourceOrDefault(%q) 与 service.NormalizeOAuthSignupSource 不一致："+
				"建号路径上的 signup_source 归一化必须只有一份实现", provider)
	}
}
