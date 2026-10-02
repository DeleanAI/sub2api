// Package pricingtest 为其它包的测试提供按生产加载路径装好的价格服务（只依赖 service 与 config，避免导入环）。
package pricingtest

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// modelPricingResourceDir 是随镜像发布的定价资源目录（backend/resources/model-pricing）。
func modelPricingResourceDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "resources", "model-pricing")
}

// OfficialPricingService 返回按生产加载路径装好的价格服务：不连远端，目录取随镜像发布的回退文件，
// 叠加官方价目录（OpenAI / Anthropic / DeepSeek 的价只来自它）。测试结束时停掉后台重载。
func OfficialPricingService(tb testing.TB) *service.PricingService {
	tb.Helper()
	cfg := &config.Config{}
	cfg.Pricing.DataDir = tb.TempDir()
	cfg.Pricing.FallbackFile = filepath.Join(modelPricingResourceDir(), "model_prices_and_context_window.json")
	cfg.Pricing.OfficialFile = filepath.Join(modelPricingResourceDir(), "official_prices.json")
	svc := service.NewPricingService(cfg, nil)
	if err := svc.Initialize(); err != nil {
		tb.Fatalf("load pricing: %v", err)
	}
	tb.Cleanup(svc.Stop)
	return svc
}
