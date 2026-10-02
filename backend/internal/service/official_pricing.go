package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
)

// 官方价目录（pricing.official_file，默认随镜像发布的 resources/model-pricing/official_prices.json）。
//
// 文件按厂商分组，价格单位是「美元 / 百万 token」，与各厂商官方定价页的表格一一对应，便于逐格核对。
// 目录里的每个模型是它的唯一价格来源：
//   - 整条替换远端目录 / 回退文件里的同名条目（不是逐字段合并，远端多出来的字段不会漏进来）；
//   - 目录里该模型的带日期快照、带厂商前缀的键（claude-opus-4-1-20250805、openai/gpt-4o、
//     vertex_ai/claude-opus-4-5@20251101）一并指向官方价，任何查找路径都绕不开；
//   - 运营 override 文件对官方模型不生效（告警说明），要改价就改官方价文件，或用分组 / 渠道定价。
//
// 服务档（fast / flex / ultrafast）以倍数声明：官方各档对每个模型都是统一倍数（缓存读写随之同比例），
// 文件里既可以直接写倍数（Flex 官方口径即「按 Batch 价」= 0.5），也可以照抄官方档位表的价格，
// 由加载器折算成倍数并校验各分项比例一致。没声明的档位按标准价计费（计费时留日志）。

// officialPriceFileUnit 是官方价文件里价格数字的单位。
const officialPriceFileUnit = "per_million_tokens"

// ErrOfficialPriceCatalog 标记官方价目录本身加载失败（缺失、不可读或不合规）。价格服务初始化遇到它时
// 拒绝启动（见 ProvidePricingService）：这些模型在别处没有价，带病启动只会把它们的用量全部记成零成本。
var ErrOfficialPriceCatalog = errors.New("official price catalog unavailable")

// officialServiceTiers 是官方价文件可以声明的服务档。
var officialServiceTiers = map[string]bool{"fast": true, "flex": true, "ultrafast": true}

// officialModelModes 是官方价文件可以声明的模型类别（缺省 chat）。
var officialModelModes = map[string]bool{"chat": true, "embedding": true, "image_generation": true}

type officialPriceFile struct {
	Currency    string                           `json:"currency"`
	Unit        string                           `json:"unit"`
	Description string                           `json:"description"`
	Providers   map[string]officialPriceProvider `json:"providers"`
}

type officialPriceProvider struct {
	LiteLLMProvider string                        `json:"litellm_provider"`
	Source          string                        `json:"source"`
	CheckedAt       string                        `json:"checked_at"`
	Notes           []string                      `json:"notes"`
	Models          map[string]officialPriceModel `json:"models"`
}

type officialPriceModel struct {
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

type officialPriceLongContext struct {
	AboveInputTokens int     `json:"above_input_tokens"`
	InputMultiplier  float64 `json:"input_multiplier"`
	OutputMultiplier float64 `json:"output_multiplier"`
}

// officialTierPrices 是照抄官方档位表的一组价格（缺省的分项不参与校验）。
type officialTierPrices struct {
	Input        *float64 `json:"input"`
	CachedInput  *float64 `json:"cached_input"`
	CacheWrite   *float64 `json:"cache_write"`
	CacheWrite1h *float64 `json:"cache_write_1h"`
	Output       *float64 `json:"output"`
}

// officialPriceCatalog 是加载、校验后的官方价目录。
type officialPriceCatalog struct {
	// entries 按小写模型名索引，包含每个模型的正式名与别名（别名与正式名共用同一个价卡）。
	entries map[string]*LiteLLMModelPricing
	// models 是正式名（不含别名）的数量，仅用于日志。
	models int
}

// loadOfficialPriceCatalog 读取并校验官方价文件。任何不合规（未知字段、缺价、档位比例不一致、
// 别名冲突……）都返回错误：这个文件是计费的唯一依据，宁可加载失败也不带病生效。
func loadOfficialPriceCatalog(path string) (*officialPriceCatalog, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrOfficialPriceCatalog, path, err)
	}
	catalog, err := parseOfficialPriceCatalog(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrOfficialPriceCatalog, path, err)
	}
	return catalog, nil
}

func parseOfficialPriceCatalog(body []byte) (*officialPriceCatalog, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var file officialPriceFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("official price file: %w", err)
	}
	if file.Currency != "USD" {
		return nil, fmt.Errorf("official price file: currency must be USD, got %q", file.Currency)
	}
	if file.Unit != officialPriceFileUnit {
		return nil, fmt.Errorf("official price file: unit must be %s, got %q", officialPriceFileUnit, file.Unit)
	}
	if len(file.Providers) == 0 {
		return nil, fmt.Errorf("official price file: no providers")
	}

	catalog := &officialPriceCatalog{entries: make(map[string]*LiteLLMModelPricing)}
	owner := make(map[string]string) // 小写名 → 声明它的 "provider/model"，用于报告冲突
	register := func(name, declaredBy string, pricing *LiteLLMModelPricing) error {
		if name == "" || name != strings.ToLower(strings.TrimSpace(name)) {
			return fmt.Errorf("official price file: %s: model names and aliases must be lowercase and trimmed, got %q", declaredBy, name)
		}
		if prev, ok := owner[name]; ok {
			return fmt.Errorf("official price file: %q is declared by both %s and %s", name, prev, declaredBy)
		}
		owner[name] = declaredBy
		catalog.entries[name] = pricing
		return nil
	}

	providers := make([]string, 0, len(file.Providers))
	for name := range file.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	for _, providerName := range providers {
		provider := file.Providers[providerName]
		if strings.TrimSpace(provider.LiteLLMProvider) == "" || strings.TrimSpace(provider.Source) == "" || strings.TrimSpace(provider.CheckedAt) == "" {
			return nil, fmt.Errorf("official price file: provider %s needs litellm_provider, source and checked_at", providerName)
		}
		if len(provider.Models) == 0 {
			return nil, fmt.Errorf("official price file: provider %s has no models", providerName)
		}
		modelNames := make([]string, 0, len(provider.Models))
		for name := range provider.Models {
			modelNames = append(modelNames, name)
		}
		sort.Strings(modelNames)
		for _, modelName := range modelNames {
			declaredBy := providerName + "/" + modelName
			pricing, err := buildOfficialModelPricing(provider, provider.Models[modelName])
			if err != nil {
				return nil, fmt.Errorf("official price file: %s: %w", declaredBy, err)
			}
			pricing.PriceSource = "official:" + providerName
			if err := register(modelName, declaredBy, pricing); err != nil {
				return nil, err
			}
			catalog.models++
			for _, alias := range provider.Models[modelName].Aliases {
				if err := register(alias, declaredBy, pricing); err != nil {
					return nil, err
				}
			}
		}
	}
	return catalog, nil
}

// buildOfficialModelPricing 把一条官方价（美元 / 百万 token）换算成按 token 计价的价卡。
func buildOfficialModelPricing(provider officialPriceProvider, m officialPriceModel) (*LiteLLMModelPricing, error) {
	mode := m.Mode
	if mode == "" {
		mode = "chat"
	}
	if !officialModelModes[mode] {
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	for field, value := range map[string]*float64{
		"input": m.Input, "cached_input": m.CachedInput, "cache_write": m.CacheWrite, "cache_write_1h": m.CacheWrite1h,
		"output": m.Output, "image_input": m.ImageInput, "image_cached_input": m.ImageCachedInput, "image_output": m.ImageOutput,
	} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return nil, fmt.Errorf("%s must be a finite non-negative number", field)
		}
	}
	if m.Input == nil {
		return nil, fmt.Errorf("input price is required")
	}
	switch mode {
	case "chat":
		if m.Output == nil {
			return nil, fmt.Errorf("output price is required for chat models")
		}
	case "image_generation":
		if m.ImageOutput == nil {
			return nil, fmt.Errorf("image_output price is required for image generation models")
		}
	}
	if m.CacheWrite1h != nil && m.CacheWrite == nil {
		return nil, fmt.Errorf("cache_write_1h needs cache_write (the 5-minute price)")
	}

	perToken := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v / 1e6
	}
	input := perToken(m.Input)
	pricing := &LiteLLMModelPricing{
		InputCostPerToken:  input,
		OutputCostPerToken: perToken(m.Output),
		// 官方口径：输入 token 只分未缓存 / 缓存读 / 缓存写三类。没有缓存读价的模型没有缓存折扣，
		// 没有缓存写价的模型写入不另收费——两者都按未缓存输入计。
		CacheReadInputTokenCost:     input,
		CacheCreationInputTokenCost: input,
		SupportsPromptCaching:       m.CachedInput != nil,
		LiteLLMProvider:             provider.LiteLLMProvider,
		Mode:                        mode,
		InputCostPerImageToken:      perToken(m.ImageInput),
		OutputCostPerImageToken:     perToken(m.ImageOutput),
		CacheReadInputImageTokenCost: func() float64 {
			if m.ImageCachedInput != nil {
				return perToken(m.ImageCachedInput)
			}
			return perToken(m.ImageInput)
		}(),
		ServiceTierMultipliers: map[string]float64{},
	}
	if m.CachedInput != nil {
		pricing.CacheReadInputTokenCost = perToken(m.CachedInput)
	}
	if m.CacheWrite != nil {
		pricing.CacheCreationInputTokenCost = perToken(m.CacheWrite)
	}
	if m.CacheWrite1h != nil {
		pricing.CacheCreationInputTokenCostAbove1hr = perToken(m.CacheWrite1h)
	}
	if lc := m.LongContext; lc != nil {
		if lc.AboveInputTokens <= 0 || lc.InputMultiplier < 1 || lc.OutputMultiplier < 1 || (lc.InputMultiplier == 1 && lc.OutputMultiplier == 1) {
			return nil, fmt.Errorf("long_context needs above_input_tokens > 0 and multipliers >= 1 (not both 1)")
		}
		pricing.LongContextInputTokenThreshold = lc.AboveInputTokens
		pricing.LongContextInputCostMultiplier = lc.InputMultiplier
		pricing.LongContextOutputCostMultiplier = lc.OutputMultiplier
	}
	for tier, raw := range m.Tiers {
		if !officialServiceTiers[tier] {
			return nil, fmt.Errorf("unknown service tier %q", tier)
		}
		multiplier, err := officialTierMultiplier(m, raw)
		if err != nil {
			return nil, fmt.Errorf("tier %s: %w", tier, err)
		}
		pricing.ServiceTierMultipliers[tier] = multiplier
	}
	pricing.SupportsServiceTier = len(pricing.ServiceTierMultipliers) > 0
	return pricing, nil
}

// officialTierMultiplier 解析一个档位声明：数字即倍数；对象是照抄官方档位表的价格，倍数取输入价之比，
// 其余给出的分项必须与标准价同比例（官方各档对每个模型都是统一倍数，比例不一致说明抄错了）。
func officialTierMultiplier(m officialPriceModel, raw json.RawMessage) (float64, error) {
	var multiplier float64
	if err := json.Unmarshal(raw, &multiplier); err == nil {
		if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
			return 0, fmt.Errorf("multiplier must be a positive number")
		}
		return multiplier, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var prices officialTierPrices
	if err := dec.Decode(&prices); err != nil {
		return 0, fmt.Errorf("must be a multiplier or a price object: %w", err)
	}
	if prices.Input == nil || *prices.Input <= 0 || *m.Input <= 0 {
		return 0, fmt.Errorf("price object needs a positive input price (and the model a positive standard input price)")
	}
	multiplier = *prices.Input / *m.Input
	for field, pair := range map[string][2]*float64{
		"cached_input":   {prices.CachedInput, m.CachedInput},
		"cache_write":    {prices.CacheWrite, m.CacheWrite},
		"cache_write_1h": {prices.CacheWrite1h, m.CacheWrite1h},
		"output":         {prices.Output, m.Output},
	} {
		tierPrice, standard := pair[0], pair[1]
		if tierPrice == nil {
			continue
		}
		if standard == nil {
			return 0, fmt.Errorf("%s is priced in the tier but not in the standard rates", field)
		}
		if math.Abs(*tierPrice-*standard*multiplier) > 1e-9*math.Max(1, *tierPrice) {
			return 0, fmt.Errorf("%s %.6g is not %.6g x the standard %.6g (the tier ratio taken from input)", field, *tierPrice, multiplier, *standard)
		}
	}
	return multiplier, nil
}

// officialSnapshotSuffix 匹配目录键里的快照日期后缀：Anthropic 的 -20251101、Vertex 的 @20251101、
// OpenAI 的 -2024-08-06。
var officialSnapshotSuffix = regexp.MustCompile(`(?:[-@]\d{8}|-\d{4}-\d{2}-\d{2})$`)

// officialBaseName 返回目录键或请求模型名对应的官方正式名 / 别名（不是官方模型时返回空串）：
// 先看去掉厂商前缀后的名字本身，再看去掉快照日期后的名字。
func (c *officialPriceCatalog) officialBaseName(name string) string {
	if c == nil {
		return ""
	}
	segment := lastSegment(strings.ToLower(strings.TrimSpace(name)))
	if _, ok := c.entries[segment]; ok {
		return segment
	}
	if stripped := officialSnapshotSuffix.ReplaceAllString(segment, ""); stripped != segment {
		if _, ok := c.entries[stripped]; ok {
			return stripped
		}
	}
	return ""
}

// apply 把官方价铺进目录数据：正式名与别名整条替换，目录里指向官方模型的快照 / 带前缀键改指官方价。
// 返回被改指的目录键数量（日志用）。
func (c *officialPriceCatalog) apply(data map[string]*LiteLLMModelPricing) int {
	for name, pricing := range c.entries {
		data[name] = pricing
	}
	repointed := 0
	for key := range data {
		if _, official := c.entries[key]; official {
			continue
		}
		if base := c.officialBaseName(key); base != "" {
			data[key] = c.entries[base]
			repointed++
		}
	}
	return repointed
}
