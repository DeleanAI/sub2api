//go:build unit

package migrations

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 走声明而不是点名：平台名单从 domain 的常量来，迁移里的 CHECK 必须覆盖它们。
//
// 为什么需要这条护栏：合并上游 v0.2.5 时，上游的 237/238 号迁移重写了这两个 CHECK
// 以加入 MiniMax 与 OpenCode，但上游不知道 fork 加过 Qwen（229 号），于是按迁移顺序
// 把 'qwen' 从白名单里静默抹掉了。生产当时恰好没有 qwen 的配额行与 composite 路由，
// 迁移不报错，声明（AllowedQuotaPlatforms / isConcreteRequestPlatform 都含 Qwen）与
// 数据库约束就这样分叉，等到谁给 Qwen 配配额才会撞 CHECK 违反。
//
// 点名式测试（断言某一号迁移里有某个字面名单）抓不到这种回退——它只校验历史那一刻
// 的名单是否写对，而问题恰恰出在「后来的迁移覆盖了它」。所以这里按平台声明遍历，
// 并且只看**最后生效**的那条 CHECK。

// quotaPlatformsRequiringCheck 是必须出现在 user_platform_quotas /
// composite_model_routes 的 CHECK 白名单里的平台。
//
// 与 service.AllowedQuotaPlatforms / isConcreteRequestPlatform 同一组值；这里不能
// import internal/service（那会造成 migrations ← service 的反向依赖），因此从
// internal/domain 的常量构造，domain 是两者共同的源头。
func quotaPlatformsRequiringCheck() []string {
	return []string{
		domain.PlatformAnthropic,
		domain.PlatformOpenAI,
		domain.PlatformGemini,
		domain.PlatformAntigravity,
		domain.PlatformGrok,
		domain.PlatformKimi,
		domain.PlatformZhipu,
		domain.PlatformDeepseek,
		domain.PlatformQwen,
		domain.PlatformMiniMax,
		domain.PlatformOpenCodeGo,
	}
}

// lastCheckListFor 返回全部迁移中**最后一次**为该约束设置的 CHECK 平台列表。
// 迁移按文件名顺序执行，后写的赢——所以只有最后那条才是生效的。
func lastCheckListFor(t *testing.T, constraintName, column string) []string {
	t.Helper()

	names := embeddedMigrationNames(t)
	sort.Strings(names)

	pattern := regexp.MustCompile(
		`ADD\s+CONSTRAINT\s+` + regexp.QuoteMeta(constraintName) +
			`\s+CHECK\s*\(\s*` + regexp.QuoteMeta(column) + `\s+IN\s*\(([^)]*)\)`,
	)

	var last []string
	var lastFile string
	for _, name := range names {
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		content, err := FS.ReadFile(name)
		require.NoError(t, err)
		// 只看 Up 段：Down 段的名单是刻意回退用的，不代表生效状态。
		up := string(content)
		if idx := strings.Index(up, "-- +goose Down"); idx >= 0 {
			up = up[:idx]
		}
		normalized := strings.Join(strings.Fields(up), " ")
		for _, m := range pattern.FindAllStringSubmatch(normalized, -1) {
			items := strings.Split(m[1], ",")
			vals := make([]string, 0, len(items))
			for _, it := range items {
				vals = append(vals, strings.Trim(strings.TrimSpace(it), "'"))
			}
			last = vals
			lastFile = name
		}
	}
	require.NotEmpty(t, last,
		"找不到 %s 的 CHECK 定义——约束被改名或写法变了，这条护栏需要同步更新", constraintName)
	t.Logf("%s 的最终 CHECK 来自 %s：%v", constraintName, lastFile, last)
	return last
}

func TestPlatformChecksCoverEveryDeclaredQuotaPlatform(t *testing.T) {
	cases := []struct {
		constraint string
		column     string
	}{
		{"user_platform_quotas_platform_check", "platform"},
		{"composite_model_routes_target_platform_check", "target_platform"},
	}

	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			allowed := lastCheckListFor(t, tc.constraint, tc.column)
			set := make(map[string]struct{}, len(allowed))
			for _, a := range allowed {
				set[a] = struct{}{}
			}
			for _, platform := range quotaPlatformsRequiringCheck() {
				_, ok := set[platform]
				require.Truef(t, ok,
					"平台 %q 在后端声明里允许，但最终生效的 %s 白名单没有它（当前：%v）。"+
						"上游新增平台的迁移会整条重写这个 CHECK，重写时漏掉 fork 自己加的平台，"+
						"就会在没人注意的时候把它从白名单里抹掉",
					platform, tc.constraint, allowed)
			}
		})
	}
}

// CHECK 白名单不该出现声明里没有的平台——多出来的值意味着两边已经分叉，
// 或者某个平台被删了却没清理约束。
func TestPlatformChecksHaveNoUndeclaredPlatforms(t *testing.T) {
	declared := make(map[string]struct{})
	for _, p := range quotaPlatformsRequiringCheck() {
		declared[p] = struct{}{}
	}

	for _, tc := range []struct {
		constraint string
		column     string
	}{
		{"user_platform_quotas_platform_check", "platform"},
		{"composite_model_routes_target_platform_check", "target_platform"},
	} {
		t.Run(tc.constraint, func(t *testing.T) {
			for _, allowed := range lastCheckListFor(t, tc.constraint, tc.column) {
				_, ok := declared[allowed]
				require.Truef(t, ok,
					"%s 允许平台 %q，但后端声明里没有它：%s",
					tc.constraint, allowed,
					fmt.Sprintf("声明为 %v", quotaPlatformsRequiringCheck()))
			}
		})
	}
}
