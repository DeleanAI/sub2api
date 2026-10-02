package service

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// shippedOfficialPriceFile 是随镜像发布的官方价目录（测试从包目录出发找到它）。
func shippedOfficialPriceFile() string {
	return filepath.Join("..", "..", "resources", "model-pricing", "official_prices.json")
}

// minimalRemoteCatalog 是只含一个无关模型的远端目录：parsePricingData 要求至少一条有效条目。
const minimalRemoteCatalog = `{"unrelated-test-model": {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6, "litellm_provider": "test"}}`

// newOfficialPricingService 构造叠加了随镜像发布的官方价目录的价格服务；remoteCatalog 为空时用最小目录。
func newOfficialPricingService(t *testing.T, remoteCatalog string) *PricingService {
	t.Helper()
	if remoteCatalog == "" {
		remoteCatalog = minimalRemoteCatalog
	}
	cfg := &config.Config{}
	cfg.Pricing.OfficialFile = shippedOfficialPriceFile()
	svc := NewPricingService(cfg, nil)
	snapshot, err := svc.buildPricingData([]byte(remoteCatalog))
	require.NoError(t, err)
	svc.pricingData = snapshot.data
	svc.official = snapshot.official
	return svc
}

// newOfficialBillingService 是基于官方价目录的计费服务（无硬编码以外的其它价格来源）。
func newOfficialBillingService(t *testing.T, remoteCatalog string) *BillingService {
	t.Helper()
	return NewBillingService(&config.Config{}, newOfficialPricingService(t, remoteCatalog))
}

var (
	sharedOfficialPricingOnce sync.Once
	sharedOfficialPricing     *PricingService
)

// sharedOfficialPricingService 是各测试共用的官方价目录价格服务（只读，按需加载一次）。
// 生产里 OpenAI / Anthropic / DeepSeek 的价只来自官方价目录，测试用同一份数据，不另写一套价。
func sharedOfficialPricingService() *PricingService {
	sharedOfficialPricingOnce.Do(func() {
		cfg := &config.Config{}
		cfg.Pricing.OfficialFile = shippedOfficialPriceFile()
		svc := NewPricingService(cfg, nil)
		snapshot, err := svc.buildPricingData([]byte(minimalRemoteCatalog))
		if err != nil {
			panic("load shipped official price catalog: " + err.Error())
		}
		svc.pricingData = snapshot.data
		svc.official = snapshot.official
		sharedOfficialPricing = svc
	})
	return sharedOfficialPricing
}

// newTestBillingService 是基于随镜像发布的官方价目录的计费服务（OpenAI / Anthropic / DeepSeek 的价），
// 其余厂商走硬编码回退价。
func newTestBillingService() *BillingService {
	return NewBillingService(&config.Config{}, sharedOfficialPricingService())
}
