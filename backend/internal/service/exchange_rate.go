package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
	"golang.org/x/sync/singleflight"
)

// 汇率与币种折算。
//
// 站内只有一个记账币种（设置 balance_currency，默认 USD）：余额、扣费、限额都以它计。凡是以其它币种支付或
// 计价的金额——管理员按人民币 / USDT / USDC 调整余额、在线支付、以人民币标价的模型（方舟 Seedance）——都在
// 交易发生时按当天汇率折算成记账币种，所用汇率连同来源一起记在该笔记录上（CurrencyConversion），可复现。
//
// 汇率口径（统一换算成「1 单位该币值多少美元」，两种非美元币种之间经美元交叉）：
//   - CNY：中国外汇交易中心公布的人民币对美元中间价（工作日北京时间 9:15 公布），取交易时刻之前最近一次
//     公布的一条；节假日不公布，沿用上一次公布的中间价。
//   - USDT / USDC：CoinGecko 当日（UTC 日期）00:00 的美元价格。
//
// 充值（FXStrict）要求汇率确定是当前有效的：人民币中间价刷新失败、稳定币当日价取不到，都拒绝入账。
// 计费（FXBilling）不能因为汇率源暂时不可用就丢单：沿用最近一次公布的中间价，并在折算记录上标 stale、记日志。

// FX 汇率来源标识（存进 exchange_rates.source 与折算记录）。
const (
	FXSourceCFETSCentralParity = "cfets_central_parity"
	FXSourceCoinGeckoDaily     = "coingecko_daily_price"
)

// fxCurrency 是一个可折算币种的声明：取价来源，以及能否作为记账 / 标价币种（只有 ISO 法币可以）。
type fxCurrency struct {
	source      string // "" 表示美元本身
	coinGeckoID string
	fiat        bool
}

// fxCurrencies 是可折算币种的唯一声明：校验、取价、前端可选项都从这里出。
var fxCurrencies = map[string]fxCurrency{
	"USD":  {fiat: true},
	"CNY":  {source: FXSourceCFETSCentralParity, fiat: true},
	"USDT": {source: FXSourceCoinGeckoDaily, coinGeckoID: "tether"},
	"USDC": {source: FXSourceCoinGeckoDaily, coinGeckoID: "usd-coin"},
}

// SupportedFXCurrencies 返回可折算币种（有序），fiatOnly 时只含能作为记账 / 标价币种的法币。
func SupportedFXCurrencies(fiatOnly bool) []string {
	codes := make([]string, 0, len(fxCurrencies))
	for code, c := range fxCurrencies {
		if fiatOnly && !c.fiat {
			continue
		}
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// NormalizeFXCurrency 校验并归一化一个可折算币种代码（去空白、转大写）。
func NormalizeFXCurrency(code string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if _, ok := fxCurrencies[normalized]; !ok {
		return "", infraerrors.BadRequest("UNSUPPORTED_CURRENCY",
			fmt.Sprintf("currency must be one of %s, got %q", strings.Join(SupportedFXCurrencies(false), ", "), code))
	}
	return normalized, nil
}

// NormalizeFiatCurrency 校验并归一化记账 / 标价币种（只认可折算的 ISO 法币）。
func NormalizeFiatCurrency(code string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if c, ok := fxCurrencies[normalized]; !ok || !c.fiat {
		return "", infraerrors.BadRequest("UNSUPPORTED_CURRENCY",
			fmt.Sprintf("currency must be one of %s, got %q", strings.Join(SupportedFXCurrencies(true), ", "), code))
	}
	return normalized, nil
}

// ExchangeRate 是一条存档的汇率：1 单位 Currency 值 USDPerUnit 美元。
type ExchangeRate struct {
	Currency    string    `json:"currency"`
	RateDate    string    `json:"rate_date"`    // YYYY-MM-DD：中间价的公布日 / CoinGecko 的 UTC 日期
	USDPerUnit  float64   `json:"usd_per_unit"` // 1 单位该币值多少美元
	Quote       float64   `json:"quote"`        // 来源原样的报价
	QuoteUnit   string    `json:"quote_unit"`   // 报价单位，如 "CNY per USD"
	Source      string    `json:"source"`
	SourceURL   string    `json:"source_url"`
	PublishedAt time.Time `json:"published_at"` // 生效时刻
	FetchedAt   time.Time `json:"fetched_at"`
}

// CurrencyConversion 记录一笔金额从原币种折算到目标币种的依据。
type CurrencyConversion struct {
	FromCurrency string         `json:"from_currency"`
	ToCurrency   string         `json:"to_currency"`
	FromAmount   *float64       `json:"from_amount,omitempty"` // 充值等有明确原币金额的场景
	ToAmount     *float64       `json:"to_amount,omitempty"`
	Rate         float64        `json:"rate"` // 1 单位原币 = Rate 单位目标币
	At           time.Time      `json:"at"`   // 交易时刻
	Legs         []ExchangeRate `json:"legs"` // 参与折算的非美元报价
	// Stale 为 true 表示计费时汇率源刷新失败，沿用了最近一次存档的中间价（StaleReason 写明原因）。
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`
}

// EncodeCurrencyConversion 是 currency_conversion（JSONB）列的唯一编码（兑换码、支付订单、用量日志、批量图片任务
// 共用）；nil 返回 nil（写 NULL）。
func EncodeCurrencyConversion(conversion *CurrencyConversion) ([]byte, error) {
	if conversion == nil {
		return nil, nil
	}
	raw, err := json.Marshal(conversion)
	if err != nil {
		return nil, fmt.Errorf("marshal currency_conversion: %w", err)
	}
	return raw, nil
}

// DecodeCurrencyConversion 是 currency_conversion 列的唯一解码；NULL 返回 nil。解析失败也返回 nil 并告警（row 写明
// 是哪一行）：折算依据是审计信息，金额本身已按记账币种落库，读不出依据不能挡住这行记录的其它用途。
func DecodeCurrencyConversion(raw []byte, row string) *CurrencyConversion {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var conversion CurrencyConversion
	if err := json.Unmarshal(raw, &conversion); err != nil {
		slog.Error("currency_conversion is unreadable", "row", row, "error", err)
		return nil
	}
	return &conversion
}

// FXMode 决定汇率源不可用时的处理。
type FXMode int

const (
	// FXStrict：充值等资金入账。汇率必须确认是当前有效的，否则报错（拒绝入账）。
	FXStrict FXMode = iota
	// FXBilling：用量计费。沿用最近一次存档的中间价并标 stale，不因汇率源暂时不可用而丢单。
	FXBilling
)

// ErrExchangeRateUnavailable 表示取不到所需汇率（拒绝入账；计费时按无价处理）。errors.Is 按错误码匹配，
// 具体原因见各处 fxUnavailable 的消息。
var ErrExchangeRateUnavailable = infraerrors.ServiceUnavailable("EXCHANGE_RATE_UNAVAILABLE", "exchange rate unavailable")

func fxUnavailable(format string, args ...any) error {
	return infraerrors.ServiceUnavailable("EXCHANGE_RATE_UNAVAILABLE", fmt.Sprintf(format, args...))
}

// ExchangeRateRepository 是汇率存档。
type ExchangeRateRepository interface {
	UpsertExchangeRates(ctx context.Context, rates []ExchangeRate) error
	// LatestExchangeRatePublishedBy 返回 published_at <= at 的最新一条；没有时返回 (nil, nil)。
	LatestExchangeRatePublishedBy(ctx context.Context, currency string, at time.Time) (*ExchangeRate, error)
	// GetExchangeRate 返回指定日期的一条；没有时返回 (nil, nil)。
	GetExchangeRate(ctx context.Context, currency, rateDate string) (*ExchangeRate, error)
	ListExchangeRates(ctx context.Context, currency string, limit int) ([]ExchangeRate, error)
}

// ExchangeRateFetcher 是汇率源（外部接口）。
type ExchangeRateFetcher interface {
	// FetchCNYCentralParity 返回 [from, to] 日期区间内公布的人民币对美元中间价。
	FetchCNYCentralParity(ctx context.Context, from, to time.Time) ([]ExchangeRate, error)
	// FetchCoinGeckoDailyPrice 返回 coinID 在 date（UTC 日期）00:00 的美元价格。
	FetchCoinGeckoDailyPrice(ctx context.Context, currency, coinID string, date time.Time) (*ExchangeRate, error)
}

// AccountingCurrencyReader 给出站内记账币种。
type AccountingCurrencyReader interface {
	BalanceCurrency(ctx context.Context) BalanceCurrency
}

// CNY 中间价的公布时刻：工作日北京时间 9:15（节假日不公布）。汇率源与刷新节奏都以这里为准。
const (
	cnyPublishHour   = 9
	cnyPublishMinute = 15
)

// CNYCentralParityZone 是中间价公布所在的时区（北京时间）。
var CNYCentralParityZone = time.FixedZone("Asia/Shanghai", 8*3600)

// CNYCentralParityPublishedAt 返回 rateDate（北京日期）那一天中间价的生效时刻：当天 9:15。
func CNYCentralParityPublishedAt(rateDate time.Time) time.Time {
	day := rateDate.In(CNYCentralParityZone)
	return time.Date(day.Year(), day.Month(), day.Day(), cnyPublishHour, cnyPublishMinute, 0, 0, CNYCentralParityZone).UTC()
}

const (
	// cnyRefreshInterval：平时每小时最多向外汇交易中心刷新一次中间价。
	cnyRefreshInterval = time.Hour
	// cnyPublishRetryInterval / cnyPublishRetryWindow：工作日 9:15 公布之后的两小时里，存档还没有当天的中间价时
	// 每 5 分钟重取一次——交易当天的中间价一公布就要用上，不能等满一小时。
	cnyPublishRetryInterval = 5 * time.Minute
	cnyPublishRetryWindow   = 2 * time.Hour
	// cnyRefreshWindow：每次刷新抓最近 30 天，覆盖长假。
	cnyRefreshWindow = 30 * 24 * time.Hour
	// realtimeFXWindow：交易时刻在这个窗口内视为实时交易，需要确认中间价是最新的；更早的按历史时刻查存档。
	realtimeFXWindow = 48 * time.Hour
	// cnyHistoryGap：历史时刻之前 15 天内都没有存档的中间价时，补抓那一段。
	cnyHistoryGap = 15 * 24 * time.Hour
)

// ExchangeRateService 取汇率、做折算。没有后台任务：用到时按需刷新并存档。
type ExchangeRateService struct {
	repo       ExchangeRateRepository
	fetcher    ExchangeRateFetcher
	accounting AccountingCurrencyReader
	now        func() time.Time

	mu             sync.Mutex
	lastCNYRefresh time.Time
	refreshGroup   singleflight.Group
}

// NewExchangeRateService 创建汇率服务。
func NewExchangeRateService(repo ExchangeRateRepository, fetcher ExchangeRateFetcher, accounting AccountingCurrencyReader) *ExchangeRateService {
	return &ExchangeRateService{repo: repo, fetcher: fetcher, accounting: accounting, now: time.Now}
}

// AccountingCurrency 返回站内记账币种（未装配设置时为默认 USD）。
func (s *ExchangeRateService) AccountingCurrency(ctx context.Context) string {
	if s == nil || s.accounting == nil {
		return DefaultBalanceCurrency
	}
	return s.accounting.BalanceCurrency(ctx).Code
}

// ConvertToAccounting 把 amount（from 币种）折算成记账币种。同币种返回原值与 nil 折算记录。
func (s *ExchangeRateService) ConvertToAccounting(ctx context.Context, amount float64, from string, at time.Time, mode FXMode) (float64, *CurrencyConversion, error) {
	return s.Convert(ctx, amount, from, s.AccountingCurrency(ctx), at, mode)
}

// Convert 把 amount 从 from 币种折算成 to 币种（at 为交易时刻）。同币种返回原值与 nil 折算记录。
func (s *ExchangeRateService) Convert(ctx context.Context, amount float64, from, to string, at time.Time, mode FXMode) (float64, *CurrencyConversion, error) {
	// 同币种不需要汇率：先比原始代码，不在折算表里的币种（如只走某个网关的 HKD）同币种时也照常通过。
	if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(to)) {
		return amount, nil, nil
	}
	from, err := NormalizeFXCurrency(from)
	if err != nil {
		return 0, nil, err
	}
	to, err = NormalizeFXCurrency(to)
	if err != nil {
		return 0, nil, err
	}
	if s == nil {
		return 0, nil, fxUnavailable("converting %s to %s needs the exchange rate service, which is not configured", from, to)
	}
	if at.IsZero() {
		at = s.now()
	}
	conversion := &CurrencyConversion{FromCurrency: from, ToCurrency: to, At: at.UTC()}
	fromRate, err := s.usdPerUnit(ctx, from, at, mode, conversion)
	if err != nil {
		return 0, nil, err
	}
	toRate, err := s.usdPerUnit(ctx, to, at, mode, conversion)
	if err != nil {
		return 0, nil, err
	}
	conversion.Rate = fromRate / toRate
	converted := amount * conversion.Rate
	return converted, conversion, nil
}

// usdPerUnit 返回 at 时刻 1 单位 currency 值多少美元，把用到的报价追加进 conversion.Legs。
func (s *ExchangeRateService) usdPerUnit(ctx context.Context, currency string, at time.Time, mode FXMode, conversion *CurrencyConversion) (float64, error) {
	c := fxCurrencies[currency]
	var (
		rate *ExchangeRate
		err  error
	)
	switch c.source {
	case "":
		return 1, nil
	case FXSourceCFETSCentralParity:
		rate, err = s.cnyRateAt(ctx, at, mode, conversion)
	case FXSourceCoinGeckoDaily:
		rate, err = s.coinRateOn(ctx, currency, c.coinGeckoID, at)
	default:
		err = fmt.Errorf("no exchange rate source for %s", currency)
	}
	if err != nil {
		return 0, err
	}
	conversion.Legs = append(conversion.Legs, *rate)
	return rate.USDPerUnit, nil
}

// cnyRateAt 返回 at 时刻有效的人民币中间价（published_at <= at 的最新一条）。
func (s *ExchangeRateService) cnyRateAt(ctx context.Context, at time.Time, mode FXMode, conversion *CurrencyConversion) (*ExchangeRate, error) {
	realtime := s.now().Sub(at) < realtimeFXWindow
	var refreshErr error
	if realtime {
		refreshErr = s.refreshCNYIfDue(ctx)
	}
	rate, err := s.repo.LatestExchangeRatePublishedBy(ctx, "CNY", at)
	if err != nil {
		return nil, fmt.Errorf("read CNY central parity: %w", err)
	}
	if rate == nil || (!realtime && at.Sub(rate.PublishedAt) > cnyHistoryGap) {
		// 历史时刻附近没有存档（或从未抓过）：补抓那一段再查。
		if backfillErr := s.fetchAndStoreCNY(ctx, at.Add(-cnyRefreshWindow), at); backfillErr != nil && refreshErr == nil {
			refreshErr = backfillErr
		}
		if rate, err = s.repo.LatestExchangeRatePublishedBy(ctx, "CNY", at); err != nil {
			return nil, fmt.Errorf("read CNY central parity: %w", err)
		}
	}
	if rate == nil {
		return nil, fxUnavailable("no CNY central parity published before %s (%v)", at.UTC().Format(time.RFC3339), refreshErr)
	}
	if refreshErr != nil {
		if mode == FXStrict {
			return nil, fxUnavailable("cannot confirm the current CNY central parity (latest archived %s): %v", rate.RateDate, refreshErr)
		}
		conversion.Stale = true
		conversion.StaleReason = fmt.Sprintf("CNY central parity refresh failed, used the latest archived fixing %s: %v", rate.RateDate, refreshErr)
		slog.Warn("fx.cny_central_parity_stale", "rate_date", rate.RateDate, "at", at.UTC(), "error", refreshErr)
	}
	return rate, nil
}

// cnyRefreshDue 判断现在要不要向外汇交易中心重取中间价：
//   - 距上次成功刷新满一小时；
//   - 今天（工作日）9:15 已过而上次刷新在它之前：当天的中间价刚公布，马上取；
//   - 9:15 之后两小时内、距上次刷新满 5 分钟、存档里还没有当天的中间价（公布稍有延迟，或节假日不公布）。
func (s *ExchangeRateService) cnyRefreshDue(ctx context.Context, now time.Time) bool {
	s.mu.Lock()
	last := s.lastCNYRefresh
	s.mu.Unlock()
	if now.Sub(last) >= cnyRefreshInterval {
		return true
	}
	today := now.In(CNYCentralParityZone)
	if wd := today.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	published := CNYCentralParityPublishedAt(today)
	if now.Before(published) {
		return false
	}
	if last.Before(published) {
		return true
	}
	if now.Sub(published) >= cnyPublishRetryWindow || now.Sub(last) < cnyPublishRetryInterval {
		return false
	}
	archived, err := s.repo.GetExchangeRate(ctx, "CNY", today.Format(time.DateOnly))
	return err != nil || archived == nil
}

// refreshCNYIfDue 在到点时（cnyRefreshDue）向外汇交易中心抓最近 30 天的中间价并存档；并发调用合并成一次。
func (s *ExchangeRateService) refreshCNYIfDue(ctx context.Context) error {
	if !s.cnyRefreshDue(ctx, s.now()) {
		return nil
	}
	_, err, _ := s.refreshGroup.Do("cny", func() (any, error) {
		now := s.now()
		if err := s.fetchAndStoreCNY(ctx, now.Add(-cnyRefreshWindow), now); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.lastCNYRefresh = now
		s.mu.Unlock()
		return nil, nil
	})
	return err
}

func (s *ExchangeRateService) fetchAndStoreCNY(ctx context.Context, from, to time.Time) error {
	if s.fetcher == nil {
		return errors.New("no exchange rate fetcher configured")
	}
	rates, err := s.fetcher.FetchCNYCentralParity(ctx, from, to)
	if err != nil {
		return fmt.Errorf("fetch CNY central parity: %w", err)
	}
	if len(rates) == 0 {
		return nil
	}
	if err := s.repo.UpsertExchangeRates(ctx, rates); err != nil {
		return fmt.Errorf("archive CNY central parity: %w", err)
	}
	return nil
}

// coinRateOn 返回稳定币在 at 所在 UTC 日 00:00 的美元价格；存档里没有就向 CoinGecko 取，取不到报错。
func (s *ExchangeRateService) coinRateOn(ctx context.Context, currency, coinID string, at time.Time) (*ExchangeRate, error) {
	date := at.UTC().Format(time.DateOnly)
	rate, err := s.repo.GetExchangeRate(ctx, currency, date)
	if err != nil {
		return nil, fmt.Errorf("read %s rate: %w", currency, err)
	}
	if rate != nil {
		return rate, nil
	}
	if s.fetcher == nil {
		return nil, fxUnavailable("no exchange rate fetcher configured")
	}
	day, _ := time.Parse(time.DateOnly, date)
	rate, err = s.fetcher.FetchCoinGeckoDailyPrice(ctx, currency, coinID, day)
	if err != nil {
		return nil, fxUnavailable("%s price on %s: %v", currency, date, err)
	}
	if rate == nil || rate.USDPerUnit <= 0 || math.IsNaN(rate.USDPerUnit) || math.IsInf(rate.USDPerUnit, 0) {
		return nil, fxUnavailable("%s price on %s is missing", currency, date)
	}
	if err := s.repo.UpsertExchangeRates(ctx, []ExchangeRate{*rate}); err != nil {
		return nil, fmt.Errorf("archive %s rate: %w", currency, err)
	}
	return rate, nil
}

// ListRates 返回某个币种最近的存档汇率（管理端查看用）。
func (s *ExchangeRateService) ListRates(ctx context.Context, currency string, limit int) ([]ExchangeRate, error) {
	currency, err := NormalizeFXCurrency(currency)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 365 {
		limit = 30
	}
	return s.repo.ListExchangeRates(ctx, currency, limit)
}

// PricingInAccounting 返回定价条目折算成记账币种后的克隆（今天的汇率、计费口径；价格字段逐项换算，倍率不动），
// 供展示（模型广场、可用渠道）使用；本来就是记账币种时原样返回。折算不了时去掉全部价格并记日志——
// 宁可不展示，也不把外币数字标成记账币种。s 为 nil（未装配汇率服务）时记账币种按 USD、非 USD 价格不展示。
func (s *ExchangeRateService) PricingInAccounting(ctx context.Context, p *ChannelModelPricing) *ChannelModelPricing {
	if p == nil {
		return nil
	}
	accounting := s.AccountingCurrency(ctx)
	currency := pricingCurrency(p)
	if currency == accounting {
		return p
	}
	clone := p.Clone()
	clone.Currency = accounting
	var rate float64
	if _, conversion, err := s.Convert(ctx, 1, currency, accounting, time.Now(), FXBilling); err != nil {
		slog.Warn("fx.pricing_not_converted", "models", p.Models, "currency", currency, "accounting", accounting, "error", err)
	} else {
		rate = conversion.Rate
	}
	scale := func(v *float64) *float64 {
		if v == nil || rate == 0 {
			return nil
		}
		scaled := *v * rate
		return &scaled
	}
	clone.InputPrice, clone.OutputPrice = scale(p.InputPrice), scale(p.OutputPrice)
	clone.CacheWritePrice, clone.CacheWrite1hPrice, clone.CacheReadPrice = scale(p.CacheWritePrice), scale(p.CacheWrite1hPrice), scale(p.CacheReadPrice)
	clone.ImageInputPrice, clone.ImageOutputPrice, clone.PerRequestPrice = scale(p.ImageInputPrice), scale(p.ImageOutputPrice), scale(p.PerRequestPrice)
	for i := range clone.Intervals {
		iv := &clone.Intervals[i]
		iv.InputPrice, iv.OutputPrice = scale(iv.InputPrice), scale(iv.OutputPrice)
		iv.CacheWritePrice, iv.CacheWrite1hPrice, iv.CacheReadPrice = scale(iv.CacheWritePrice), scale(iv.CacheWrite1hPrice), scale(iv.CacheReadPrice)
		iv.PerRequestPrice = scale(iv.PerRequestPrice)
	}
	return &clone
}

// creditedAmountPlaces：折算后入账到余额的金额取到分（记账币种 USD / CNY 的最小单位）。
const creditedAmountPlaces = 2

// RoundCreditedAmount 是折算后入账金额的取整：在线充值、管理员按其它币种调整余额、折算预览共用这一处。
func RoundCreditedAmount(v float64) float64 {
	return decimal.NewFromFloat(v).Round(creditedAmountPlaces).InexactFloat64()
}
