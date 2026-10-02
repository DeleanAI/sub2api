//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 计费时刻：2026-10-02（国庆）北京时间 10:00，生效的是 09-30 中间价 6.7351。
var holidayUsageTime = time.Date(2026, 10, 2, 10, 0, 0, 0, cst)

func newFXBillingFixture(t *testing.T, accounting string, pricing *ChannelModelPricing, model string) (*BillingService, *ModelPricingResolver, *fakeFetcher) {
	t.Helper()
	fx, _, fetcher := newTestFX(holidayUsageTime, accounting)
	bs := newTestBillingService()
	bs.SetExchangeRateService(fx)
	cs := newTestChannelServiceWithCache(t, &channelCache{
		pricingByGroupModel:     map[channelModelKey]*ChannelModelPricing{{groupID: 7, model: model}: pricing},
		channelByGroupID:        map[int64]*Channel{7: {ID: 7, Status: StatusActive}},
		groupPlatform:           map[int64]string{7: ""},
		wildcardByGroupPlatform: map[channelGroupPlatformKey][]*wildcardPricingEntry{},
		mappingByGroupModel:     map[channelModelKey]string{},
		wildcardMappingByGP:     map[channelGroupPlatformKey][]*wildcardMappingEntry{},
		byID:                    map[int64]*Channel{},
	})
	return bs, NewModelPricingResolver(cs, bs), fetcher
}

func TestBillingFX_CNYPriceCardIsConvertedAtTheUsageDaysParity(t *testing.T) {
	// 方舟 Seedance 的人民币标价：输出 70 元 / 百万 token，输入 0。
	pricing := &ChannelModelPricing{Currency: "CNY", BillingMode: BillingModeToken, InputPrice: testPtrFloat64(0), OutputPrice: testPtrFloat64(70e-6)}
	bs, resolver, _ := newFXBillingFixture(t, "USD", pricing, "doubao-seedance-2-5")
	groupID := int64(7)

	cost, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "doubao-seedance-2-5", GroupID: &groupID,
		Tokens: UsageTokens{InputTokens: 1000, OutputTokens: 100_000}, RateMultiplier: 0.8,
		Resolver: resolver, PricingAt: holidayUsageTime,
	})
	require.NoError(t, err)
	require.InDelta(t, 7.0/6.7351, cost.TotalCost, 1e-12, "100k tokens x 70 CNY/MTok = 7 CNY, in USD")
	require.InDelta(t, 7.0/6.7351*0.8, cost.ActualCost, 1e-12, "the configured multiplier applies after conversion")
	require.InDelta(t, 0, cost.InputCost, 1e-15)

	conv := cost.CurrencyConversion
	require.NotNil(t, conv)
	require.Equal(t, "CNY", conv.FromCurrency)
	require.Equal(t, "USD", conv.ToCurrency)
	require.InDelta(t, 1/6.7351, conv.Rate, 1e-15)
	require.Equal(t, "2026-09-30", conv.Legs[0].RateDate)
	require.False(t, conv.Stale)

	log := &UsageLog{}
	log.applyCostBreakdown(cost)
	require.Same(t, conv, log.CurrencyConversion, "the usage log keeps the rate it was billed with")
	require.Equal(t, cost.ActualCost, log.ActualCost)
}

func TestBillingFX_PerRequestCNYPriceIsConverted(t *testing.T) {
	pricing := &ChannelModelPricing{Currency: "CNY", BillingMode: BillingModePerRequest, PerRequestPrice: testPtrFloat64(1.5)}
	bs, resolver, _ := newFXBillingFixture(t, "USD", pricing, "doubao-seedream")
	groupID := int64(7)
	cost, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "doubao-seedream", GroupID: &groupID, RequestCount: 2, RateMultiplier: 1,
		Resolver: resolver, PricingAt: holidayUsageTime,
	})
	require.NoError(t, err)
	require.InDelta(t, 3.0/6.7351, cost.TotalCost, 1e-12)
	require.NotNil(t, cost.CurrencyConversion)
}

func TestBillingFX_PriceCardInTheAccountingCurrencyIsNotConverted(t *testing.T) {
	pricing := &ChannelModelPricing{Currency: "CNY", BillingMode: BillingModeToken, InputPrice: testPtrFloat64(0), OutputPrice: testPtrFloat64(70e-6)}
	bs, resolver, fetcher := newFXBillingFixture(t, "CNY", pricing, "doubao-seedance-2-5")
	groupID := int64(7)
	cost, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "doubao-seedance-2-5", GroupID: &groupID,
		Tokens: UsageTokens{OutputTokens: 100_000}, RateMultiplier: 1, Resolver: resolver, PricingAt: holidayUsageTime,
	})
	require.NoError(t, err)
	require.InDelta(t, 7.0, cost.TotalCost, 1e-12)
	require.Nil(t, cost.CurrencyConversion)
	require.Zero(t, fetcher.cnyCalls)
}

func TestBillingFX_NonUSDCardDoesNotInheritTheUSDCatalog(t *testing.T) {
	// gpt-5.4 在官方价目录里（缓存读 $0.25/MTok）。人民币条目自成一体：没写的缓存读价按条目自己的输入价，不取美元目录价。
	pricing := &ChannelModelPricing{Currency: "CNY", BillingMode: BillingModeToken, InputPrice: testPtrFloat64(10e-6), OutputPrice: testPtrFloat64(60e-6)}
	bs, resolver, _ := newFXBillingFixture(t, "USD", pricing, "gpt-5.4")
	groupID := int64(7)
	cost, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "gpt-5.4", GroupID: &groupID,
		Tokens: UsageTokens{InputTokens: 100_000, CacheReadTokens: 100_000, OutputTokens: 100_000}, RateMultiplier: 1,
		Resolver: resolver, PricingAt: holidayUsageTime,
	})
	require.NoError(t, err)
	require.InDelta(t, 1/6.7351, cost.InputCost, 1e-9)
	require.InDelta(t, 1/6.7351, cost.CacheReadCost, 1e-9, "unset cache read = the card's own input price, never 0 and never the USD catalog")
	require.InDelta(t, 6/6.7351, cost.OutputCost, 1e-9)
}

func TestBillingFX_USDCardStillInheritsTheCatalog(t *testing.T) {
	pricing := &ChannelModelPricing{Currency: "USD", BillingMode: BillingModeToken, OutputPrice: testPtrFloat64(12e-6)}
	bs, resolver, fetcher := newFXBillingFixture(t, "USD", pricing, "gpt-5.4")
	groupID := int64(7)
	cost, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "gpt-5.4", GroupID: &groupID,
		Tokens: UsageTokens{InputTokens: 100_000, CacheReadTokens: 100_000, OutputTokens: 100_000}, RateMultiplier: 1,
		Resolver: resolver, PricingAt: holidayUsageTime,
	})
	require.NoError(t, err)
	require.InDelta(t, 0.25, cost.InputCost, 1e-9, "official input price $2.5/MTok (below the 272K long-context threshold)")
	require.InDelta(t, 0.025, cost.CacheReadCost, 1e-9, "official cached input price $0.25/MTok")
	require.InDelta(t, 1.2, cost.OutputCost, 1e-9, "the card's own output price $12/MTok")
	require.Nil(t, cost.CurrencyConversion)
	require.Zero(t, fetcher.cnyCalls)
}

func TestBillingFX_UsesTheArchivedParityWhenTheSourceIsDownAndSaysSo(t *testing.T) {
	pricing := &ChannelModelPricing{Currency: "CNY", BillingMode: BillingModeToken, InputPrice: testPtrFloat64(0), OutputPrice: testPtrFloat64(70e-6)}
	bs, resolver, fetcher := newFXBillingFixture(t, "USD", pricing, "doubao-seedance-2-5")
	groupID := int64(7)
	input := CostInput{
		Ctx: context.Background(), Model: "doubao-seedance-2-5", GroupID: &groupID,
		Tokens: UsageTokens{OutputTokens: 100_000}, RateMultiplier: 1, Resolver: resolver, PricingAt: holidayUsageTime,
	}
	_, err := bs.CalculateCostUnified(input) // 第一次：抓到并存档
	require.NoError(t, err)

	fetcher.cnyErr = errors.New("chinamoney.com.cn unreachable")
	bs.fx.now = func() time.Time { return holidayUsageTime.Add(2 * time.Hour) } // 过了刷新间隔
	input.PricingAt = holidayUsageTime.Add(2 * time.Hour)
	cost, err := bs.CalculateCostUnified(input)
	require.NoError(t, err, "billing must not drop usage because the FX source is down")
	require.InDelta(t, 7.0/6.7351, cost.TotalCost, 1e-12)
	require.True(t, cost.CurrencyConversion.Stale)
	require.NotEmpty(t, cost.CurrencyConversion.StaleReason)
}

func TestBillingFX_WithoutAnyRateTheCardIsTreatedAsUnpriced(t *testing.T) {
	pricing := &ChannelModelPricing{Currency: "CNY", BillingMode: BillingModeToken, InputPrice: testPtrFloat64(0), OutputPrice: testPtrFloat64(70e-6)}
	bs, resolver, fetcher := newFXBillingFixture(t, "USD", pricing, "doubao-seedance-2-5")
	fetcher.cnyErr = errors.New("chinamoney.com.cn unreachable")
	groupID := int64(7)
	_, err := bs.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "doubao-seedance-2-5", GroupID: &groupID,
		Tokens: UsageTokens{OutputTokens: 100_000}, RateMultiplier: 1, Resolver: resolver, PricingAt: holidayUsageTime,
	})
	require.ErrorIs(t, err, ErrModelPricingUnavailable, "never bill CNY numbers as USD")

	unwired := newTestBillingService()
	_, err = unwired.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "doubao-seedance-2-5", GroupID: &groupID,
		Tokens: UsageTokens{OutputTokens: 100_000}, RateMultiplier: 1,
		Resolver: NewModelPricingResolver(resolver.channelService, unwired), PricingAt: holidayUsageTime,
	})
	require.ErrorIs(t, err, ErrModelPricingUnavailable)
}

func TestNormalizePricingCurrencies(t *testing.T) {
	entries := []ChannelModelPricing{
		{Models: []string{"a"}, InputPrice: testPtrFloat64(1)},
		{Models: []string{"b"}, Currency: " cny ", InputPrice: testPtrFloat64(0), OutputPrice: testPtrFloat64(1)},
		{Models: []string{"c"}, Currency: "CNY", BillingMode: BillingModePerRequest, PerRequestPrice: testPtrFloat64(2)},
	}
	require.NoError(t, normalizePricingCurrencies(entries))
	require.Equal(t, "USD", entries[0].Currency)
	require.Equal(t, "CNY", entries[1].Currency)
	require.Equal(t, "CNY", entries[2].Currency)

	for name, bad := range map[string]ChannelModelPricing{
		"stablecoin is not a price currency": {Models: []string{"x"}, Currency: "USDT", InputPrice: testPtrFloat64(1), OutputPrice: testPtrFloat64(1)},
		"unknown currency":                   {Models: []string{"x"}, Currency: "EUR", InputPrice: testPtrFloat64(1), OutputPrice: testPtrFloat64(1)},
		"CNY token card without output":      {Models: []string{"x"}, Currency: "CNY", InputPrice: testPtrFloat64(1)},
		"CNY token card without input":       {Models: []string{"x"}, Currency: "CNY", OutputPrice: testPtrFloat64(1)},
	} {
		require.Error(t, normalizePricingCurrencies([]ChannelModelPricing{bad}), name)
	}
}

func TestPricingInAccounting_ShowsForeignPricesConvertedOrNotAtAll(t *testing.T) {
	fx, _, fetcher := newTestFX(time.Now(), "USD")
	fetcher.fixings[time.Now().In(cst).Format(time.DateOnly)] = 6.7351
	card := &ChannelModelPricing{Currency: "CNY", InputPrice: testPtrFloat64(6.7351e-6), OutputPrice: testPtrFloat64(67.351e-6)}

	shown := fx.PricingInAccounting(context.Background(), card)
	require.Equal(t, "USD", shown.Currency)
	require.InDelta(t, 1e-6, *shown.InputPrice, 1e-15)
	require.InDelta(t, 10e-6, *shown.OutputPrice, 1e-15)
	require.Equal(t, "CNY", card.Currency, "the stored card is untouched")

	var unwired *ExchangeRateService
	hidden := unwired.PricingInAccounting(context.Background(), card)
	require.Nil(t, hidden.InputPrice, "no rate: show nothing rather than CNY numbers labelled as USD")
	require.Nil(t, hidden.OutputPrice)

	usd := &ChannelModelPricing{InputPrice: testPtrFloat64(1e-6)}
	require.Same(t, usd, fx.PricingInAccounting(context.Background(), usd))
}
