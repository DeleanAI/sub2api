//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 护栏按声明遍历：下面的测试逐条读随镜像发布的官方价文件（不经过加载器），对文件里的每个模型、
// 每个别名、每种快照写法、每个服务档、长上下文与缓存分项，核对计费结果就是文件里写的官方数字。
// 新加的模型 / 档位当天就被覆盖；代码里任何改价逻辑（哪怕只对某个模型）都会让这里失败。

type rawOfficialFile struct {
	Providers map[string]struct {
		Models map[string]rawOfficialModel `json:"models"`
	} `json:"providers"`
}

type rawOfficialModel struct {
	Mode             string                     `json:"mode"`
	Input            *float64                   `json:"input"`
	CachedInput      *float64                   `json:"cached_input"`
	CacheWrite       *float64                   `json:"cache_write"`
	CacheWrite1h     *float64                   `json:"cache_write_1h"`
	Output           *float64                   `json:"output"`
	ImageInput       *float64                   `json:"image_input"`
	ImageCachedInput *float64                   `json:"image_cached_input"`
	ImageOutput      *float64                   `json:"image_output"`
	LongContext      *officialPriceLongContext  `json:"long_context"`
	Tiers            map[string]json.RawMessage `json:"tiers"`
	Aliases          []string                   `json:"aliases"`
}

func readShippedOfficialFile(t *testing.T) rawOfficialFile {
	t.Helper()
	body, err := os.ReadFile(shippedOfficialPriceFile())
	require.NoError(t, err)
	var file rawOfficialFile
	require.NoError(t, json.Unmarshal(body, &file))
	require.NotEmpty(t, file.Providers)
	return file
}

func valueOr(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}

// expectedPerMTok 是一个模型标准档每类 token 的官方单价（美元 / 百万 token），按官方口径补齐缺省项：
// 没有缓存读价 = 没有缓存折扣，没有缓存写价 = 写入不另收费，都按未缓存输入计。
type expectedPerMTok struct{ input, cachedInput, cacheWrite, cacheWrite1h, output float64 }

func (m rawOfficialModel) standardPrices() expectedPerMTok {
	input := *m.Input
	write := valueOr(m.CacheWrite, input)
	return expectedPerMTok{
		input:        input,
		cachedInput:  valueOr(m.CachedInput, input),
		cacheWrite:   write,
		cacheWrite1h: valueOr(m.CacheWrite1h, write),
		output:       valueOr(m.Output, 0),
	}
}

// tierPrices 返回某个服务档的官方单价：倍数声明按倍数缩放标准价；照抄官方档位表的价格对象，
// 写了的分项直接用表里的数字，没写的分项按同一倍数（官方各档对每个模型都是统一倍数）。
func (m rawOfficialModel) tierPrices(t *testing.T, tier string) (expectedPerMTok, bool) {
	raw, ok := m.Tiers[tier]
	if !ok {
		return m.standardPrices(), false
	}
	std := m.standardPrices()
	var multiplier float64
	if err := json.Unmarshal(raw, &multiplier); err == nil {
		return expectedPerMTok{std.input * multiplier, std.cachedInput * multiplier, std.cacheWrite * multiplier, std.cacheWrite1h * multiplier, std.output * multiplier}, true
	}
	var obj struct {
		Input, CachedInput, CacheWrite, CacheWrite1h, Output *float64
	}
	var fields map[string]*float64
	require.NoError(t, json.Unmarshal(raw, &fields))
	obj.Input, obj.CachedInput, obj.CacheWrite, obj.CacheWrite1h, obj.Output = fields["input"], fields["cached_input"], fields["cache_write"], fields["cache_write_1h"], fields["output"]
	require.NotNil(t, obj.Input, "tier %s needs an input price", tier)
	multiplier = *obj.Input / std.input
	return expectedPerMTok{
		input:        *obj.Input,
		cachedInput:  valueOr(obj.CachedInput, std.cachedInput*multiplier),
		cacheWrite:   valueOr(obj.CacheWrite, std.cacheWrite*multiplier),
		cacheWrite1h: valueOr(obj.CacheWrite1h, std.cacheWrite1h*multiplier),
		output:       valueOr(obj.Output, std.output*multiplier),
	}, true
}

// conflictingRemoteCatalog 构造一份「远端目录」：对每个官方模型、它的带日期快照与带厂商前缀的键都给出
// 错误的价格、Fast 价与长上下文阶梯。官方价必须整条压过它们。
func conflictingRemoteCatalog(t *testing.T, file rawOfficialFile) string {
	t.Helper()
	remote := map[string]map[string]any{
		"unrelated-test-model": {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6, "litellm_provider": "test"},
	}
	wrong := func() map[string]any {
		return map[string]any{
			"input_cost_per_token": 9.9e-5, "output_cost_per_token": 7.7e-4,
			"input_cost_per_token_priority": 3.3e-4, "output_cost_per_token_priority": 2.2e-3,
			"cache_read_input_token_cost": 5.5e-5, "cache_read_input_token_cost_priority": 6.6e-5,
			"cache_creation_input_token_cost": 4.4e-5, "cache_creation_input_token_cost_above_1hr": 8.8e-5,
			"input_cost_per_token_above_128k_tokens": 3e-4, "output_cost_per_token_above_128k_tokens": 3e-3,
			"supports_service_tier": true, "litellm_provider": "remote",
		}
	}
	for _, provider := range file.Providers {
		for name := range provider.Models {
			remote[name] = wrong()
			remote[name+"-20990101"] = wrong()
			remote[name+"-2099-01-01"] = wrong()
			remote["someprovider/"+name] = wrong()
		}
	}
	body, err := json.Marshal(remote)
	require.NoError(t, err)
	return string(body)
}

func TestOfficialPriceCatalog_BillingMatchesDeclaredPricesForEveryNameTierAndTokenKind(t *testing.T) {
	file := readShippedOfficialFile(t)
	billing := newOfficialBillingService(t, conflictingRemoteCatalog(t, file))

	const mtok = 1_000_000
	checked := 0
	for providerName, provider := range file.Providers {
		for name, m := range provider.Models {
			names := append([]string{name}, m.Aliases...)
			// 目录里改指过来的快照 / 前缀键，以及目录里没有、只能靠查找时剥日期认出来的写法
			names = append(names, name+"-20990101", name+"-2099-01-01", "someprovider/"+name, name+"-20300101", name+"-2030-01-01")
			for _, id := range names {
				label := providerName + "/" + name + " as " + id
				for _, tier := range []string{"", "default", "priority", "fast", "flex", "ultrafast"} {
					tierKey := canonicalBillingServiceTier(tier)
					want := m.standardPrices()
					declared := tierKey == ""
					if tierKey != "" {
						want, declared = m.tierPrices(t, tierKey)
					}

					// 文本 token：未缓存输入 / 缓存读 / 缓存写 / 输出各 1M
					tokens := UsageTokens{InputTokens: mtok, CacheReadTokens: mtok, CacheCreationTokens: mtok, OutputTokens: mtok}
					if m.CacheWrite1h != nil {
						tokens.CacheCreation5mTokens, tokens.CacheCreation1hTokens = mtok/2, mtok/2
					}
					cost, err := billing.CalculateCostWithServiceTier(id, tokens, 1, tier)
					require.NoError(t, err, label)
					wantWrite := want.cacheWrite
					if m.CacheWrite1h != nil {
						wantWrite = (want.cacheWrite + want.cacheWrite1h) / 2
					}
					// 4M 输入 token 不触发 272K 以上的长上下文：这一组只核对短上下文，长上下文单独核对
					if m.LongContext == nil {
						require.InDelta(t, want.input, cost.InputCost, 1e-9, "%s tier=%q input", label, tier)
						require.InDelta(t, want.cachedInput, cost.CacheReadCost, 1e-9, "%s tier=%q cached input", label, tier)
						require.InDelta(t, wantWrite, cost.CacheCreationCost, 1e-9, "%s tier=%q cache write", label, tier)
						require.InDelta(t, want.output, cost.OutputCost, 1e-9, "%s tier=%q output", label, tier)
					}
					if declared {
						require.Empty(t, cost.ServiceTierNotOffered, "%s tier=%q is declared", label, tier)
					} else {
						require.Equal(t, tierKey, cost.ServiceTierNotOffered, "%s tier=%q is not declared: billed standard and flagged", label, tier)
					}

					// 短上下文：单次请求 1000 输入 + 100 输出，远低于任何阈值
					short, err := billing.CalculateCostWithServiceTier(id, UsageTokens{InputTokens: 1000, OutputTokens: 100}, 1, tier)
					require.NoError(t, err, label)
					require.InDelta(t, (want.input*1000+want.output*100)/mtok, short.TotalCost, 1e-12, "%s tier=%q short request", label, tier)

					// 长上下文：输入总量刚超过阈值时整次请求按阶梯价（输入侧 × 输入倍率、输出 × 输出倍率）
					if lc := m.LongContext; lc != nil {
						in := lc.AboveInputTokens + 1
						long, err := billing.CalculateCostWithServiceTier(id, UsageTokens{InputTokens: in, OutputTokens: 1000}, 1, tier)
						require.NoError(t, err, label)
						wantLong := (want.input*lc.InputMultiplier*float64(in) + want.output*lc.OutputMultiplier*1000) / mtok
						require.InDelta(t, wantLong, long.TotalCost, 1e-9, "%s tier=%q long context", label, tier)
						atThreshold, err := billing.CalculateCostWithServiceTier(id, UsageTokens{InputTokens: lc.AboveInputTokens, OutputTokens: 1000}, 1, tier)
						require.NoError(t, err, label)
						require.InDelta(t, (want.input*float64(lc.AboveInputTokens)+want.output*1000)/mtok, atThreshold.TotalCost, 1e-9,
							"%s tier=%q exactly at the threshold is still short context", label, tier)
					}
				}

				// 图片 token
				if m.ImageOutput != nil || m.ImageInput != nil {
					cost, err := billing.CalculateCost(id, UsageTokens{InputTokens: 2 * mtok, ImageInputTokens: mtok, OutputTokens: mtok, ImageOutputTokens: mtok}, 1)
					require.NoError(t, err, label)
					require.InDelta(t, *m.Input, cost.InputCost, 1e-9, "%s text input", label)
					require.InDelta(t, valueOr(m.ImageInput, *m.Input), cost.ImageInputCost, 1e-9, "%s image input", label)
					require.InDelta(t, valueOr(m.ImageOutput, 0), cost.ImageOutputCost, 1e-9, "%s image output", label)
				}
				checked++
			}
		}
	}
	require.Greater(t, checked, 300, "every model, alias and snapshot spelling is checked")
}

// 官方定价页写明的规则，逐条套在文件里的每个模型上：抄错一个数字就会违反其中一条。
func TestOfficialPriceCatalog_FollowsTheVendorsPublishedRules(t *testing.T) {
	file := readShippedOfficialFile(t)
	near := func(a, b float64) bool { return a == b || (a-b < 1e-9*b && b-a < 1e-9*b) }

	for name, m := range file.Providers["anthropic"].Models {
		// https://platform.claude.com/docs/en/about-claude/pricing#prompt-caching
		require.True(t, near(*m.CacheWrite, *m.Input*1.25), "%s: 5-minute cache write = 1.25x base input", name)
		require.True(t, near(*m.CacheWrite1h, *m.Input*2), "%s: 1-hour cache write = 2x base input", name)
		readRatio := 0.1
		switch name {
		case "claude-fable-5-1", "claude-mythos-5-1": // 页面脚注 1
			readRatio = 0.025
		case "claude-opus-5-5": // 页面脚注 2
			readRatio = 0.05
		}
		require.True(t, near(*m.CachedInput, *m.Input*readRatio), "%s: cache hits = %.3gx base input", name, readRatio)
		require.Nil(t, m.LongContext, "%s: Claude has no long-context surcharge", name)
		for tier := range m.Tiers {
			require.Equal(t, "fast", tier, "%s: Claude only has fast mode", name)
		}
	}

	for name, m := range file.Providers["openai"].Models {
		// GPT-5.6 and later：cache writes 1.25x input，reads 0.1x（GPT-6.1 Sol 0.05x）
		if m.CacheWrite != nil {
			require.True(t, near(*m.CacheWrite, *m.Input*1.25), "%s: cache write = 1.25x input", name)
			readRatio := 0.1
			if name == "gpt-6.1-sol" {
				readRatio = 0.05
			}
			require.True(t, near(*m.CachedInput, *m.Input*readRatio), "%s: cached input = %.2fx input", name, readRatio)
		}
		if lc := m.LongContext; lc != nil {
			require.Equal(t, 272000, lc.AboveInputTokens, name)
			require.Equal(t, 2.0, lc.InputMultiplier, name)
			require.Equal(t, 1.5, lc.OutputMultiplier, name)
		}
		if raw, ok := m.Tiers["flex"]; ok {
			require.JSONEq(t, "0.5", string(raw), "%s: Flex is priced at Batch API rates", name)
		}
		require.NotContains(t, m.Tiers, "batch", name)
	}

	for name, m := range file.Providers["deepseek"].Models {
		require.Nil(t, m.CacheWrite, "%s: DeepSeek has no cache-write charge", name)
		require.Empty(t, m.Tiers, name)
	}
}

func TestOfficialPriceCatalog_ShippedFileLoadsAndCoversEveryProvider(t *testing.T) {
	catalog, err := loadOfficialPriceCatalog(shippedOfficialPriceFile())
	require.NoError(t, err)
	file := readShippedOfficialFile(t)
	declared := 0
	for _, provider := range file.Providers {
		for name, m := range provider.Models {
			declared += 1 + len(m.Aliases)
			require.Same(t, catalog.entries[name], catalog.entries[name], name)
			for _, alias := range m.Aliases {
				require.Same(t, catalog.entries[name], catalog.entries[alias], "%s alias of %s", alias, name)
			}
		}
	}
	require.Len(t, catalog.entries, declared)
	for _, provider := range []string{"openai", "anthropic", "deepseek"} {
		require.NotEmpty(t, file.Providers[provider].Models, provider)
	}
}

func TestOfficialPriceCatalog_RejectsMalformedFiles(t *testing.T) {
	valid := func(models string) string {
		return `{"currency":"USD","unit":"per_million_tokens","providers":{"openai":{"litellm_provider":"openai","source":"https://example.com","checked_at":"2026-10-03","models":` + models + `}}}`
	}
	cases := map[string]string{
		"unknown field":              valid(`{"m":{"input":1,"output":2,"cache_wirte":3}}`),
		"tier ratio mismatch":        valid(`{"m":{"input":1,"cached_input":0.1,"output":2,"tiers":{"fast":{"input":2,"cached_input":0.3,"output":4}}}}`),
		"tier output ratio mismatch": valid(`{"m":{"input":1,"output":2,"tiers":{"fast":{"input":2,"output":5}}}}`),
		"tier prices unknown field":  valid(`{"m":{"input":1,"output":2,"tiers":{"fast":{"input":2,"outptu":4}}}}`),
		"tier priced item missing":   valid(`{"m":{"input":1,"output":2,"tiers":{"fast":{"input":2,"cached_input":0.2}}}}`),
		"unknown tier":               valid(`{"m":{"input":1,"output":2,"tiers":{"batch":0.5}}}`),
		"non-positive multiplier":    valid(`{"m":{"input":1,"output":2,"tiers":{"flex":0}}}`),
		"missing output":             valid(`{"m":{"input":1}}`),
		"missing input":              valid(`{"m":{"output":1}}`),
		"negative price":             valid(`{"m":{"input":-1,"output":1}}`),
		"uppercase name":             valid(`{"Gpt-X":{"input":1,"output":2}}`),
		"alias collides with model":  valid(`{"a":{"input":1,"output":2,"aliases":["b"]},"b":{"input":1,"output":2}}`),
		"duplicate alias":            valid(`{"a":{"input":1,"output":2,"aliases":["x"]},"b":{"input":1,"output":2,"aliases":["x"]}}`),
		"image model without output": valid(`{"m":{"mode":"image_generation","input":1}}`),
		"1h write without 5m write":  valid(`{"m":{"input":1,"output":2,"cache_write_1h":2}}`),
		"bad long context":           valid(`{"m":{"input":1,"output":2,"long_context":{"above_input_tokens":272000,"input_multiplier":1,"output_multiplier":1}}}`),
		"unknown mode":               valid(`{"m":{"mode":"audio","input":1,"output":2}}`),
		"wrong currency":             strings.Replace(valid(`{"m":{"input":1,"output":2}}`), `"USD"`, `"CNY"`, 1),
		"wrong unit":                 strings.Replace(valid(`{"m":{"input":1,"output":2}}`), `per_million_tokens`, `per_token`, 1),
		"provider without source":    strings.Replace(valid(`{"m":{"input":1,"output":2}}`), `"source":"https://example.com",`, ``, 1),
		"no models":                  valid(`{}`),
	}
	for name, body := range cases {
		_, err := parseOfficialPriceCatalog([]byte(body))
		require.Error(t, err, name)
	}
	_, err := parseOfficialPriceCatalog([]byte(valid(`{"m":{"input":1,"cached_input":0.1,"output":2,"tiers":{"fast":{"input":2,"cached_input":0.2,"output":4},"flex":0.5}}}`)))
	require.NoError(t, err)
}

func TestOfficialPriceCatalog_ConfiguredButMissingOrInvalidFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Pricing.OfficialFile = filepath.Join(dir, "missing.json")
	svc := NewPricingService(cfg, nil)
	_, err := svc.buildPricingData([]byte(minimalRemoteCatalog))
	require.Error(t, err, "a configured official catalog that cannot be read must stop the load")
	require.Error(t, svc.validateCustomPricingFiles())

	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"currency":"USD"}`), 0o600))
	cfg.Pricing.OfficialFile = bad
	_, err = svc.buildPricingData([]byte(minimalRemoteCatalog))
	require.Error(t, err)
	require.Error(t, svc.validateCustomPricingFiles())
}

// 启动路径：官方价目录坏了，价格服务初始化失败且 ProvidePricingService 拒绝启动（不带着空价表上线）；
// 目录正常时即使连不上远端也能从回退文件 + 官方价目录启动。
func TestOfficialPriceCatalog_StartupRefusesWithoutTheCatalog(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "official.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"currency":"USD","unit":"per_million_tokens","providers":{}}`), 0o600))

	newCfg := func(official string) *config.Config {
		cfg := &config.Config{}
		cfg.Pricing.DataDir = t.TempDir()
		cfg.Pricing.FallbackFile = filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json")
		cfg.Pricing.OfficialFile = official
		return cfg
	}

	svc, err := ProvidePricingService(newCfg(bad), nil)
	require.ErrorIs(t, err, ErrOfficialPriceCatalog)
	require.Nil(t, svc)

	svc, err = ProvidePricingService(newCfg(filepath.Join(dir, "missing.json")), nil)
	require.ErrorIs(t, err, ErrOfficialPriceCatalog)
	require.Nil(t, svc)

	svc, err = ProvidePricingService(newCfg(shippedOfficialPriceFile()), nil)
	require.NoError(t, err)
	t.Cleanup(svc.Stop)
	require.NotNil(t, svc.GetModelPricing("gpt-5.6-sol"))
	require.InDelta(t, 4e-6, svc.GetModelPricing("gpt-5.6-sol").InputCostPerToken, 1e-15)
}

func TestOfficialPriceCatalog_OperatorOverrideCannotRepriceOfficialModels(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "override.json")
	require.NoError(t, os.WriteFile(override, []byte(`{
		"gpt-5.6-sol": {"input_cost_per_token": 1e-3},
		"claude-opus-4-1-20250805": {"output_cost_per_token": 1e-3},
		"unrelated-test-model": {"input_cost_per_token": 5e-6}
	}`), 0o600))
	cfg := &config.Config{}
	cfg.Pricing.OfficialFile = shippedOfficialPriceFile()
	cfg.Pricing.OverrideFile = override
	svc := NewPricingService(cfg, nil)
	snapshot, err := svc.buildPricingData([]byte(minimalRemoteCatalog))
	require.NoError(t, err)
	require.InDelta(t, 4e-6, snapshot.data["gpt-5.6-sol"].InputCostPerToken, 1e-15)
	require.InDelta(t, 75e-6, snapshot.official.entries["claude-opus-4-1-20250805"].OutputCostPerToken, 1e-15)
	require.InDelta(t, 5e-6, snapshot.data["unrelated-test-model"].InputCostPerToken, 1e-15, "non-official models still take operator overrides")
}

func TestOfficialPriceCatalog_SuffixedNamesUseTheLongestOfficialPrefix(t *testing.T) {
	billing := newTestBillingService()
	for model, official := range map[string]string{
		"gpt-6-astra-high":            "gpt-6-astra",
		"gpt-5.4-mini-high":           "gpt-5.4-mini",
		"claude-opus-5-5-thinking":    "claude-opus-5-5",
		"deepseek-v4-pro-0813-latest": "deepseek-v4-pro",
		"claude-mythos-5-1-preview":   "claude-mythos-5-1",
	} {
		got, err := billing.GetModelPricing(model)
		require.NoError(t, err, model)
		want, err := billing.GetModelPricing(official)
		require.NoError(t, err, official)
		require.Equal(t, want, got, "%s should price as %s", model, official)
	}
}

func TestOfficialPriceCatalog_UndeclaredTierIsBilledStandardAndFlagged(t *testing.T) {
	billing := newTestBillingService()
	tokens := UsageTokens{InputTokens: 1000, OutputTokens: 1000}
	for _, tc := range []struct {
		model, tier string
		offered     bool
	}{
		{"gpt-5.5-pro", "fast", false},      // 官方 Fast 表没有 gpt-5.5-pro
		{"gpt-5.6-sol", "ultrafast", false}, // Ultrafast 只公布了 gpt-6-astra 的价
		{"gpt-4.1", "flex", false},          // Flex 表没有 gpt-4.1
		{"claude-opus-4-7", "fast", false},  // Opus 4.7 不提供 fast mode
		{"claude-opus-4-6", "fast", true},   // 官方：Opus 4.6 的 fast 请求按标准价计
		{"gpt-6-astra", "ultrafast", true},
	} {
		standard, err := billing.CalculateCostWithServiceTier(tc.model, tokens, 1, "")
		require.NoError(t, err)
		cost, err := billing.CalculateCostWithServiceTier(tc.model, tokens, 1, tc.tier)
		require.NoError(t, err)
		if tc.offered {
			require.Empty(t, cost.ServiceTierNotOffered, fmt.Sprintf("%s %s", tc.model, tc.tier))
			continue
		}
		require.InDelta(t, standard.TotalCost, cost.TotalCost, 1e-15, "%s %s billed at standard", tc.model, tc.tier)
		require.Equal(t, tc.tier, cost.ServiceTierNotOffered, "%s %s flagged", tc.model, tc.tier)
	}
}
