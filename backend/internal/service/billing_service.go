package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

// APIKeyRateLimitCacheData holds rate limit usage data cached in Redis.
type APIKeyRateLimitCacheData struct {
	Usage5h  float64 `json:"usage_5h"`
	Usage1d  float64 `json:"usage_1d"`
	Usage7d  float64 `json:"usage_7d"`
	Window5h int64   `json:"window_5h"` // unix timestamp, 0 = not started
	Window1d int64   `json:"window_1d"`
	Window7d int64   `json:"window_7d"`
}

// UserPlatformQuotaKey 标识一个 user×platform，用于脏集出入与批量读。
type UserPlatformQuotaKey struct {
	UserID   int64
	Platform string
}

// UserPlatformQuotaCacheEntry Redis hash 反序列化结果。
//
// SchemaVersion 用于向后兼容：
//   - 0（旧 entry，无 SchemaVersion 字段）→ 视为 cache MISS，强制 refresh
//   - 1（当前版本）→ 包含 limits 和 window_start，可免 DB 查询
//
// limit 字段为 nil 表示"无限额"（DB 中对应列为 NULL）。
const UserPlatformQuotaCacheSchemaV1 = int64(1)

type UserPlatformQuotaCacheEntry struct {
	DailyUsageUSD   float64
	WeeklyUsageUSD  float64
	MonthlyUsageUSD float64
	Version         int64
	SchemaVersion   int64

	// 以下字段仅在 SchemaVersion >= 1 时有效
	DailyLimitUSD   *float64
	WeeklyLimitUSD  *float64
	MonthlyLimitUSD *float64

	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time
}

// BillingCache defines cache operations for billing service
type BillingCache interface {
	// Balance operations
	GetUserBalance(ctx context.Context, userID int64) (float64, error)
	SetUserBalance(ctx context.Context, userID int64, balance float64) error
	DeductUserBalance(ctx context.Context, userID int64, amount float64) error
	InvalidateUserBalance(ctx context.Context, userID int64) error

	// Subscription operations
	GetSubscriptionCache(ctx context.Context, userID, groupID int64) (*SubscriptionCacheData, error)
	SetSubscriptionCache(ctx context.Context, userID, groupID int64, data *SubscriptionCacheData) error
	UpdateSubscriptionUsage(ctx context.Context, userID, groupID int64, cost float64) error
	InvalidateSubscriptionCache(ctx context.Context, userID, groupID int64) error

	// API Key rate limit operations
	GetAPIKeyRateLimit(ctx context.Context, keyID int64) (*APIKeyRateLimitCacheData, error)
	SetAPIKeyRateLimit(ctx context.Context, keyID int64, data *APIKeyRateLimitCacheData) error
	UpdateAPIKeyRateLimitUsage(ctx context.Context, keyID int64, cost float64) error
	InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error

	// user × platform quota 缓存
	GetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) (*UserPlatformQuotaCacheEntry, bool, error)
	SetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string, entry *UserPlatformQuotaCacheEntry, ttl time.Duration) error
	DeleteUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) error
	// IncrUserPlatformQuotaUsageCache 在缓存命中时累加用量；缓存未命中（key 不存在）静默返回 nil。
	// markDirty=true 时将该 key 的 member 写入 Redis 脏集，供 flusher 批量回写 DB。
	IncrUserPlatformQuotaUsageCache(ctx context.Context, userID int64, platform string, cost float64, ttl time.Duration, markDirty bool) error

	// 脏集读写，供 flusher 使用。
	PopDirtyUserPlatformQuotaKeys(ctx context.Context, n int) ([]UserPlatformQuotaKey, error)
	ReaddDirtyUserPlatformQuotaKeys(ctx context.Context, keys []UserPlatformQuotaKey) error
	BatchGetUserPlatformQuotaCache(ctx context.Context, keys []UserPlatformQuotaKey) ([]*UserPlatformQuotaCacheEntry, error)
}

// ModelPricing 模型价格配置（per-token价格，与LiteLLM格式一致）
type ModelPricing struct {
	InputPricePerToken                 float64            // 每token输入价格 (USD)
	InputPricePerTokenPriority         float64            // priority service tier 下每token输入价格 (USD)
	ImageInputPricePerToken            float64            // 图片输入 token 价格 (USD)，用于多模态 embedding 等图文不同价场景；为 0 时回退到 InputPricePerToken
	ImageCacheReadPricePerToken        float64            // 图片缓存输入价格；无独立价格时沿用缓存读取价
	OutputPricePerToken                float64            // 每token输出价格 (USD)
	OutputPricePerTokenPriority        float64            // priority service tier 下每token输出价格 (USD)
	CacheCreationPricePerToken         float64            // 缓存创建每token价格 (USD)
	CacheCreationPricePerTokenPriority float64            // priority service tier 下缓存创建每token价格 (USD)
	CacheReadPricePerToken             float64            // 缓存读取每token价格 (USD)
	CacheReadPricePerTokenPriority     float64            // priority service tier 下缓存读取每token价格 (USD)
	FastMultiplier                     *float64           // 渠道显式 Fast/priority 倍率；nil 时沿用模型目录行为
	FlexMultiplier                     *float64           // 渠道显式 Flex 倍率；nil 时沿用默认行为
	ServiceTierMultipliers             map[string]float64 // 官方价目录声明的服务档倍数；非 nil 时未列出的档位按标准价计（见 resolveServiceTierMultiplier）
	ReasoningEffortMultipliers         map[string]float64 // 最终转发的推理等级对应的计费倍率；未配置的等级按 1 倍计费
	CacheCreation5mPrice               float64            // 5分钟缓存创建每token价格 (USD)
	CacheCreation1hPrice               float64            // 1小时缓存创建每token价格 (USD)
	SupportsCacheBreakdown             bool               // 是否支持详细的缓存分类
	LongContextInputThreshold          int                // 超过阈值后按整次会话提升输入价格
	LongContextThresholdInclusive      bool               // 达到阈值即应用（xAI）；默认保持严格大于以兼容既有模型
	LongContextInputMultiplier         float64            // 长上下文整次会话输入倍率
	LongContextOutputMultiplier        float64            // 长上下文整次会话输出倍率
	ImageOutputPricePerToken           float64            // 图片输出 token 价格 (USD)
	ImageOutputPriceExplicit           bool               // 是否由渠道定价显式设定（为 true 时即使 == 0 也不回退）
}

func normalizeBillingServiceTier(serviceTier string) string {
	return strings.ToLower(strings.TrimSpace(serviceTier))
}

func usePriorityServiceTierPricing(serviceTier string, pricing *ModelPricing) bool {
	if pricing == nil {
		return false
	}
	if canonicalBillingServiceTier(serviceTier) != "fast" {
		return false
	}
	if pricing.FastMultiplier != nil {
		return false
	}
	return pricing.InputPricePerTokenPriority > 0 || pricing.OutputPricePerTokenPriority > 0 ||
		pricing.CacheCreationPricePerTokenPriority > 0 || pricing.CacheReadPricePerTokenPriority > 0
}

// canonicalBillingServiceTier 把请求 / 上游声明的服务档归一：priority 与 fast 同档（OpenAI 2026-07-30 起
// 把 Priority 更名为 Fast），default / standard / auto / scale 及空值都是标准档（返回 ""）。
func canonicalBillingServiceTier(serviceTier string) string {
	switch tier := normalizeBillingServiceTier(serviceTier); tier {
	case "", "default", "standard", "auto", "scale":
		return ""
	case "priority", "fast":
		return "fast"
	default:
		return tier
	}
}

// resolveServiceTierMultiplier 返回服务档相对标准价的倍数，以及价卡是否提供该档。优先级：
//  1. 渠道 / 分组显式配置的 Fast、Flex 倍率；
//  2. 官方价目录逐档声明的倍数（ServiceTierMultipliers 非 nil）：没列出的档位不提供，按标准价计；
//  3. 远端 / 回退目录的价卡（没有逐档声明）：Fast 2 倍、Flex 0.5 倍的通用口径，其余档位无依据，按标准价计。
//
// offered=false 时调用方按标准价计费，并通过 CostBreakdown.ServiceTierNotOffered 留痕。
func resolveServiceTierMultiplier(serviceTier string, pricing *ModelPricing) (multiplier float64, offered bool) {
	tier := canonicalBillingServiceTier(serviceTier)
	if tier == "" {
		return 1, true
	}
	if pricing != nil {
		switch tier {
		case "fast":
			if pricing.FastMultiplier != nil {
				return *pricing.FastMultiplier, true
			}
		case "flex":
			if pricing.FlexMultiplier != nil {
				return *pricing.FlexMultiplier, true
			}
		}
		if pricing.ServiceTierMultipliers != nil {
			if m, ok := pricing.ServiceTierMultipliers[tier]; ok {
				return m, true
			}
			return 1, false
		}
	}
	switch tier {
	case "fast":
		return 2, true
	case "flex":
		return 0.5, true
	default:
		return 1, false
	}
}

// UsageTokens 使用的token数量
type UsageTokens struct {
	InputTokens           int
	ImageInputTokens      int
	ImageCacheReadTokens  int
	OutputTokens          int
	CacheCreationTokens   int
	CacheReadTokens       int
	CacheCreation5mTokens int
	CacheCreation1hTokens int
	ImageOutputTokens     int
}

// CostBreakdown 费用明细
type CostBreakdown struct {
	InputCost                 float64 // 文本输入费用（不含图片输入，图片输入单独记入 ImageInputCost）
	ImageInputCost            float64 // 图片输入 token 费用（如 gpt-image-2 图片编辑）
	OutputCost                float64
	ImageOutputCost           float64
	CacheCreationCost         float64
	CacheReadCost             float64
	TotalCost                 float64
	ActualCost                float64 // 应用倍率后的实际费用
	BillingMode               string  // 计费模式（"token"/"per_request"/"image"），由 CalculateCostUnified 填充
	LongContextBillingApplied bool
	// ServiceTierNotOffered 是请求了、但价卡不提供的服务档（如官方没公布 Ultrafast 价的模型收到
	// service_tier=ultrafast）；这类请求按标准价计费，由 CalculateCostUnified 记日志。空串表示无此情况。
	ServiceTierNotOffered string
	// CurrencyConversion 非空表示价卡不是记账币种：上面各项费用已按其中的汇率折算成记账币种
	// （落账到 usage_logs.currency_conversion）。nil 表示价卡就是记账币种。
	CurrencyConversion *CurrencyConversion
	// RateMultiplier 是本次实际施加的基础倍率（用户覆盖 ?? 分组默认，token 计费已含高峰因子；
	// 负值已钳为 0），由 CalculateCostUnified 填充，仅供不变式校验与落账快照。
	RateMultiplier float64
	// ModelRateMultiplier 是分组逐模型倍率因子，由 CalculateCostUnified 填充：
	// token 计费按 Group.ModelRateMultiplierFor(Model) 解析，未命中为 1；按次/图片/视频计费恒为 1。
	// 不变式：ActualCost = TotalCost × RateMultiplier × ModelRateMultiplier
	//（长上下文阈值拆分的超额部分另乘 extraMultiplier，是既有例外）。
	// 零值表示结果未经统一入口评估（CalculateImageCost 等直接构造的结果）。
	ModelRateMultiplier float64
}

func applyCostBreakdownMultiplier(cost *CostBreakdown, multiplier float64) {
	if cost == nil || multiplier == 1 {
		return
	}
	cost.InputCost *= multiplier
	cost.ImageInputCost *= multiplier
	cost.OutputCost *= multiplier
	cost.ImageOutputCost *= multiplier
	cost.CacheCreationCost *= multiplier
	cost.CacheReadCost *= multiplier
	cost.TotalCost *= multiplier
	cost.ActualCost *= multiplier
}

func reasoningEffortBillingMultiplier(effort string, multipliers map[string]float64) float64 {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort != "none" {
		effort = NormalizeMaxReasoningEffort(effort)
	}
	if effort == "" {
		return 1
	}
	multiplier := multipliers[effort]
	if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return 1
	}
	return multiplier
}

func resolvedChannelTimeMultiplier(resolved *ResolvedPricing, at time.Time) float64 {
	if resolved == nil || resolved.Source != PricingSourceChannel || resolved.channelPricing == nil {
		return 1
	}
	return resolved.channelPricing.TimePricing.MultiplierAt(at)
}

// ErrModelPricingUnavailable indicates that none of the configured pricing
// sources can price the requested model.
var ErrModelPricingUnavailable = errors.New("pricing not found")

// BillingService 计费服务
type BillingService struct {
	cfg            *config.Config
	pricingService *PricingService
	fallbackPrices map[string]*ModelPricing // 硬编码回退价格
	// fx 把非记账币种的价卡折算成记账币种（见 convertToAccountingCurrency）；未装配时记账币种按 USD，
	// 遇到非美元价卡按无价处理。
	fx *ExchangeRateService

	// fallbackWarnSeen 记录已打过 fallback 警告日志的(已小写化)模型名,
	// 让 "[Billing] Using fallback pricing" 每个模型每进程最多打一条,
	// 避免热路径上每请求刷屏(issue #3394)。零值即可用,无需在构造函数初始化。
	fallbackWarnSeen sync.Map
}

// NewBillingService 创建计费服务实例
func NewBillingService(cfg *config.Config, pricingService *PricingService) *BillingService {
	s := &BillingService{
		cfg:            cfg,
		pricingService: pricingService,
		fallbackPrices: make(map[string]*ModelPricing),
	}

	// 初始化硬编码回退价格（当动态价格不可用时使用）
	s.initFallbackPricing()

	return s
}

// SetExchangeRateService 装配汇率服务（wire 里调用）。
func (s *BillingService) SetExchangeRateService(fx *ExchangeRateService) {
	s.fx = fx
}

// ProvideBillingService 创建装配好汇率服务的计费服务。
func ProvideBillingService(cfg *config.Config, pricingService *PricingService, fx *ExchangeRateService) *BillingService {
	s := NewBillingService(cfg, pricingService)
	s.SetExchangeRateService(fx)
	return s
}

// exchangeRates 返回装配的汇率服务（可能为 nil：ExchangeRateService 的方法对 nil 接收者按记账币种 USD 处理）。
func (s *BillingService) exchangeRates() *ExchangeRateService {
	if s == nil {
		return nil
	}
	return s.fx
}

// accountingCurrency 返回站内记账币种。
func (s *BillingService) accountingCurrency(ctx context.Context) string {
	if s == nil || s.fx == nil {
		return DefaultBalanceCurrency
	}
	return s.fx.AccountingCurrency(ctx)
}

// convertToAccountingCurrency 把按 priceCurrency 算出的费用折算成记账币种（使用时刻 at 的汇率），并把折算依据
// 记在 breakdown 上。这是价格币种折算的唯一施加点：token / 按次 / 图片 / 视频计费都经 CalculateCostUnified 到这里。
// 取不到汇率时按无价处理（ErrModelPricingUnavailable：零成本落账 + 告警），不按 1:1 悄悄记账。
func (s *BillingService) convertToAccountingCurrency(ctx context.Context, priceCurrency string, at time.Time, breakdown *CostBreakdown) error {
	accounting := s.accountingCurrency(ctx)
	if breakdown == nil || priceCurrency == accounting {
		return nil
	}
	if s == nil || s.fx == nil {
		return fmt.Errorf("%w: price card is in %s but the accounting currency is %s and no exchange rate service is configured",
			ErrModelPricingUnavailable, priceCurrency, accounting)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, conversion, err := s.fx.Convert(ctx, 1, priceCurrency, accounting, at, FXBilling)
	if err != nil {
		return fmt.Errorf("%w: convert %s price card to %s: %v", ErrModelPricingUnavailable, priceCurrency, accounting, err)
	}
	applyCostBreakdownMultiplier(breakdown, conversion.Rate)
	breakdown.CurrencyConversion = conversion
	return nil
}

// ConvertPriceToAccounting 把以 priceCurrency 计的金额折算成记账币种（at 时刻、计费口径的汇率），供不经
// CalculateCostUnified 的场景（账号统计自定义规则、批量图片单价快照、模型广场展示）使用：与计费同一个折算点。
// 同币种时折算记录为 nil。
func (s *BillingService) ConvertPriceToAccounting(ctx context.Context, amount float64, priceCurrency string, at time.Time) (float64, *CurrencyConversion, error) {
	if at.IsZero() {
		at = time.Now()
	}
	breakdown := &CostBreakdown{TotalCost: amount}
	if err := s.convertToAccountingCurrency(ctx, priceCurrency, at, breakdown); err != nil {
		return 0, nil, err
	}
	return breakdown.TotalCost, breakdown.CurrencyConversion, nil
}

// resolvedPriceCurrency 返回解析出的价卡的标价币种：分组 / 渠道条目按其 currency，价格目录为 USD。
func resolvedPriceCurrency(resolved *ResolvedPricing) string {
	if resolved != nil && resolved.channelPricing != nil &&
		(resolved.Source == PricingSourceGroup || resolved.Source == PricingSourceChannel) {
		return pricingCurrency(resolved.channelPricing)
	}
	return CatalogPriceCurrency
}

// initFallbackPricing 初始化硬编码回退价格（当动态价格不可用时使用）
// 价格单位：USD per token（与LiteLLM格式一致）。
// OpenAI / Anthropic / DeepSeek 的模型不在这里：它们的价只来自官方价目录（pricing.official_file）。
func (s *BillingService) initFallbackPricing() {
	// Gemini 3.1 Pro
	s.fallbackPrices["gemini-3.1-pro"] = &ModelPricing{
		InputPricePerToken:         2e-6,   // $2 per MTok
		OutputPricePerToken:        12e-6,  // $12 per MTok
		CacheCreationPricePerToken: 2e-6,   // $2 per MTok
		CacheReadPricePerToken:     0.2e-6, // $0.20 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Gemini 3.6 Flash (Google AI pricing: $1.50 input / $7.50 output /
	// $0.15 cached input per MTok). Antigravity's -high/-low/-medium/-tiered
	// aliases are matched below so unavailable remote pricing never records
	// token-bearing requests at $0.
	s.fallbackPrices["gemini-3.6-flash"] = &ModelPricing{
		InputPricePerToken:     1.5e-6,
		OutputPricePerToken:    7.5e-6,
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}

	// Gemini 3.7 Flash (Google AI pricing: $0.75 input / $3.75 output /
	// $0.075 cached input per MTok, promotional through 2026-12-31; official
	// rates double to $1.50/$7.50/$0.15 from 2027-01-01). Antigravity's
	// -high/-low/-medium/-tiered aliases are matched below so unavailable
	// remote pricing never records token-bearing requests at $0.
	s.fallbackPrices["gemini-3.7-flash"] = &ModelPricing{
		InputPricePerToken:     0.75e-6,
		OutputPricePerToken:    3.75e-6,
		CacheReadPricePerToken: 0.075e-6,
		SupportsCacheBreakdown: false,
	}

	// Gemini 3.8 Flash (Google AI pricing: $0.75 input / $3.75 output /
	// $0.075 cached input per MTok, promotional through 2026-12-31; official
	// rates double to $1.50/$7.50/$0.15 from 2027-01-01). Antigravity's
	// -high/-low/-medium/-tiered aliases are matched below so unavailable
	// remote pricing never records token-bearing requests at $0.
	s.fallbackPrices["gemini-3.8-flash"] = &ModelPricing{
		InputPricePerToken:     0.75e-6,
		OutputPricePerToken:    3.75e-6,
		CacheReadPricePerToken: 0.075e-6,
		SupportsCacheBreakdown: false,
	}

	// ============================================================
	// 国产 LLM 兜底定价（数据源：各家官方定价页/USD 口径）
	// 顺序：智谱 GLM → 月之暗面 Kimi → MiniMax（DeepSeek 的价在官方价目录里）
	// 覆盖逻辑见同文件 getFallbackPricing()
	// ============================================================

	// ---- 智谱 GLM（Z.AI）----
	// Source: https://docs.z.ai/guides/overview/pricing (USD per 1M tokens)
	// 注意：CacheReadPricePerToken 即"缓存命中"价格，CacheCreationPricePerToken 留空（智谱未公开写入价，按 0 处理）。
	// GLM-4.6 与 GLM-4.5 在 z.ai 国际版上定价一致；GLM-4.5 国内按 ¥0.8/¥2，汇率换算后约 $0.112/$0.28，与国际版 $0.6/$2.2 不同，本分支采用国际版 USD 口径与现有 Claude/GPT 一致。
	// GLM-5.3 / GLM-5.2 与 GLM-5.1 在 z.ai 上同价。
	// GLM-5.3-Flash 列表价 $0.15/$0.50（2026-09-09 前五折促销，此处按列表价，与其它模型口径一致）。
	s.fallbackPrices["glm-5.3-flash"] = &ModelPricing{
		InputPricePerToken:     0.15e-6, // $0.15 per MTok
		OutputPricePerToken:    0.5e-6,  // $0.50 per MTok
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5.3"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5.2"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5.1"] = &ModelPricing{
		InputPricePerToken:     1.4e-6, // $1.40 per MTok
		OutputPricePerToken:    4.4e-6, // $4.40 per MTok
		CacheReadPricePerToken: 0.26e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5"] = &ModelPricing{
		InputPricePerToken:     1e-6, // $1.00 per MTok
		OutputPricePerToken:    3.2e-6,
		CacheReadPricePerToken: 0.2e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-5-turbo"] = &ModelPricing{
		InputPricePerToken:     1.2e-6,
		OutputPricePerToken:    4e-6,
		CacheReadPricePerToken: 0.24e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.7"] = &ModelPricing{
		InputPricePerToken:     0.6e-6, // $0.60 per MTok
		OutputPricePerToken:    2.2e-6,
		CacheReadPricePerToken: 0.11e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.7-flashx"] = &ModelPricing{
		InputPricePerToken:     0.07e-6, // $0.07 per MTok
		OutputPricePerToken:    0.4e-6,
		CacheReadPricePerToken: 0.01e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.6"] = &ModelPricing{
		InputPricePerToken:     0.6e-6, // $0.60 per MTok
		OutputPricePerToken:    2.2e-6,
		CacheReadPricePerToken: 0.11e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5"] = &ModelPricing{
		InputPricePerToken:     0.6e-6, // $0.60 per MTok
		OutputPricePerToken:    2.2e-6,
		CacheReadPricePerToken: 0.11e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5-x"] = &ModelPricing{
		InputPricePerToken:     2.2e-6, // $2.20 per MTok
		OutputPricePerToken:    8.9e-6,
		CacheReadPricePerToken: 0.45e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5-air"] = &ModelPricing{
		InputPricePerToken:     0.2e-6, // $0.20 per MTok
		OutputPricePerToken:    1.1e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.5-airx"] = &ModelPricing{
		InputPricePerToken:     1.1e-6,
		OutputPricePerToken:    4.5e-6,
		CacheReadPricePerToken: 0.22e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4-32b-0414-128k"] = &ModelPricing{
		InputPricePerToken:     0.1e-6, // $0.10 per MTok
		OutputPricePerToken:    0.1e-6,
		SupportsCacheBreakdown: false,
	}
	// GLM-4.5-Flash / GLM-4.7-Flash 在 z.ai 上为 Free，保留 zero-cost entry 防止未知 alias 误计费。
	s.fallbackPrices["glm-4.5-flash"] = &ModelPricing{
		InputPricePerToken:     0,
		OutputPricePerToken:    0,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["glm-4.7-flash"] = &ModelPricing{
		InputPricePerToken:     0,
		OutputPricePerToken:    0,
		SupportsCacheBreakdown: false,
	}

	// ---- 月之暗面 Kimi（K 系列）----
	// Source: https://platform.moonshot.cn/docs/pricing/overview (元/百万 tokens 口径)
	//       交叉验证：https://www.tmtpost.com/7961404.html (USD 口径)
	// Moonshot V1 (¥2/¥5/¥10 多 tier) 公开页未直接标注 USD 价，本分支不覆盖，避免误计价。
	// K2-0905 / K2-0711 官方页面未保留定价，不覆盖。
	// Kimi K3 国际站 USD 价目：https://platform.kimi.ai/docs/pricing/chat-k3.md
	// Kimi Code bare aliases（k3 / k3-256k）官方无按 token 价目；复用 API Platform
	// kimi-k3 档位作代理计费 fallback（同 kimi-for-coding 对 K2.6 的处理口径）。
	s.fallbackPrices["kimi-k3"] = &ModelPricing{
		InputPricePerToken:     3e-6,    // $3.00 per MTok (cache miss)
		OutputPricePerToken:    15e-6,   // $15.00 per MTok
		CacheReadPricePerToken: 0.30e-6, // $0.30 per MTok (cache hit)
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2.6"] = &ModelPricing{
		InputPricePerToken:     0.95e-6, // $0.95 per MTok (cache miss)
		OutputPricePerToken:    4e-6,    // $4.00 per MTok
		CacheReadPricePerToken: 0.15e-6, // $0.15 per MTok (cache hit, ¥1.10)
		SupportsCacheBreakdown: false,
	}
	// kimi-for-coding 走 Kimi Coding endpoint，按当前 K2.6 coding 档位兜底计费。
	s.fallbackPrices["kimi-for-coding"] = &ModelPricing{
		InputPricePerToken:     0.95e-6,
		OutputPricePerToken:    4e-6,
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2.5"] = &ModelPricing{
		InputPricePerToken:     0.60e-6, // $0.60 per MTok
		OutputPricePerToken:    3e-6,    // $3.00 per MTok
		CacheReadPricePerToken: 0.098e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2-thinking"] = &ModelPricing{
		InputPricePerToken:     0.56e-6, // ¥4/百万 ≈ $0.56
		OutputPricePerToken:    2.24e-6, // ¥16/百万
		CacheReadPricePerToken: 0.14e-6, // ¥1/百万
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["kimi-k2"] = &ModelPricing{
		InputPricePerToken:     0.56e-6, // ¥4/百万
		OutputPricePerToken:    2.24e-6, // ¥16/百万
		CacheReadPricePerToken: 0.14e-6, // ¥1/百万
		SupportsCacheBreakdown: false,
	}

	// ---- MiniMax M 系列 ----
	// Source: https://platform.minimax.io/docs/guides/pricing-paygo
	// 注意：MiniMax M3 在 >512K context 时价格翻倍，本兜底采用 ≤512K 标准 tier（保守口径，对用户有利）。
	// 如需支持长上下文 multiplier，可后续参考 GPT-5.4 模式扩展 LongContextXxx 字段。
	s.fallbackPrices["minimax-m3"] = &ModelPricing{
		InputPricePerToken:     0.60e-6, // $0.60 per MTok (≤512K standard tier, 含 50% 永久折扣前原价 $1.20)
		OutputPricePerToken:    2.40e-6,
		CacheReadPricePerToken: 0.12e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.7"] = &ModelPricing{
		InputPricePerToken:     0.30e-6, // $0.30 per MTok
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.06e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.7-highspeed"] = &ModelPricing{
		InputPricePerToken:     0.60e-6,
		OutputPricePerToken:    2.40e-6,
		CacheReadPricePerToken: 0.06e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.5"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2.1"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["minimax-m2"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    1.20e-6,
		CacheReadPricePerToken: 0.03e-6,
		SupportsCacheBreakdown: false,
	}

	// ---- 火山方舟 豆包 Embedding（多模态向量化）----
	// doubao-embedding-vision 图文向量化：上游 usage 回传 prompt_tokens_details.{text_tokens,image_tokens}，
	// 按量付费官方价 文本 ¥0.7/MTok、图片 ¥1.8/MTok；汇率口径 ÷7.14（与本表其他国产模型一致，¥1≈$0.14）。
	// embedding 无 output，OutputPricePerToken 置 0。
	s.fallbackPrices["doubao-embedding-vision"] = &ModelPricing{
		InputPricePerToken:      0.098e-6, // ¥0.7/MTok ≈ $0.098（文本输入）
		ImageInputPricePerToken: 0.252e-6, // ¥1.8/MTok ≈ $0.252（图片输入）
		OutputPricePerToken:     0,
		SupportsCacheBreakdown:  false,
	}

	// xAI Grok 4.5: $2 input / $0.30 cached input / $6 output below 200k;
	// long-context rates are $4 / $0.60 / $12 (>=200k prompt tokens).
	s.fallbackPrices["grok-4.5"] = &ModelPricing{
		InputPricePerToken:            2e-6,
		OutputPricePerToken:           6e-6,
		CacheReadPricePerToken:        0.3e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// xAI Grok 4.6: $2 input / $0.50 cached input / $6 output below 200k;
	// long-context rates are $4 / $1 / $12 (>=200k prompt tokens).
	s.fallbackPrices["grok-4.6"] = &ModelPricing{
		InputPricePerToken:            2e-6,
		OutputPricePerToken:           6e-6,
		CacheReadPricePerToken:        0.5e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// xAI Grok 4.7: $2 input / $0.50 cached input / $6 output below 200k;
	// long-context rates are $4 / $1 / $12 (>=200k prompt tokens).
	s.fallbackPrices["grok-4.7"] = &ModelPricing{
		InputPricePerToken:            2e-6,
		OutputPricePerToken:           6e-6,
		CacheReadPricePerToken:        0.5e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// xAI Grok 4.3: $1.25 input / $0.20 cached / $2.50 output below 200k;
	// long-context rates are $2.50 / $0.40 / $5.
	s.fallbackPrices["grok-4.3"] = &ModelPricing{
		InputPricePerToken:            1.25e-6,
		OutputPricePerToken:           2.5e-6,
		CacheReadPricePerToken:        0.2e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}
	// Grok 4.20 variants share the official $1.25 / $0.20 / $2.50 card
	// (and $2.50 / $0.40 / $5 long-context rates) with Grok 4.3.
	s.fallbackPrices["grok-4.20"] = &ModelPricing{
		InputPricePerToken:            1.25e-6,
		OutputPricePerToken:           2.5e-6,
		CacheReadPricePerToken:        0.2e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}

	// Keep legacy Grok 3 Mini requests on their own historical xAI price card;
	// otherwise the generic Grok fallback bills them as Grok 4.5.
	s.fallbackPrices["grok-3-mini"] = &ModelPricing{
		InputPricePerToken:     0.30e-6,
		OutputPricePerToken:    0.50e-6,
		CacheReadPricePerToken: 0.075e-6,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["grok-3-mini-fast"] = &ModelPricing{
		InputPricePerToken:     0.60e-6,
		OutputPricePerToken:    4e-6,
		CacheReadPricePerToken: 0.15e-6,
		SupportsCacheBreakdown: false,
	}
	// xAI Grok Build 0.1 (official docs: $1 input / $0.20 cached input /
	// $2 output per MTok). Composer is available only through Grok Build and
	// has no standalone public API rate card, so its aliases use this coding
	// model rate instead of silently billing at zero.
	s.fallbackPrices["grok-build-0.1"] = &ModelPricing{
		InputPricePerToken:            1e-6,
		OutputPricePerToken:           2e-6,
		CacheReadPricePerToken:        0.2e-6,
		SupportsCacheBreakdown:        false,
		LongContextInputThreshold:     200000,
		LongContextThresholdInclusive: true,
		LongContextInputMultiplier:    2,
		LongContextOutputMultiplier:   2,
	}
}

// getFallbackPricing 根据模型系列获取回退价格
func (s *BillingService) getFallbackPricing(model string) *ModelPricing {
	modelLower := strings.ToLower(model)
	if pricing, ok := s.fallbackPrices[modelLower]; ok && pricing != nil {
		return pricing
	}

	// 按模型系列匹配
	if strings.Contains(modelLower, "gemini-3.1-pro") || strings.Contains(modelLower, "gemini-3-1-pro") {
		return s.fallbackPrices["gemini-3.1-pro"]
	}
	if strings.Contains(modelLower, "gemini-3.6-flash") || strings.Contains(modelLower, "gemini-3-6-flash") {
		return s.fallbackPrices["gemini-3.6-flash"]
	}
	if strings.Contains(modelLower, "gemini-3.7-flash") || strings.Contains(modelLower, "gemini-3-7-flash") {
		return s.fallbackPrices["gemini-3.7-flash"]
	}
	if strings.Contains(modelLower, "gemini-3.8-flash") || strings.Contains(modelLower, "gemini-3-8-flash") {
		return s.fallbackPrices["gemini-3.8-flash"]
	}

	// ---- 国产 LLM 兜底匹配 ----
	// 匹配策略：长 key 优先（具体模型 → 系列 / 厂商），未知型号不回退以避免误计价。
	// 采用"白名单"语义：未在本表命中的国产模型 alias 一律不返回兜底价。

	// 智谱 GLM（z.ai 公开 SKU：glm-5.3 / glm-5.3-flash / glm-5.2 / glm-5.1 / glm-5 / glm-5-turbo / glm-4.7 / glm-4.6 / glm-4.5 等）
	// 匹配顺序：先判别最高 tier，再依次降级。
	// 注意：带小数点的型号必须排在裸 "glm-5" 之前，否则会被 strings.Contains 抢走；
	// glm-5.3-flash 必须排在 glm-5.3 之前（前者包含后者子串）。
	if strings.Contains(modelLower, "glm-5.3-flash") || strings.Contains(modelLower, "glm-5.3flash") {
		return s.fallbackPrices["glm-5.3-flash"]
	}
	if strings.Contains(modelLower, "glm-5.3") {
		return s.fallbackPrices["glm-5.3"]
	}
	if strings.Contains(modelLower, "glm-5.2") {
		return s.fallbackPrices["glm-5.2"]
	}
	if strings.Contains(modelLower, "glm-5.1") {
		return s.fallbackPrices["glm-5.1"]
	}
	if strings.Contains(modelLower, "glm-5-turbo") || strings.Contains(modelLower, "glm-5turbo") {
		return s.fallbackPrices["glm-5-turbo"]
	}
	if strings.Contains(modelLower, "glm-5") {
		return s.fallbackPrices["glm-5"]
	}
	if strings.Contains(modelLower, "glm-4.7-flashx") {
		return s.fallbackPrices["glm-4.7-flashx"]
	}
	if strings.Contains(modelLower, "glm-4.7-flash") {
		return s.fallbackPrices["glm-4.7-flash"]
	}
	if strings.Contains(modelLower, "glm-4.7") {
		return s.fallbackPrices["glm-4.7"]
	}
	if strings.Contains(modelLower, "glm-4.6") {
		return s.fallbackPrices["glm-4.6"]
	}
	if strings.Contains(modelLower, "glm-4.5-flash") {
		return s.fallbackPrices["glm-4.5-flash"]
	}
	if strings.Contains(modelLower, "glm-4.5-x") || strings.Contains(modelLower, "glm-4.5x") {
		return s.fallbackPrices["glm-4.5-x"]
	}
	if strings.Contains(modelLower, "glm-4.5-airx") || strings.Contains(modelLower, "glm-4.5airx") {
		return s.fallbackPrices["glm-4.5-airx"]
	}
	if strings.Contains(modelLower, "glm-4.5-air") || strings.Contains(modelLower, "glm-4.5air") {
		return s.fallbackPrices["glm-4.5-air"]
	}
	if strings.Contains(modelLower, "glm-4.5") {
		return s.fallbackPrices["glm-4.5"]
	}
	if strings.Contains(modelLower, "glm-4-32b") {
		return s.fallbackPrices["glm-4-32b-0414-128k"]
	}

	// 月之暗面 Kimi（kimi-k3 / k3 / k3-256k / kimi-k2.6 / kimi-for-coding / kimi-k2.5 / kimi-k2-thinking / kimi-k2）
	// K2-0905 / K2-0711 官方未保留定价，不进入 fallback。
	// K3 规则置于 K2 前：API Platform 仅官方 kimi-k3（及 / 路径后缀）；
	// Code bare aliases 仅精确 k3 / k3-256k 或 /k3|/k3-256k 后缀，避免 kimi-k30 等未知型号误命中。
	// 注意：kimi-k3[1m] 是 Claude Code 上下文选择语法，不是 Kimi API 模型 ID，不进入 fallback。
	if strings.Contains(modelLower, "kimi-for-coding") {
		return s.fallbackPrices["kimi-for-coding"]
	}
	if modelLower == "kimi-k3" || strings.HasSuffix(modelLower, "/kimi-k3") ||
		modelLower == "k3" || modelLower == "k3-256k" ||
		strings.HasSuffix(modelLower, "/k3") || strings.HasSuffix(modelLower, "/k3-256k") {
		return s.fallbackPrices["kimi-k3"]
	}
	if strings.Contains(modelLower, "kimi-k2.6") || strings.Contains(modelLower, "kimi-k2-6") {
		return s.fallbackPrices["kimi-k2.6"]
	}
	if strings.Contains(modelLower, "kimi-k2.5") || strings.Contains(modelLower, "kimi-k2-5") {
		return s.fallbackPrices["kimi-k2.5"]
	}
	if strings.Contains(modelLower, "kimi-k2-thinking") || strings.Contains(modelLower, "kimi-k2-thinking-") {
		return s.fallbackPrices["kimi-k2-thinking"]
	}
	if strings.Contains(modelLower, "kimi-k2") || strings.Contains(modelLower, "kimi/k2") {
		return s.fallbackPrices["kimi-k2"]
	}

	// MiniMax M 系列（M3 / M2.7 / M2.5 / M2.1 / M2；含 highspeed 变体）
	if strings.Contains(modelLower, "minimax-m3") {
		return s.fallbackPrices["minimax-m3"]
	}
	if strings.Contains(modelLower, "minimax-m2.7-highspeed") || strings.Contains(modelLower, "minimax-m2-7-highspeed") {
		return s.fallbackPrices["minimax-m2.7-highspeed"]
	}
	if strings.Contains(modelLower, "minimax-m2.7") || strings.Contains(modelLower, "minimax-m2-7") {
		return s.fallbackPrices["minimax-m2.7"]
	}
	if strings.Contains(modelLower, "minimax-m2.5") || strings.Contains(modelLower, "minimax-m2-5") {
		return s.fallbackPrices["minimax-m2.5"]
	}
	if strings.Contains(modelLower, "minimax-m2.1") || strings.Contains(modelLower, "minimax-m2-1") {
		return s.fallbackPrices["minimax-m2.1"]
	}
	if strings.Contains(modelLower, "minimax-m2") || strings.Contains(modelLower, "minimax-m-2") {
		return s.fallbackPrices["minimax-m2"]
	}

	// 火山方舟 豆包 Embedding（多模态向量化）。
	// most-specific-first：放在未来任何 doubao-embedding / doubao 宽匹配之前。
	// 覆盖带版本后缀的别名（如 doubao-embedding-vision-251215）。
	if strings.Contains(modelLower, "doubao-embedding-vision") {
		return s.fallbackPrices["doubao-embedding-vision"]
	}

	switch modelLower {
	case "grok", "grok-latest", "grok-4.6", "grok-4.6-latest":
		return s.fallbackPrices["grok-4.6"]
	case "grok-4.7", "grok-4.7-latest":
		return s.fallbackPrices["grok-4.7"]
	case "grok-4.5", "grok-4.5-latest":
		return s.fallbackPrices["grok-4.5"]
	case "grok-3-mini":
		return s.fallbackPrices["grok-3-mini"]
	case "grok-3-mini-fast":
		return s.fallbackPrices["grok-3-mini-fast"]
	case "grok-4.3":
		return s.fallbackPrices["grok-4.3"]
	case "grok-4.20-0309-reasoning",
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-multi-agent-0309",
		"grok-4.20-reasoning",
		"grok-4.20-non-reasoning":
		return s.fallbackPrices["grok-4.20"]
	case "grok-build", "grok-build-latest", "grok-build-0.1", "grok-composer", "grok-composer-2.5-fast", "composer-2.5":
		return s.fallbackPrices["grok-build-0.1"]
	}

	// Unknown Grok text IDs (grok-5, dated snapshots, provider-prefixed) inherit
	// the current default text card so a new model cannot ship unbilled.
	if pricing := s.grokUnknownTextFamilyFallback(modelLower); pricing != nil {
		return pricing
	}

	return nil
}

func (s *BillingService) grokUnknownTextFamilyFallback(model string) *ModelPricing {
	if s == nil || !isGrokUnknownTextFamilyModel(model) {
		return nil
	}
	return s.fallbackPrices["grok-4.6"]
}

func isGrokUnknownTextFamilyModel(model string) bool {
	native := strings.ToLower(strings.TrimSpace(xai.StripGrokProviderPrefix(model)))
	if isGrokMediaFamilyModel(native) {
		return false
	}
	switch {
	case native == "grok", native == "grok-latest":
		return true
	case strings.HasPrefix(native, "grok-build"),
		strings.HasPrefix(native, "grok-composer"),
		strings.HasPrefix(native, "composer-"):
		return true
	case len(native) > 5 && strings.HasPrefix(native, "grok-"):
		rest := native[len("grok-"):]
		return rest[0] >= '0' && rest[0] <= '9'
	default:
		return false
	}
}

// isGrokMediaFamilyModel matches ids that are billed per image/video/audio unit
// rather than per token, so version-numbered media ids (grok-2-image-1212,
// grok-5-video) cannot slip into the unknown-text fallback and pick up a token
// card. "vision" is deliberately absent: multimodal chat models are token billed.
func isGrokMediaFamilyModel(native string) bool {
	for _, marker := range []string{"imagine", "image", "video", "audio", "speech", "tts", "transcribe", "realtime"} {
		if strings.Contains(native, marker) {
			return true
		}
	}
	return false
}

// HasIdentifiedTokenPricing 判断模型能否在价格表中被"确定性识别"出 token 价格。
//
// 与 GetModelPricing 的关键区别：本函数拒绝按子串猜系列的兜底。GetModelPricing 会
// 让任意含 "haiku"/"opus"/"claude" 的名字（哪怕是不存在的型号）落到 getFallbackPricing
// 的系列兜底价上，因此凡是模型名来自外部、且"能查到价"会直接影响计费金额的场景
// （如按上游响应自报模型计费），都必须用本函数而不是 GetModelPricing 做准入判断。
func (s *BillingService) HasIdentifiedTokenPricing(model string) bool {
	if s == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if s.pricingService != nil {
		// 仅有图片价的条目不能用于 token 计费，口径与 GetModelPricing 保持一致。
		if pricing := s.pricingService.GetIdentifiedModelPricing(model); pricing != nil && !pricing.TokenPricingAbsent {
			return true
		}
	}
	pricing, ok := s.fallbackPrices[model]
	return ok && pricing != nil
}

// GetModelPricing 获取模型价格配置。价卡原样来自价格服务（官方价目录 → 远端 / 回退目录）或硬编码回退，
// 这里不再按模型改价：官方价目录里的模型，计费结果只由目录里的数字和倍率决定。
func (s *BillingService) GetModelPricing(model string) (*ModelPricing, error) {
	// 标准化模型名称（转小写）
	model = strings.ToLower(model)

	// 1. 优先从动态价格服务获取
	if s.pricingService != nil {
		litellmPricing := s.pricingService.GetModelPricing(model)
		// 仅有图片价、无 token 价的条目（如 LiteLLM 的 imagen 类模型）不能用于
		// token 计费：直接返回会把 token 流量按 $0 计费。跳过后走 fallback，
		// 无 fallback 则 fail-closed（ErrModelPricingUnavailable）。
		// 图片计费路径（getDefaultImagePrice / getImageUnitPrice）直接读
		// PricingService，不受影响。
		if litellmPricing != nil && litellmPricing.TokenPricingAbsent {
			litellmPricing = nil
		}
		if litellmPricing != nil {
			// 启用 5m/1h 分类计费的条件：
			// 1. 存在 1h 价格
			// 2. 1h 价格 > 5m 价格（防止 LiteLLM 数据错误导致少收费）
			price5m := litellmPricing.CacheCreationInputTokenCost
			price1h := litellmPricing.CacheCreationInputTokenCostAbove1hr
			enableBreakdown := price1h > 0 && price1h > price5m
			return &ModelPricing{
				InputPricePerToken:                 litellmPricing.InputCostPerToken,
				InputPricePerTokenPriority:         litellmPricing.InputCostPerTokenPriority,
				OutputPricePerToken:                litellmPricing.OutputCostPerToken,
				OutputPricePerTokenPriority:        litellmPricing.OutputCostPerTokenPriority,
				CacheCreationPricePerToken:         litellmPricing.CacheCreationInputTokenCost,
				CacheCreationPricePerTokenPriority: litellmPricing.CacheCreationInputTokenCostPriority,
				CacheReadPricePerToken:             litellmPricing.CacheReadInputTokenCost,
				CacheReadPricePerTokenPriority:     litellmPricing.CacheReadInputTokenCostPriority,
				CacheCreation5mPrice:               price5m,
				CacheCreation1hPrice:               price1h,
				SupportsCacheBreakdown:             enableBreakdown,
				LongContextInputThreshold:          litellmPricing.LongContextInputTokenThreshold,
				// xAI 的长上下文阈值语义为"达到即进高档"（LiteLLM 同口径），其余提供商为严格大于。
				LongContextThresholdInclusive: strings.EqualFold(litellmPricing.LiteLLMProvider, "xai"),
				LongContextInputMultiplier:    litellmPricing.LongContextInputCostMultiplier,
				LongContextOutputMultiplier:   litellmPricing.LongContextOutputCostMultiplier,
				ImageInputPricePerToken:       litellmPricing.InputCostPerImageToken,
				ImageCacheReadPricePerToken:   litellmPricing.CacheReadInputImageTokenCost,
				ImageOutputPricePerToken:      litellmPricing.OutputCostPerImageToken,
				ServiceTierMultipliers:        maps.Clone(litellmPricing.ServiceTierMultipliers),
			}, nil
		}
	}

	// 2. 使用硬编码回退价格
	fallback := s.getFallbackPricing(model)
	if fallback != nil {
		// 按模型名去重:每个模型每进程最多打一条 warn,避免热路径每请求刷屏（issue #3394）。
		// model 在函数入口已 ToLower,故 GLM-5.2 / glm-5.2 视为同一条目。
		if _, seen := s.fallbackWarnSeen.LoadOrStore(model, struct{}{}); !seen {
			log.Printf("[Billing] Using fallback pricing for model: %s", model)
		}
		return fallback, nil
	}

	return nil, fmt.Errorf("%w for model: %s", ErrModelPricingUnavailable, model)
}

// GetModelPricingWithChannel 获取模型定价，渠道配置的价格覆盖默认值
// 渠道存在时，未配置的图片输出价格归零（不回退到 LiteLLM）
func (s *BillingService) GetModelPricingWithChannel(model string, channelPricing *ChannelModelPricing) (*ModelPricing, error) {
	pricing, err := s.GetModelPricing(model)
	if err != nil {
		return nil, err
	}
	if channelPricing == nil {
		return pricing, nil
	}
	// 防止修改 fallbackPrices 中的共享指针
	cloned := *pricing
	pricing = &cloned
	applyChannelTokenPriceOverrides(pricing, channelPricing)
	pricing.FastMultiplier = channelPricing.FastMultiplier
	pricing.FlexMultiplier = channelPricing.FlexMultiplier
	pricing.ReasoningEffortMultipliers = maps.Clone(channelPricing.ReasoningEffortMultipliers)
	if channelPricing.ImageOutputPrice != nil {
		pricing.ImageOutputPricePerToken = *channelPricing.ImageOutputPrice
	} else {
		pricing.ImageOutputPricePerToken = 0
	}
	pricing.ImageOutputPriceExplicit = true
	applyChannelImageInputPrice(channelPricing, pricing)
	return pricing, nil
}

// channelTierOverridePrice applies a Standard-tier override while preserving
// an explicit model-catalog Fast/Priority ratio. If the catalog has no tier
// price, generic service-tier defaults remain responsible for the fallback.
func channelTierOverridePrice(baseStandard, baseTier, channelStandard float64) float64 {
	if baseStandard > 0 && baseTier > 0 {
		return channelStandard * (baseTier / baseStandard)
	}
	return 0
}

func applyChannelTokenPriceOverrides(pricing *ModelPricing, channelPricing *ChannelModelPricing) {
	if pricing == nil || channelPricing == nil {
		return
	}
	if channelPricing.InputPrice != nil {
		priority := channelTierOverridePrice(pricing.InputPricePerToken, pricing.InputPricePerTokenPriority, *channelPricing.InputPrice)
		pricing.InputPricePerToken = *channelPricing.InputPrice
		pricing.InputPricePerTokenPriority = priority
	}
	if channelPricing.OutputPrice != nil {
		priority := channelTierOverridePrice(pricing.OutputPricePerToken, pricing.OutputPricePerTokenPriority, *channelPricing.OutputPrice)
		pricing.OutputPricePerToken = *channelPricing.OutputPrice
		pricing.OutputPricePerTokenPriority = priority
	}
	if channelPricing.CacheWritePrice != nil {
		priority := channelTierOverridePrice(pricing.CacheCreationPricePerToken, pricing.CacheCreationPricePerTokenPriority, *channelPricing.CacheWritePrice)
		pricing.CacheCreationPricePerToken = *channelPricing.CacheWritePrice
		pricing.CacheCreationPricePerTokenPriority = priority
		pricing.CacheCreation5mPrice = *channelPricing.CacheWritePrice
		if channelPricing.CacheWrite1hPrice == nil {
			// Preserve the pre-split behavior for existing configurations: a lone
			// cache_write_price continues to override both TTL tiers.
			pricing.CacheCreation1hPrice = *channelPricing.CacheWritePrice
		}
	}
	if channelPricing.CacheWrite1hPrice != nil {
		pricing.CacheCreation1hPrice = *channelPricing.CacheWrite1hPrice
		pricing.SupportsCacheBreakdown = true
	}
	if channelPricing.CacheReadPrice != nil {
		priority := channelTierOverridePrice(pricing.CacheReadPricePerToken, pricing.CacheReadPricePerTokenPriority, *channelPricing.CacheReadPrice)
		pricing.CacheReadPricePerToken = *channelPricing.CacheReadPrice
		pricing.CacheReadPricePerTokenPriority = priority
	}
}

// --- 统一计费入口 ---

// CostInput 统一计费输入
type CostInput struct {
	Ctx                       context.Context
	Model                     string
	GroupID                   *int64 // 用于渠道定价查找
	Group                     *Group
	Tokens                    UsageTokens
	RequestCount              int     // 按次计费时使用
	UsageUnits                float64 // 音频等连续计量单位（分钟/小时/百万字符）
	SizeTier                  string  // 按次/图片模式的层级标签（"1K","2K","4K","HD" 等）
	RateMultiplier            float64
	PricingAt                 time.Time             // 渠道分时定价使用的计费时刻
	ServiceTier               string                // "priority","flex","" 等
	ReasoningEffort           string                // 最终转发的推理等级，按匹配定价中配置的等级倍率计费
	Resolver                  *ModelPricingResolver // 定价解析器
	Resolved                  *ResolvedPricing      // 可选：预解析的定价结果（避免重复 Resolve 调用）
	LongContextBillingEnabled *bool
}

// CalculateCostUnified 是带分组的 token 计费唯一入口，支持三种计费模式：
// 先按 Resolver/内置定价/长上下文阈值拆分算出基础结果，再在唯一的一处乘入分组逐模型
// 倍率因子（applyGroupModelRateMultiplier）。两条网关（Anthropic 与 OpenAI/Grok）计算
// 任何 token 费用都必须经过这里——任何绕过本函数的路径都会让逐模型倍率在该平台静默失效。
func (s *BillingService) CalculateCostUnified(input CostInput) (*CostBreakdown, error) {
	// 保存时强制 > 0；若仍有负数泄漏（缓存/迁移残留），按 0 处理避免按 1x 误扣。
	if input.RateMultiplier < 0 {
		input.RateMultiplier = 0
	}
	breakdown, priceCurrency, err := s.calculateBaseCostUnified(input)
	if err != nil || breakdown == nil {
		return breakdown, err
	}
	pricingAt := input.PricingAt
	if pricingAt.IsZero() {
		pricingAt = time.Now()
	}
	if err := s.convertToAccountingCurrency(input.Ctx, priceCurrency, pricingAt, breakdown); err != nil {
		return nil, err
	}
	applyGroupModelRateMultiplier(breakdown, input)
	if breakdown.ServiceTierNotOffered != "" {
		slog.Info("billing.service_tier_not_offered",
			"model", input.Model,
			"requested_tier", normalizeBillingServiceTier(input.ServiceTier),
			"billed_tier", "standard",
			"reason", "the model's price card does not offer this service tier (no official price), billed at standard rates")
	}
	return breakdown, nil
}

// calculateBaseCostUnified 计算不含逐模型因子的基础费用（ActualCost = TotalCost × RateMultiplier），
// 金额以价卡的标价币种计，一并返回该币种。
func (s *BillingService) calculateBaseCostUnified(input CostInput) (*CostBreakdown, string, error) {
	if input.Resolver == nil {
		// 无 Resolver：内置定价路径，长上下文阶梯由定价目录驱动
		// （上游 0.2.x 起网关不再单独传阈值，见 CalculateTokenCostForRequest）。
		applyLongContextBilling := true
		if input.LongContextBillingEnabled != nil {
			applyLongContextBilling = *input.LongContextBillingEnabled
		}
		pricing, err := s.GetModelPricing(input.Model)
		if err != nil {
			return nil, "", err
		}
		breakdown := s.computeTokenBreakdown(pricing, input.Tokens, input.RateMultiplier, input.ServiceTier, applyLongContextBilling)
		applyCostBreakdownMultiplier(breakdown, reasoningEffortBillingMultiplier(input.ReasoningEffort, pricing.ReasoningEffortMultipliers))
		breakdown.BillingMode = string(BillingModeToken)
		return breakdown, CatalogPriceCurrency, nil
	}

	// 优先使用预解析结果，避免重复 Resolve 调用
	resolved := input.Resolved
	if resolved == nil {
		resolved = input.Resolver.Resolve(input.Ctx, PricingInput{
			Model:   input.Model,
			GroupID: input.GroupID,
			Group:   input.Group,
		})
	}

	var breakdown *CostBreakdown
	var err error
	switch resolved.Mode {
	case BillingModePerRequest, BillingModeImage, BillingModeVideo:
		breakdown, err = s.calculatePerRequestCost(resolved, input)
		if err == nil && resolved.channelPricing != nil {
			applyCostBreakdownMultiplier(breakdown, reasoningEffortBillingMultiplier(input.ReasoningEffort, resolved.channelPricing.ReasoningEffortMultipliers))
		}
	default: // BillingModeToken
		breakdown, err = s.calculateTokenCost(resolved, input)
	}
	if err == nil && breakdown != nil {
		breakdown.BillingMode = string(resolved.Mode)
		if breakdown.BillingMode == "" {
			breakdown.BillingMode = string(BillingModeToken)
		}
	}
	return breakdown, resolvedPriceCurrency(resolved), err
}

// applyGroupModelRateMultiplier 是分组逐模型倍率在计费侧的唯一施加点：
// ActualCost 乘入因子，并把实际施加的两个倍率写回 breakdown 供落账快照与不变式校验。
//
// 对所有计费模式一视同仁（token / 按次 / 图片 / 视频），不为任何模式开口子。
// 原因是 usage_logs 一行只有一个 model_rate_multiplier 列，而落账不变式
// ActualCost = TotalCost × RateMultiplier × ModelRateMultiplier 是对整行声明的：
// 只要有一个组成部分不吃这个因子，这一行记下的倍率就不再描述本行的金额，
// 按该关系做对账、分成或退款的下游会直接算错。运营者配 "claude-opus-* → 2×"
// 的语义也就是"这个分组对这些模型按 2 倍计费"，没有按计费模式再分一层的说法。
func applyGroupModelRateMultiplier(breakdown *CostBreakdown, input CostInput) {
	breakdown.RateMultiplier = input.RateMultiplier
	factor := resolveGroupModelRateMultiplier(input.Group, input.Model)
	breakdown.ModelRateMultiplier = factor
	if factor != 1 {
		breakdown.ActualCost *= factor
	}
}

// mergeRequestSurcharge 把附加计费（web search 这类叠加项）并入本次请求的费用。
//
// surcharge 由 CalculateSearchCost 这类函数单独算出，走不到 CalculateCostUnified，
// 因此天然不带逐模型因子。直接相加会得到一行 ActualCost 里"token 吃了因子、
// surcharge 没吃"的混合金额，而 usage_logs 既没有 search_count 也没有分项成本，
// 事后无法把这种行和纯 token 行区分开——不变式一旦对某些行不成立，它对所有行都
// 失去意义。所以在这里补乘同一个因子：base 非空时直接沿用它已解析出的
// ModelRateMultiplier（同一次解析，不可能分叉），纯 surcharge 行才回到唯一解析点。
func mergeRequestSurcharge(base *CostBreakdown, surcharge *CostBreakdown, group *Group, model string) *CostBreakdown {
	if surcharge == nil || (surcharge.TotalCost == 0 && surcharge.ActualCost == 0) {
		return base
	}
	factor := resolveGroupModelRateMultiplier(group, model)
	if base != nil {
		factor = base.ModelRateMultiplier
	}
	if factor != 1 {
		surcharge.ActualCost *= factor
	}
	if base == nil {
		surcharge.ModelRateMultiplier = factor
		return surcharge
	}
	base.TotalCost += surcharge.TotalCost
	base.ActualCost += surcharge.ActualCost
	return base
}

// calculateTokenCost 按 token 区间计费
func (s *BillingService) calculateTokenCost(resolved *ResolvedPricing, input CostInput) (*CostBreakdown, error) {
	totalContext := input.Tokens.InputTokens + input.Tokens.CacheCreationTokens + input.Tokens.CacheReadTokens

	// 分组开关是统一入口；账号 API 开关保留为额外开启能力，但 false 不否决分组配置。
	contextTierPricingEnabled := resolved.longContextPricingEnabled
	if input.LongContextBillingEnabled != nil && *input.LongContextBillingEnabled {
		contextTierPricingEnabled = true
	}

	pricingContext := totalContext
	if !contextTierPricingEnabled {
		// 渠道可能显式配置了第一档，也可能只配置高上下文档。用 1 token
		// 选择最低档；未命中时自然回退到渠道基础价。
		pricingContext = 1
	}
	pricing := input.Resolver.GetIntervalPricing(resolved, pricingContext)
	if pricing == nil {
		return nil, fmt.Errorf("no pricing available for model: %s: %w", input.Model, ErrModelPricingUnavailable)
	}

	// 官方长上下文阶梯仅在无区间定价时应用（区间定价已包含上下文分层）。
	applyLongCtx := len(resolved.Intervals) == 0 && contextTierPricingEnabled

	breakdown := s.computeTokenBreakdown(pricing, input.Tokens, input.RateMultiplier, input.ServiceTier, applyLongCtx)
	applyCostBreakdownMultiplier(breakdown, resolvedChannelTimeMultiplier(resolved, input.PricingAt))
	applyCostBreakdownMultiplier(breakdown, reasoningEffortBillingMultiplier(input.ReasoningEffort, pricing.ReasoningEffortMultipliers))
	return breakdown, nil
}

// computeTokenBreakdown 是 token 计费的核心逻辑，由 calculateTokenCost 和 calculateCostInternal 共用。
// applyLongCtx 控制是否检查长上下文定价（区间定价已自含上下文分层，不需要额外应用）。
func (s *BillingService) computeTokenBreakdown(
	pricing *ModelPricing, tokens UsageTokens,
	rateMultiplier float64, serviceTier string,
	applyLongCtx bool,
) *CostBreakdown {
	// 保存时强制 > 0；若仍有负数泄漏，按 0 处理避免按 1x 误扣。
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}

	inputPrice := pricing.InputPricePerToken
	outputPrice := pricing.OutputPricePerToken
	cacheReadPrice := pricing.CacheReadPricePerToken
	cacheCreationPrice := pricing.CacheCreationPricePerToken
	cacheCreationMultiplier := 1.0
	tierMultiplier := 1.0
	tierOffered := true

	if usePriorityServiceTierPricing(serviceTier, pricing) {
		if pricing.InputPricePerTokenPriority > 0 {
			inputPrice = pricing.InputPricePerTokenPriority
		}
		if pricing.OutputPricePerTokenPriority > 0 {
			outputPrice = pricing.OutputPricePerTokenPriority
		}
		if pricing.CacheReadPricePerTokenPriority > 0 {
			cacheReadPrice = pricing.CacheReadPricePerTokenPriority
		}
		if pricing.CacheCreationPricePerTokenPriority > 0 {
			cacheCreationPrice = pricing.CacheCreationPricePerTokenPriority
		}
	} else {
		tierMultiplier, tierOffered = resolveServiceTierMultiplier(serviceTier, pricing)
	}

	longContextPricingEligible := applyLongCtx && s.shouldApplySessionLongContextPricing(tokens, pricing)
	var baselineCost *CostBreakdown
	if longContextPricingEligible {
		baselineCost = s.computeTokenBreakdown(pricing, tokens, rateMultiplier, serviceTier, false)
		// 倍率 ≤0 表示该项未配置（目录/覆写条目可能只写了 input 或 output 一侧），
		// 按 1 计而不是乘 0：乘 0 会把超阈值请求的对应分项算成免费。
		longCtxInputMultiplier := longContextMultiplierOrOne(pricing.LongContextInputMultiplier)
		inputPrice *= longCtxInputMultiplier
		outputPrice *= longContextMultiplierOrOne(pricing.LongContextOutputMultiplier)
		// 缓存读取本质上是输入侧的复用，应与 input 一同应用长上下文倍率；
		// 否则 cache hit 越多，少计的费用越多（见 #2293）。
		cacheReadPrice *= longCtxInputMultiplier
		// 缓存创建（cache_write）也是输入侧操作，三档价格（标准 / 5m / 1h）
		// 都通过 computeCacheCreationCost 直接读取 pricing.*，不会经过这里
		// 的倍率修改，因此显式向下传一个倍率，避免长上下文场景下被漏乘。
		cacheCreationMultiplier = longCtxInputMultiplier
	}

	bd := &CostBreakdown{}
	// 分离图片输入 token 与文本输入 token（多模态 embedding、图片编辑等图文不同价场景）。
	// InputCost 仅计文本输入，图片输入费用单独记入 ImageInputCost，便于对账；总额不变。
	// ImageInputTokens 为 0 时（绝大多数 chat/vision 流量）走原始单价路径，行为不变。
	if tokens.ImageInputTokens > 0 {
		imageInputTokens := tokens.ImageInputTokens
		textInputTokens := tokens.InputTokens - imageInputTokens
		if textInputTokens < 0 {
			textInputTokens = 0
			imageInputTokens = tokens.InputTokens
		}
		imageInputPrice := pricing.ImageInputPricePerToken
		if imageInputPrice == 0 {
			// 未配置图片输入档时回退到文本 input 价（已含 priority / 长上下文调整）
			imageInputPrice = inputPrice
		}
		bd.InputCost = float64(textInputTokens) * inputPrice
		bd.ImageInputCost = float64(imageInputTokens) * imageInputPrice
	} else {
		bd.InputCost = float64(tokens.InputTokens) * inputPrice
	}

	// 分离图片输出 token 与文本输出 token
	textOutputTokens := tokens.OutputTokens - tokens.ImageOutputTokens
	if textOutputTokens < 0 {
		textOutputTokens = 0
	}
	bd.OutputCost = float64(textOutputTokens) * outputPrice

	// 图片输出 token 费用（独立费率）
	if tokens.ImageOutputTokens > 0 {
		imgPrice := pricing.ImageOutputPricePerToken
		if imgPrice == 0 && !pricing.ImageOutputPriceExplicit {
			imgPrice = outputPrice
		}
		bd.ImageOutputCost = float64(tokens.ImageOutputTokens) * imgPrice
	}

	// 缓存创建费用
	bd.CacheCreationCost = s.computeCacheCreationCost(pricing, tokens, cacheCreationPrice, cacheCreationMultiplier)

	bd.CacheReadCost = float64(tokens.CacheReadTokens) * cacheReadPrice
	if imageCached := min(max(tokens.ImageCacheReadTokens, 0), max(tokens.CacheReadTokens, 0)); imageCached > 0 && pricing.ImageCacheReadPricePerToken > 0 {
		bd.CacheReadCost = float64(tokens.CacheReadTokens-imageCached)*cacheReadPrice + float64(imageCached)*pricing.ImageCacheReadPricePerToken
	}

	if tierMultiplier != 1.0 {
		bd.InputCost *= tierMultiplier
		bd.ImageInputCost *= tierMultiplier
		bd.OutputCost *= tierMultiplier
		bd.ImageOutputCost *= tierMultiplier
		bd.CacheCreationCost *= tierMultiplier
		bd.CacheReadCost *= tierMultiplier
	}

	bd.TotalCost = bd.InputCost + bd.ImageInputCost + bd.OutputCost + bd.ImageOutputCost +
		bd.CacheCreationCost + bd.CacheReadCost
	bd.ActualCost = bd.TotalCost * rateMultiplier
	bd.LongContextBillingApplied = baselineCost != nil && bd.ActualCost > baselineCost.ActualCost
	if !tierOffered {
		bd.ServiceTierNotOffered = canonicalBillingServiceTier(serviceTier)
	}

	return bd
}

// computeCacheCreationCost 计算缓存创建费用（支持 5m/1h 分类或标准计费）。
// multiplier 用于长上下文等场景下的整体价格缩放（普通调用传 1.0 即可）。
func (s *BillingService) computeCacheCreationCost(pricing *ModelPricing, tokens UsageTokens, price, multiplier float64) float64 {
	if pricing.SupportsCacheBreakdown && (pricing.CacheCreation5mPrice > 0 || pricing.CacheCreation1hPrice > 0) {
		cacheCreation5mTokens, cacheCreation1hTokens := normalizeCacheCreationBreakdown(tokens)
		if cacheCreation5mTokens == 0 && cacheCreation1hTokens == 0 && tokens.CacheCreationTokens > 0 {
			// API 未返回 ephemeral 明细，回退到全部按 5m 单价计费
			return float64(tokens.CacheCreationTokens) * pricing.CacheCreation5mPrice * multiplier
		}
		return float64(cacheCreation5mTokens)*pricing.CacheCreation5mPrice*multiplier +
			float64(cacheCreation1hTokens)*pricing.CacheCreation1hPrice*multiplier
	}
	return float64(tokens.CacheCreationTokens) * price * multiplier
}

// normalizeCacheCreationBreakdown caps contradictory 5m/1h details at an explicitly
// positive aggregate while retaining their reported ratio as closely as integer tokens allow.
func normalizeCacheCreationBreakdown(tokens UsageTokens) (int, int) {
	cacheCreation5mTokens := tokens.CacheCreation5mTokens
	cacheCreation1hTokens := tokens.CacheCreation1hTokens
	aggregate := tokens.CacheCreationTokens
	if cacheCreation5mTokens < 0 {
		cacheCreation5mTokens = 0
	}
	if cacheCreation1hTokens < 0 {
		cacheCreation1hTokens = 0
	}
	if aggregate <= 0 || (cacheCreation5mTokens <= aggregate && cacheCreation1hTokens <= aggregate-cacheCreation5mTokens) {
		return cacheCreation5mTokens, cacheCreation1hTokens
	}

	detailTotal := float64(cacheCreation5mTokens) + float64(cacheCreation1hTokens)
	normalized5mTokens := math.Round(float64(aggregate) * float64(cacheCreation5mTokens) / detailTotal)
	if normalized5mTokens >= float64(aggregate) {
		cacheCreation5mTokens = aggregate
	} else {
		cacheCreation5mTokens = int(normalized5mTokens)
	}
	return cacheCreation5mTokens, aggregate - cacheCreation5mTokens
}

// calculatePerRequestCost 按次/图片计费
func (s *BillingService) calculatePerRequestCost(resolved *ResolvedPricing, input CostInput) (*CostBreakdown, error) {
	units := input.UsageUnits
	if units <= 0 {
		count := input.RequestCount
		if count <= 0 {
			count = 1
		}
		units = float64(count)
	}

	var unitPrice float64

	if input.SizeTier != "" {
		unitPrice = input.Resolver.GetRequestTierPrice(resolved, input.SizeTier)
	}

	if unitPrice == 0 {
		totalContext := input.Tokens.InputTokens + input.Tokens.CacheCreationTokens + input.Tokens.CacheReadTokens
		unitPrice = input.Resolver.GetRequestTierPriceByContext(resolved, totalContext)
	}

	// 回退到默认按次价格
	if unitPrice == 0 {
		unitPrice = resolved.DefaultPerRequestPrice
	}

	totalCost := unitPrice * units
	actualCost := totalCost * input.RateMultiplier

	return &CostBreakdown{
		TotalCost:  totalCost,
		ActualCost: actualCost,
	}, nil
}

// CalculateCost 计算使用费用
func (s *BillingService) CalculateCost(model string, tokens UsageTokens, rateMultiplier float64) (*CostBreakdown, error) {
	return s.calculateCostInternal(model, tokens, rateMultiplier, "", nil)
}

func (s *BillingService) CalculateCostWithServiceTier(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string) (*CostBreakdown, error) {
	return s.calculateCostInternal(model, tokens, rateMultiplier, serviceTier, nil)
}

func (s *BillingService) calculateCostInternal(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string, channelPricing *ChannelModelPricing) (*CostBreakdown, error) {
	return s.calculateCostInternalWithPolicy(model, tokens, rateMultiplier, serviceTier, channelPricing, true)
}

func (s *BillingService) calculateCostInternalWithPolicy(
	model string,
	tokens UsageTokens,
	rateMultiplier float64,
	serviceTier string,
	channelPricing *ChannelModelPricing,
	longContextBillingEnabled bool,
) (*CostBreakdown, error) {
	var pricing *ModelPricing
	var err error
	if channelPricing != nil {
		pricing, err = s.GetModelPricingWithChannel(model, channelPricing)
	} else {
		pricing, err = s.GetModelPricing(model)
	}
	if err != nil {
		return nil, err
	}

	return s.computeTokenBreakdown(pricing, tokens, rateMultiplier, serviceTier, longContextBillingEnabled), nil
}

// longContextMultiplierOrOne 把未配置（≤0）的长上下文倍率归一为 1。
func longContextMultiplierOrOne(m float64) float64 {
	if m <= 0 {
		return 1
	}
	return m
}

func (s *BillingService) shouldApplySessionLongContextPricing(tokens UsageTokens, pricing *ModelPricing) bool {
	if pricing == nil || pricing.LongContextInputThreshold <= 0 {
		return false
	}
	if pricing.LongContextInputMultiplier <= 1 && pricing.LongContextOutputMultiplier <= 1 {
		return false
	}
	totalInputTokens := tokens.InputTokens + tokens.CacheCreationTokens + tokens.CacheReadTokens
	if pricing.LongContextThresholdInclusive {
		return totalInputTokens >= pricing.LongContextInputThreshold
	}
	return totalInputTokens > pricing.LongContextInputThreshold
}

// CalculateCostWithConfig 使用配置中的默认倍率计算费用
func (s *BillingService) CalculateCostWithConfig(model string, tokens UsageTokens) (*CostBreakdown, error) {
	multiplier := s.cfg.Default.RateMultiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	return s.CalculateCost(model, tokens, multiplier)
}

// ListSupportedModels 列出所有支持的模型（现在总是返回true，因为有模糊匹配）
func (s *BillingService) ListSupportedModels() []string {
	models := make([]string, 0)
	// 返回回退价格支持的模型系列
	for model := range s.fallbackPrices {
		models = append(models, model)
	}
	return models
}

// IsModelSupported 检查模型是否支持（现在总是返回true，因为有模糊匹配回退）
func (s *BillingService) IsModelSupported(model string) bool {
	// 所有Claude模型都有回退价格支持
	modelLower := strings.ToLower(model)
	return strings.Contains(modelLower, "claude") ||
		strings.Contains(modelLower, "opus") ||
		strings.Contains(modelLower, "sonnet") ||
		strings.Contains(modelLower, "haiku")
}

// GetEstimatedCost 估算费用（用于前端展示）
func (s *BillingService) GetEstimatedCost(model string, estimatedInputTokens, estimatedOutputTokens int) (float64, error) {
	tokens := UsageTokens{
		InputTokens:  estimatedInputTokens,
		OutputTokens: estimatedOutputTokens,
	}

	breakdown, err := s.CalculateCostWithConfig(model, tokens)
	if err != nil {
		return 0, err
	}

	return breakdown.ActualCost, nil
}

// GetPricingServiceStatus 获取价格服务状态
func (s *BillingService) GetPricingServiceStatus() map[string]any {
	if s.pricingService != nil {
		return s.pricingService.GetStatus()
	}
	return map[string]any{
		"model_count":  len(s.fallbackPrices),
		"last_updated": "using fallback",
		"local_hash":   "N/A",
	}
}

// ForceUpdatePricing 强制更新价格数据
func (s *BillingService) ForceUpdatePricing() error {
	if s.pricingService != nil {
		return s.pricingService.ForceUpdate()
	}
	return fmt.Errorf("pricing service not initialized")
}

// ImagePriceConfig 图片计费配置
type ImagePriceConfig struct {
	Price1K *float64 // 1K 尺寸价格（nil 表示使用默认值）
	Price2K *float64 // 2K 尺寸价格（nil 表示使用默认值）
	Price4K *float64 // 4K 尺寸价格（nil 表示使用默认值）
}

// VideoPriceConfig 视频生成计费配置。所有价格均为**每秒**单价（USD/s），与 xAI 官方计费口径一致。
type VideoPriceConfig struct {
	Price480P  *float64 // 480p 每秒价格（nil 表示使用默认值）
	Price720P  *float64 // 720p 每秒价格（nil 表示使用默认值）
	Price1080P *float64 // 1080p 每秒价格（nil 表示使用默认值）
	// ModelPrices is optional per-model-family override: family → resolution → USD/s.
	// When set for a model, it wins over Price* flat columns for that model only.
	ModelPrices map[string]map[string]float64
}

const (
	defaultImageGenerationPrice = 0.134

	defaultGrokImagineImagePrice1K        = 0.02
	defaultGrokImagineImagePrice2K        = 0.02
	defaultGrokImagineImageQualityPrice1K = 0.05
	defaultGrokImagineImageQualityPrice2K = 0.07
	defaultGrokImagineImage20Price1K      = 0.06 // default quality is Medium
	defaultGrokImagineImage20Price2K      = 0.08

	// 视频默认价为 xAI 官方**每秒**输出价格（USD/s），总价 = 每秒价 × 时长（秒）。
	defaultGrokImagineVideoPrice480P    = 0.05
	defaultGrokImagineVideoPrice720P    = 0.07
	defaultGrokImagineVideo15Price480P  = 0.08
	defaultGrokImagineVideo15Price720P  = 0.14
	defaultGrokImagineVideo15Price1080P = 0.25

	// Codex alpha/search 网页搜索单次默认价：OpenAI 官方 web search 定价 $10/1000 次。
	defaultWebSearchPricePerCall = 0.01

	// xAI server-side web/X search and code execution are $5/1000 calls.
	defaultSearchPricePer1k = 5.0

	// Generic realtime defaults to think-fast-1.0; think-fast-2.0 can be
	// configured independently through per-model group/channel pricing.
	defaultAudioRealtimePricePerMin     = 0.05
	defaultAudioTTSPricePerMillionChars = 15.0
	defaultAudioSTTPricePerHour         = 0.10
)

// CalculateWebSearchCost 计算 Codex alpha/search 网页搜索按次费用。
// callCount: 搜索调用次数（每次请求为 1）
// groupPrice: 分组配置的单次价格（nil 表示使用默认价 0.01；0 表示免费）
// rateMultiplier: 分组费率倍数
func (s *BillingService) CalculateWebSearchCost(callCount int, groupPrice *float64, rateMultiplier float64) *CostBreakdown {
	if callCount <= 0 {
		return &CostBreakdown{}
	}
	unitPrice := defaultWebSearchPricePerCall
	if groupPrice != nil && *groupPrice >= 0 {
		unitPrice = *groupPrice
	}
	totalCost := unitPrice * float64(callCount)

	return newPerUnitCost(totalCost, rateMultiplier, BillingModePerRequest)
}

// newPerUnitCost 构造"按次/按量"计费结果（搜索、音频、图片、视频……）：单价×数量得到
// TotalCost，再乘入基础倍率。
//
// 关键是把 RateMultiplier 记下来，而不是只留下相乘后的金额：落账不变式
// ActualCost = TotalCost × RateMultiplier × ModelRateMultiplier 是对 usage_logs 的
// 每一行声明的，施加了倍率却不记账，这一行事后就无法自证，对账只能靠猜。
// 这些函数都走不到 CalculateCostUnified，逐模型因子由调用链末端的
// finalizeRecordedCost 统一补上（ModelRateMultiplier 留 0 表示"尚未施加"）。
func newPerUnitCost(totalCost, rateMultiplier float64, mode BillingMode) *CostBreakdown {
	// 保存时强制 > 0；若仍有负数泄漏（缓存/迁移残留），按 0 处理避免按 1x 误扣。
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	return &CostBreakdown{
		TotalCost:      totalCost,
		ActualCost:     totalCost * rateMultiplier,
		RateMultiplier: rateMultiplier,
		BillingMode:    string(mode),
	}
}

// finalizeRecordedCost 是"要落进 usage_logs 的那个 CostBreakdown"的唯一收口。
//
// 两条网关的 calculateRecordUsageCost 有十来条互斥分支（token / 按次 / 图片 / 视频 /
// 音频 / 搜索 / 各种降级兜底），只有走 CalculateCostUnified 的那几条会施加并记录
// 逐模型因子。收口放在包装函数里而不是每个 return 前面：新增一条分支不可能绕过它。
// ModelRateMultiplier == 0 就是"这条分支没经过统一入口"的标记——校验保证真实因子
// 恒在 (0,100]，取不到 0。
func finalizeRecordedCost(cost *CostBreakdown, group *Group, model string) *CostBreakdown {
	if cost == nil {
		return nil
	}
	if cost.ModelRateMultiplier != 0 {
		return cost
	}
	factor := resolveGroupModelRateMultiplier(group, model)
	cost.ModelRateMultiplier = factor
	if factor != 1 {
		cost.ActualCost *= factor
	}
	return cost
}

// CalculateSearchCost bills search/tool invocations (e.g. web_search) per 1k calls.
// groupPricePer1k: nil → defaultSearchPricePer1k; explicit 0 → free; >0 → that rate.
func (s *BillingService) CalculateSearchCost(numCalls int, groupPricePer1k *float64, rateMultiplier float64) *CostBreakdown {
	if numCalls <= 0 {
		return &CostBreakdown{}
	}
	pricePer1k := defaultSearchPricePer1k
	if groupPricePer1k != nil {
		if *groupPricePer1k < 0 {
			return &CostBreakdown{}
		}
		pricePer1k = *groupPricePer1k
	}
	if pricePer1k == 0 {
		return &CostBreakdown{}
	}
	unit := pricePer1k / 1000.0
	return newPerUnitCost(unit*float64(numCalls), rateMultiplier, BillingModePerRequest)
}

type audioPriceConfig struct {
	RealtimePerMin *float64
	TTSPerMChars   *float64
	STTPerHour     *float64
}

// CalculateAudioCost supports realtime (per min), tts (per M chars), stt (per hr).
// Missing group prices use defaults; explicit 0 means free for that mode.
func (s *BillingService) CalculateAudioCost(mode string, durationOrUnits float64, groupConfig *audioPriceConfig, rateMultiplier float64) *CostBreakdown {
	if durationOrUnits <= 0 {
		return &CostBreakdown{}
	}
	var unitPrice float64
	switch strings.ToLower(mode) {
	case "realtime":
		unitPrice = defaultAudioRealtimePricePerMin
		if groupConfig != nil && groupConfig.RealtimePerMin != nil {
			unitPrice = *groupConfig.RealtimePerMin
		}
	case "tts":
		unitPrice = defaultAudioTTSPricePerMillionChars
		if groupConfig != nil && groupConfig.TTSPerMChars != nil {
			unitPrice = *groupConfig.TTSPerMChars
		}
	case "stt":
		unitPrice = defaultAudioSTTPricePerHour
		if groupConfig != nil && groupConfig.STTPerHour != nil {
			unitPrice = *groupConfig.STTPerHour
		}
	default:
		return &CostBreakdown{}
	}
	if unitPrice <= 0 {
		return &CostBreakdown{}
	}
	return newPerUnitCost(unitPrice*durationOrUnits, rateMultiplier, BillingModePerRequest)
}

// CalculateImageCost 计算图片生成费用
// model: 请求的模型名称（用于获取 LiteLLM 默认价格）
// imageSize: 图片尺寸 "1K", "2K", "4K"
// imageCount: 生成的图片数量
// groupConfig: 分组配置的价格（可能为 nil，表示使用默认值）
// rateMultiplier: 费率倍数
func (s *BillingService) CalculateImageCost(model string, imageSize string, imageCount int, groupConfig *ImagePriceConfig, rateMultiplier float64) *CostBreakdown {
	if imageCount <= 0 {
		return &CostBreakdown{}
	}
	imageSize = NormalizeImageBillingTierOrDefault(imageSize)

	// 获取单价
	unitPrice := s.getImageUnitPrice(model, imageSize, groupConfig)

	// 计算总费用
	totalCost := unitPrice * float64(imageCount)

	return newPerUnitCost(totalCost, rateMultiplier, BillingModeImage)
}

// CalculateVideoCost 计算视频生成费用（按秒计费，与 xAI 口径一致）。
// model: 请求的模型名称（用于获取默认价格）
// resolution: 视频分辨率 "480p", "720p", "1080p"
// videoCount: 生成的视频数量
// durationSeconds: 单个视频时长（秒），<=0 时按上游默认时长计
// groupConfig: 分组配置的每秒价格（可能为 nil，表示使用默认值）
// rateMultiplier: 费率倍数
func (s *BillingService) CalculateVideoCost(model string, resolution string, videoCount int, durationSeconds int, groupConfig *VideoPriceConfig, rateMultiplier float64) *CostBreakdown {
	if videoCount <= 0 {
		return &CostBreakdown{}
	}
	resolution = NormalizeVideoBillingResolutionOrDefault(resolution)
	durationSeconds = NormalizeVideoBillingDurationSecondsOrDefault(durationSeconds)

	perSecondPrice := s.getVideoUnitPrice(model, resolution, groupConfig)
	totalCost := perSecondPrice * float64(durationSeconds) * float64(videoCount)

	return newPerUnitCost(totalCost, rateMultiplier, BillingModeVideo)
}

// getImageUnitPrice 获取图片单价
func (s *BillingService) getImageUnitPrice(model string, imageSize string, groupConfig *ImagePriceConfig) float64 {
	// 优先使用分组配置的价格
	if groupConfig != nil {
		switch imageSize {
		case "1K":
			if groupConfig.Price1K != nil {
				return *groupConfig.Price1K
			}
		case "2K":
			if groupConfig.Price2K != nil {
				return *groupConfig.Price2K
			}
		case "4K":
			if groupConfig.Price4K != nil {
				return *groupConfig.Price4K
			}
		}
	}

	// 回退到 LiteLLM 默认价格
	return s.getDefaultImagePrice(model, imageSize)
}

func (s *BillingService) getVideoUnitPrice(model string, resolution string, groupConfig *VideoPriceConfig) float64 {
	// Order: (a) per-model map (b) flat group video_price_* (c) model-aware code defaults.
	if groupConfig != nil {
		if price := LookupVideoModelPrice(groupConfig.ModelPrices, model, resolution); price != nil {
			return *price
		}
		switch NormalizeVideoBillingResolutionOrDefault(resolution) {
		case VideoBillingResolution480P:
			if groupConfig.Price480P != nil {
				return *groupConfig.Price480P
			}
		case VideoBillingResolution720P:
			if groupConfig.Price720P != nil {
				return *groupConfig.Price720P
			}
		case VideoBillingResolution1080P:
			if groupConfig.Price1080P != nil {
				return *groupConfig.Price1080P
			}
		}
	}

	return s.getDefaultVideoPrice(model, resolution)
}

// getDefaultImagePrice 获取 LiteLLM 默认图片价格
func (s *BillingService) getDefaultImagePrice(model string, imageSize string) float64 {
	if price, ok := getDefaultGrokImagineImagePrice(model, imageSize); ok {
		return price
	}

	basePrice := 0.0

	// 从 PricingService 获取 output_cost_per_image
	if s.pricingService != nil {
		pricing := s.pricingService.GetModelPricing(model)
		if pricing != nil && pricing.OutputCostPerImage > 0 {
			basePrice = pricing.OutputCostPerImage
		}
	}

	// 如果没有找到价格，使用硬编码默认值（$0.134，来自 gemini-3-pro-image-preview）
	if basePrice <= 0 {
		basePrice = defaultImageGenerationPrice
	}

	// 2K 尺寸 1.5 倍，4K 尺寸翻倍
	if imageSize == "2K" {
		return basePrice * 1.5
	}
	if imageSize == "4K" {
		return basePrice * 2
	}

	return basePrice
}

func (s *BillingService) getDefaultVideoPrice(model string, resolution string) float64 {
	if price, ok := getDefaultGrokImagineVideoPrice(model, resolution); ok {
		return price
	}

	// The bundled LiteLLM schema does not expose an output video generation price.
	// Keep the historical model default as the fallback (interpreted as a per-second
	// rate; today only Grok models reach video billing, so this path is a safety net),
	// while letting group-level video prices override it independently from image prices.
	return s.getDefaultImagePrice(model, ImageBillingSize2K)
}

func getDefaultGrokImagineImagePrice(model string, imageSize string) (float64, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch model {
	case "grok-imagine-image-2.0":
		return getGrokImagineImageTierPrice(
			imageSize,
			defaultGrokImagineImage20Price1K,
			defaultGrokImagineImage20Price2K,
		), true
	case "grok-imagine-image-quality":
		return getGrokImagineImageTierPrice(
			imageSize,
			defaultGrokImagineImageQualityPrice1K,
			defaultGrokImagineImageQualityPrice2K,
		), true
	case "grok-imagine", "grok-imagine-image", "grok-imagine-edit":
		return getGrokImagineImageTierPrice(
			imageSize,
			defaultGrokImagineImagePrice1K,
			defaultGrokImagineImagePrice2K,
		), true
	default:
		return 0, false
	}
}

func getGrokImagineImageTierPrice(imageSize string, price1K float64, price2K float64) float64 {
	switch NormalizeImageBillingTierOrDefault(imageSize) {
	case ImageBillingSize1K:
		return price1K
	case ImageBillingSize2K, ImageBillingSize4K:
		return price2K
	default:
		return price2K
	}
}

func getDefaultGrokImagineVideoPrice(model string, resolution string) (float64, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(model, "grok-imagine-video-1.5"):
		switch NormalizeVideoBillingResolutionOrDefault(resolution) {
		case VideoBillingResolution480P:
			return defaultGrokImagineVideo15Price480P, true
		case VideoBillingResolution720P:
			return defaultGrokImagineVideo15Price720P, true
		case VideoBillingResolution1080P:
			return defaultGrokImagineVideo15Price1080P, true
		default:
			return defaultGrokImagineVideo15Price480P, true
		}
	case strings.HasPrefix(model, "grok-imagine-video"):
		switch NormalizeVideoBillingResolutionOrDefault(resolution) {
		case VideoBillingResolution480P:
			return defaultGrokImagineVideoPrice480P, true
		case VideoBillingResolution720P, VideoBillingResolution1080P:
			return defaultGrokImagineVideoPrice720P, true
		default:
			return defaultGrokImagineVideoPrice480P, true
		}
	default:
		return 0, false
	}
}
