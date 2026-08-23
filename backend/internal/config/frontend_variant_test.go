//go:build unit

package config

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/frontendvariant"
)

// server.frontend_variant 是运维唯一的入口，这里守住三件事：默认值、环境变量能改、非法名字启动就失败。
// "这个变体在不在二进制里"由 web.VariantFS 判定（config 不能引 web，会成环），那部分在
// internal/web 的 embed 用例里覆盖。

func TestFrontendVariantDefaultsToBaseVariant(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.FrontendVariant != frontendvariant.Default {
		t.Fatalf("默认变体应为 %q，实际 %q", frontendvariant.Default, cfg.Server.FrontendVariant)
	}
}

func TestFrontendVariantIsReadFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SERVER_FRONTEND_VARIANT", "acme-dark")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.FrontendVariant != "acme-dark" {
		t.Fatalf("SERVER_FRONTEND_VARIANT 没生效，实际 %q", cfg.Server.FrontendVariant)
	}
	// 安装向导阶段走的是这条轻量读法，必须给出同一个答案，否则向导页和主服务会是两套前端。
	if got := GetFrontendVariant(); got != "acme-dark" {
		t.Fatalf("GetFrontendVariant() = %q，与 Load() 的结果不一致", got)
	}
}

func TestGetFrontendVariantFallsBackToDefault(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SERVER_FRONTEND_VARIANT", "")

	if got := GetFrontendVariant(); got != frontendvariant.Default {
		t.Fatalf("GetFrontendVariant() = %q，未配置时应为 %q", got, frontendvariant.Default)
	}
}

func TestValidateRejectsInvalidFrontendVariantName(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	for _, name := range []string{"", "Bad_Name", "../dist", "with space"} {
		cfg.Server.FrontendVariant = name
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate() 应拒绝非法变体名 %q", name)
		}
		if !strings.Contains(err.Error(), "server.frontend_variant") {
			t.Fatalf("Validate() 的报错应点名 server.frontend_variant，实际: %v", err)
		}
	}
}

func TestValidateTrimsFrontendVariant(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	cfg.Server.FrontendVariant = "  example  "
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	if cfg.Server.FrontendVariant != "example" {
		t.Fatalf("Validate() 应把变体名两端空白去掉，实际 %q", cfg.Server.FrontendVariant)
	}
}
