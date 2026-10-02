//go:build unit

package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// memExchangeRates 是内存里的汇率存档。
type memExchangeRates struct {
	mu    sync.Mutex
	rates map[string]ExchangeRate // currency|date
}

func newMemExchangeRates() *memExchangeRates {
	return &memExchangeRates{rates: map[string]ExchangeRate{}}
}

func (m *memExchangeRates) UpsertExchangeRates(_ context.Context, rates []ExchangeRate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rates {
		m.rates[r.Currency+"|"+r.RateDate] = r
	}
	return nil
}

func (m *memExchangeRates) LatestExchangeRatePublishedBy(_ context.Context, currency string, at time.Time) (*ExchangeRate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *ExchangeRate
	for _, r := range m.rates {
		if r.Currency != currency || r.PublishedAt.After(at) {
			continue
		}
		if best == nil || r.PublishedAt.After(best.PublishedAt) {
			cp := r
			best = &cp
		}
	}
	return best, nil
}

func (m *memExchangeRates) GetExchangeRate(_ context.Context, currency, rateDate string) (*ExchangeRate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rates[currency+"|"+rateDate]; ok {
		return &r, nil
	}
	return nil, nil
}

func (m *memExchangeRates) ListExchangeRates(_ context.Context, currency string, limit int) ([]ExchangeRate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ExchangeRate
	for _, r := range m.rates {
		if r.Currency == currency {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RateDate > out[j].RateDate })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fakeFetcher 按真实接口的口径返回中间价（2026 年中秋 09-25、国庆 10-01~07 不公布）与稳定币价。
type fakeFetcher struct {
	mu         sync.Mutex
	cnyCalls   int
	coinCalls  int
	cnyErr     error
	coinErr    error
	fixings    map[string]float64 // date -> CNY per USD
	coinPrices map[string]float64 // currency|date -> USD
	// publishDelay：中间价在 9:15 之后多久才能从接口取到（模拟公布延迟）。接口只返回已经能取到的中间价。
	publishDelay time.Duration
}

var cst = time.FixedZone("Asia/Shanghai", 8*3600)

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		fixings:    map[string]float64{"2026-09-24": 6.7420, "2026-09-28": 6.7399, "2026-09-29": 6.7411, "2026-09-30": 6.7351},
		coinPrices: map[string]float64{"USDT|2026-10-02": 0.9997228229236466, "USDC|2026-10-02": 0.9999},
	}
}

func (f *fakeFetcher) FetchCNYCentralParity(_ context.Context, from, to time.Time) ([]ExchangeRate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cnyCalls++
	if f.cnyErr != nil {
		return nil, f.cnyErr
	}
	var out []ExchangeRate
	for date, q := range f.fixings {
		day, _ := time.ParseInLocation(time.DateOnly, date, cst)
		published := CNYCentralParityPublishedAt(day)
		if day.Before(time.Date(from.In(cst).Year(), from.In(cst).Month(), from.In(cst).Day(), 0, 0, 0, 0, cst)) || published.Add(f.publishDelay).After(to) {
			continue
		}
		out = append(out, ExchangeRate{Currency: "CNY", RateDate: date, USDPerUnit: 1 / q, Quote: q, QuoteUnit: "CNY per USD",
			Source: FXSourceCFETSCentralParity, PublishedAt: published})
	}
	return out, nil
}

func (f *fakeFetcher) FetchCoinGeckoDailyPrice(_ context.Context, currency, _ string, date time.Time) (*ExchangeRate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.coinCalls++
	if f.coinErr != nil {
		return nil, f.coinErr
	}
	d := date.UTC().Format(time.DateOnly)
	p, ok := f.coinPrices[currency+"|"+d]
	if !ok {
		return nil, errors.New("no data")
	}
	return &ExchangeRate{Currency: currency, RateDate: d, USDPerUnit: p, Quote: p, QuoteUnit: "USD per " + currency,
		Source: FXSourceCoinGeckoDaily, PublishedAt: date.UTC()}, nil
}

type fixedAccounting string

func (c fixedAccounting) BalanceCurrency(context.Context) BalanceCurrency {
	return balanceCurrencyOf(string(c))
}

func newTestFX(now time.Time, accounting string) (*ExchangeRateService, *memExchangeRates, *fakeFetcher) {
	repo, fetcher := newMemExchangeRates(), newFakeFetcher()
	svc := NewExchangeRateService(repo, fetcher, fixedAccounting(accounting))
	current := now
	svc.now = func() time.Time { return current }
	return svc, repo, fetcher
}

func TestExchangeRate_CNYUsesTheLatestFixingPublishedBeforeTheTransaction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, cst) // 国庆假期：最近一次公布的是 09-30
	fx, _, _ := newTestFX(now, "USD")

	usd, conv, err := fx.ConvertToAccounting(ctx, 1000, "CNY", now, FXStrict)
	require.NoError(t, err)
	require.InDelta(t, 1000/6.7351, usd, 1e-9)
	require.Equal(t, "CNY", conv.FromCurrency)
	require.Equal(t, "USD", conv.ToCurrency)
	require.Len(t, conv.Legs, 1)
	require.Equal(t, "2026-09-30", conv.Legs[0].RateDate)
	require.Equal(t, 6.7351, conv.Legs[0].Quote)
	require.False(t, conv.Stale)

	// 09-30 当天 9:15 公布之前，适用的是 09-29 的中间价；9:15 起是 09-30 的。
	before := time.Date(2026, 9, 30, 9, 14, 0, 0, cst)
	_, conv, err = fx.Convert(ctx, 1, "CNY", "USD", before, FXBilling)
	require.NoError(t, err)
	require.Equal(t, "2026-09-29", conv.Legs[0].RateDate)
	_, conv, err = fx.Convert(ctx, 1, "CNY", "USD", time.Date(2026, 9, 30, 9, 15, 0, 0, cst), FXBilling)
	require.NoError(t, err)
	require.Equal(t, "2026-09-30", conv.Legs[0].RateDate)

	// 中秋节（09-25，周五）与周末不公布：09-27 适用 09-24 的中间价。
	_, conv, err = fx.Convert(ctx, 1, "CNY", "USD", time.Date(2026, 9, 27, 12, 0, 0, 0, cst), FXBilling)
	require.NoError(t, err)
	require.Equal(t, "2026-09-24", conv.Legs[0].RateDate)

	// 记账币种是人民币时，美元按中间价换成人民币。
	cnyFX, _, _ := newTestFX(now, "CNY")
	cny, conv, err := cnyFX.ConvertToAccounting(ctx, 10, "USD", now, FXStrict)
	require.NoError(t, err)
	require.InDelta(t, 67.351, cny, 1e-9)
	require.InDelta(t, 6.7351, conv.Rate, 1e-12)
}

func TestExchangeRate_RealtimeRefreshIsThrottledAndConcurrentCallsShareOneFetch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, cst)
	fx, _, fetcher := newTestFX(now, "USD")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := fx.ConvertToAccounting(ctx, 1, "CNY", now, FXStrict)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, fetcher.cnyCalls, "concurrent realtime conversions share one refresh")

	fx.now = func() time.Time { return now.Add(59 * time.Minute) }
	_, _, err := fx.ConvertToAccounting(ctx, 1, "CNY", now.Add(59*time.Minute), FXStrict)
	require.NoError(t, err)
	require.Equal(t, 1, fetcher.cnyCalls, "within an hour of a successful refresh")

	fx.now = func() time.Time { return now.Add(61 * time.Minute) }
	_, _, err = fx.ConvertToAccounting(ctx, 1, "CNY", now.Add(61*time.Minute), FXStrict)
	require.NoError(t, err)
	require.Equal(t, 2, fetcher.cnyCalls)
}

// 当天的中间价 9:15 一公布就要用上：跨过 9:15 马上重取；公布稍有延迟时 9:15 之后两小时内每 5 分钟重试，
// 取到当天的就停；周末不公布，照常每小时一次。
func TestExchangeRate_RefreshesRightAfterTheDailyPublication(t *testing.T) {
	ctx := context.Background()
	at := func(hh, mm int) time.Time { return time.Date(2026, 9, 30, hh, mm, 0, 0, cst) } // 周三
	fx, _, fetcher := newTestFX(at(9, 10), "USD")
	fetcher.publishDelay = 5 * time.Minute // 9:20 才能从接口取到当天的中间价
	convert := func(now time.Time) *CurrencyConversion {
		t.Helper()
		fx.now = func() time.Time { return now }
		_, conv, err := fx.ConvertToAccounting(ctx, 1, "CNY", now, FXStrict)
		require.NoError(t, err)
		return conv
	}

	require.Equal(t, "2026-09-29", convert(at(9, 10)).Legs[0].RateDate)
	require.Equal(t, 1, fetcher.cnyCalls)

	require.Equal(t, "2026-09-29", convert(at(9, 16)).Legs[0].RateDate, "published at 9:15 but not retrievable yet")
	require.Equal(t, 2, fetcher.cnyCalls, "crossing 9:15 triggers a refresh at once")

	convert(at(9, 19))
	require.Equal(t, 2, fetcher.cnyCalls, "retries are at least 5 minutes apart")

	require.Equal(t, "2026-09-30", convert(at(9, 22)).Legs[0].RateDate)
	require.Equal(t, 3, fetcher.cnyCalls, "keeps retrying until today's fixing is archived")

	convert(at(9, 40))
	require.Equal(t, 3, fetcher.cnyCalls, "today's fixing is archived: back to hourly")

	saturday := time.Date(2026, 10, 3, 9, 10, 0, 0, cst)
	convert(saturday)
	require.Equal(t, 4, fetcher.cnyCalls, "more than an hour since the last refresh")
	convert(saturday.Add(10 * time.Minute))
	require.Equal(t, 4, fetcher.cnyCalls, "no fixing is published on weekends")
}

func TestExchangeRate_RefreshFailureRefusesTopUpsButBillingUsesTheArchivedFixingAndSaysSo(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, cst)
	fx, _, fetcher := newTestFX(now, "USD")
	_, _, err := fx.ConvertToAccounting(ctx, 1, "CNY", now, FXStrict) // 先存一份档
	require.NoError(t, err)

	later := now.Add(2 * time.Hour)
	fx.now = func() time.Time { return later }
	fetcher.cnyErr = errors.New("chinamoney unreachable")

	_, _, err = fx.ConvertToAccounting(ctx, 100, "CNY", later, FXStrict)
	require.ErrorIs(t, err, ErrExchangeRateUnavailable)
	require.Contains(t, err.Error(), "cannot confirm the current CNY central parity")

	usd, conv, err := fx.ConvertToAccounting(ctx, 100, "CNY", later, FXBilling)
	require.NoError(t, err)
	require.InDelta(t, 100/6.7351, usd, 1e-9)
	require.True(t, conv.Stale)
	require.Contains(t, conv.StaleReason, "2026-09-30")
}

func TestExchangeRate_HistoricalTransactionsBackfillTheArchive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, cst)
	fx, repo, fetcher := newTestFX(now, "USD")

	at := time.Date(2026, 9, 30, 5, 55, 31, 0, time.UTC) // shenyan 的人民币充值时刻（北京时间 13:55）
	usd, conv, err := fx.ConvertToAccounting(ctx, 1000, "CNY", at, FXStrict)
	require.NoError(t, err)
	require.InDelta(t, 1000/6.7351, usd, 1e-9)
	require.Equal(t, 148.48, RoundCreditedAmount(usd), "credited amounts are rounded to cents")
	require.Equal(t, "2026-09-30", conv.Legs[0].RateDate)
	require.Equal(t, 1, fetcher.cnyCalls)
	archived, err := repo.GetExchangeRate(ctx, "CNY", "2026-09-30")
	require.NoError(t, err)
	require.NotNil(t, archived)

	_, _, err = fx.ConvertToAccounting(ctx, 1, "CNY", at, FXStrict)
	require.NoError(t, err)
	require.Equal(t, 1, fetcher.cnyCalls, "the archive answers historical lookups")
}

func TestExchangeRate_StablecoinsUseTheDailyPriceAndRefuseWithoutIt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)
	fx, repo, fetcher := newTestFX(now, "USD")

	usd, conv, err := fx.ConvertToAccounting(ctx, 500, "usdt", now, FXStrict)
	require.NoError(t, err)
	require.InDelta(t, 500*0.9997228229236466, usd, 1e-9)
	require.Equal(t, "USDT", conv.FromCurrency)
	require.Equal(t, "2026-10-02", conv.Legs[0].RateDate)
	require.Equal(t, FXSourceCoinGeckoDaily, conv.Legs[0].Source)
	_, _, err = fx.ConvertToAccounting(ctx, 1, "USDT", now, FXStrict)
	require.NoError(t, err)
	require.Equal(t, 1, fetcher.coinCalls, "one fetch per currency and UTC day")
	archived, _ := repo.GetExchangeRate(ctx, "USDT", "2026-10-02")
	require.NotNil(t, archived)

	_, _, err = fx.ConvertToAccounting(ctx, 1, "USDC", now.Add(24*time.Hour), FXStrict) // 10-03 没有价
	require.ErrorIs(t, err, ErrExchangeRateUnavailable)

	// 两种非美元币种之间经美元交叉：1 USDT = (0.99972… USD) / (1/6.7351 USD per CNY) CNY
	cnyFX, _, _ := newTestFX(now, "CNY")
	cny, conv, err := cnyFX.ConvertToAccounting(ctx, 1, "USDT", now, FXStrict)
	require.NoError(t, err)
	require.InDelta(t, 0.9997228229236466*6.7351, cny, 1e-9)
	require.Len(t, conv.Legs, 2)
}

func TestExchangeRate_SameCurrencyAndValidation(t *testing.T) {
	ctx := context.Background()
	fx, _, fetcher := newTestFX(time.Now(), "USD")
	amount, conv, err := fx.ConvertToAccounting(ctx, 12.5, "usd", time.Now(), FXStrict)
	require.NoError(t, err)
	require.Equal(t, 12.5, amount)
	require.Nil(t, conv)
	require.Zero(t, fetcher.cnyCalls)

	_, _, err = fx.ConvertToAccounting(ctx, 1, "EUR", time.Now(), FXStrict)
	require.Error(t, err)

	amount, conv, err = fx.Convert(ctx, 30, "hkd", " HKD ", time.Now(), FXStrict)
	require.NoError(t, err, "the same currency needs no rate even when it is not in the FX table")
	require.Equal(t, 30.0, amount)
	require.Nil(t, conv)

	require.Equal(t, 1.01, RoundCreditedAmount(1.005), "half-up on the decimal value, not on the binary float")
	require.Equal(t, 148.48, RoundCreditedAmount(148.475894938))

	_, err = NormalizeFiatCurrency("USDT")
	require.Error(t, err, "stablecoins can be paid in but cannot be an accounting or price currency")
	code, err := NormalizeFiatCurrency(" cny ")
	require.NoError(t, err)
	require.Equal(t, "CNY", code)
	require.Equal(t, []string{"CNY", "USD", "USDC", "USDT"}, SupportedFXCurrencies(false))
	require.Equal(t, []string{"CNY", "USD"}, SupportedFXCurrencies(true))
}
