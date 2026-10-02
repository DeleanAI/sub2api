//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// 2026-10-02 是国庆假期，外汇交易中心不公布中间价：下单按最近一次公布的 09-30 中间价 6.7351 折算。
var holidayOrderTime = time.Date(2026, 10, 2, 10, 0, 0, 0, cst)

func newFXPaymentService(accounting string) (*PaymentService, *fakeFetcher) {
	fx, _, fetcher := newTestFX(holidayOrderTime, accounting)
	return &PaymentService{exchangeRates: fx}, fetcher
}

func TestCreateOrderAmounts_BalanceTopUpIsConvertedAtTheDaysCentralParity(t *testing.T) {
	svc, _ := newFXPaymentService("USD")
	amounts, err := svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{Amount: 1000},
		nil, &PaymentConfig{BalanceRechargeMultiplier: 1}, "CNY", holidayOrderTime)
	require.NoError(t, err)
	require.Equal(t, 148.48, amounts.orderAmount, "1000 CNY / 6.7351, rounded to cents")
	require.Equal(t, 1000.0, amounts.gatewayBase)
	require.Equal(t, "1000.00", amounts.payAmountStr)
	require.NotNil(t, amounts.conversion)
	require.Equal(t, "CNY", amounts.conversion.FromCurrency)
	require.Equal(t, "USD", amounts.conversion.ToCurrency)
	require.Equal(t, 1000.0, *amounts.conversion.FromAmount)
	require.Equal(t, 148.48, *amounts.conversion.ToAmount)
	require.Equal(t, "2026-09-30", amounts.conversion.Legs[0].RateDate)
	require.Equal(t, FXSourceCFETSCentralParity, amounts.conversion.Legs[0].Source)
}

func TestCreateOrderAmounts_RechargeMultiplierAppliesAfterConversion(t *testing.T) {
	svc, _ := newFXPaymentService("USD")
	amounts, err := svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{Amount: 1000},
		nil, &PaymentConfig{BalanceRechargeMultiplier: 1.1, RechargeFeeRate: 2.5}, "CNY", holidayOrderTime)
	require.NoError(t, err)
	require.Equal(t, 163.33, amounts.orderAmount, "148.48 x 1.1")
	require.Equal(t, 148.48, *amounts.conversion.ToAmount, "the conversion record keeps the pre-multiplier amount")
	require.Equal(t, "1025.00", amounts.payAmountStr, "the fee is charged on the payment currency amount")
}

func TestCreateOrderAmounts_SameCurrencyTopUpIsNotConverted(t *testing.T) {
	svc, fetcher := newFXPaymentService("CNY")
	amounts, err := svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{Amount: 88},
		nil, &PaymentConfig{BalanceRechargeMultiplier: 1}, "CNY", holidayOrderTime)
	require.NoError(t, err)
	require.Equal(t, 88.0, amounts.orderAmount)
	require.Nil(t, amounts.conversion)
	require.Zero(t, fetcher.cnyCalls)
}

func TestCreateOrderAmounts_SubscriptionPriceIsConvertedToTheGatewayCurrency(t *testing.T) {
	svc, _ := newFXPaymentService("USD")
	plan := &dbent.SubscriptionPlan{Price: 9.99} // 未写币种 = USD
	amounts, err := svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{},
		plan, &PaymentConfig{RechargeFeeRate: 2.5}, "CNY", holidayOrderTime)
	require.NoError(t, err)
	require.Equal(t, 9.99, amounts.orderAmount, "the order keeps the plan price in the plan currency")
	require.Equal(t, 67.28, amounts.gatewayBase, "9.99 USD x 6.7351")
	require.Equal(t, "68.97", amounts.payAmountStr, "fee 1.682 rounds up to 1.69")
	require.Equal(t, "USD", amounts.conversion.FromCurrency)
	require.Equal(t, "CNY", amounts.conversion.ToCurrency)
	require.Equal(t, 9.99, *amounts.conversion.FromAmount)
	require.Equal(t, 67.28, *amounts.conversion.ToAmount)
}

func TestCreateOrderAmounts_SubscriptionPricedInTheGatewayCurrencyIsChargedAsIs(t *testing.T) {
	svc, fetcher := newFXPaymentService("USD")
	for _, tc := range []struct{ planCurrency, gateway string }{{"CNY", "CNY"}, {"", "USD"}, {"hkd", "HKD"}} {
		plan := &dbent.SubscriptionPlan{Price: 30, Currency: tc.planCurrency}
		amounts, err := svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{},
			plan, &PaymentConfig{}, tc.gateway, holidayOrderTime)
		require.NoError(t, err, tc)
		require.Equal(t, 30.0, amounts.gatewayBase, tc)
		require.Nil(t, amounts.conversion, tc)
	}
	require.Zero(t, fetcher.cnyCalls)
}

func TestCreateOrderAmounts_RefusesWhenTheRateCannotBeConfirmed(t *testing.T) {
	svc, fetcher := newFXPaymentService("USD")
	fetcher.cnyErr = errors.New("chinamoney.com.cn unreachable")
	_, err := svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{Amount: 100},
		nil, &PaymentConfig{BalanceRechargeMultiplier: 1}, "CNY", holidayOrderTime)
	require.ErrorIs(t, err, ErrExchangeRateUnavailable)

	_, err = svc.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{Amount: 100},
		nil, &PaymentConfig{BalanceRechargeMultiplier: 1}, "HKD", holidayOrderTime)
	require.Error(t, err, "a gateway currency without a rate source cannot be credited in USD")

	unwired := &PaymentService{}
	_, err = unwired.computeCreateOrderAmounts(context.Background(), CreateOrderRequest{Amount: 100},
		nil, &PaymentConfig{BalanceRechargeMultiplier: 1}, "CNY", holidayOrderTime)
	require.ErrorIs(t, err, ErrExchangeRateUnavailable, "no FX service must refuse, not panic or credit 1:1")
}

func TestCheckoutExchangeRates_ReportsWhatTheOrderWillUse(t *testing.T) {
	svc, fetcher := newFXPaymentService("USD")
	delete(fetcher.coinPrices, "USDC|2026-10-02")
	accounting, rates := svc.CheckoutExchangeRates(context.Background(), []string{"CNY", "cny", "USD", "HKD", "USDT", "USDC"})
	require.Equal(t, "USD", accounting)
	require.Equal(t, CheckoutExchangeRate{USDPerUnit: 1}, rates["USD"])
	require.InDelta(t, 1/6.7351, rates["CNY"].USDPerUnit, 1e-15)
	require.Equal(t, "2026-09-30", rates["CNY"].RateDate)
	require.Equal(t, FXSourceCFETSCentralParity, rates["CNY"].Source)
	require.Equal(t, CheckoutExchangeRate{USDPerUnit: 0.9997228229236466, RateDate: "2026-10-02", Source: FXSourceCoinGeckoDaily}, rates["USDT"])
	require.NotContains(t, rates, "HKD", "no rate source")
	require.NotContains(t, rates, "USDC", "no price for the day: no preview, and the order would be refused too")
	require.Len(t, rates, 3)
}

func TestPaymentOrderAmountCurrency(t *testing.T) {
	encode := func(c *CurrencyConversion) []byte {
		raw, err := EncodeCurrencyConversion(c)
		require.NoError(t, err)
		return raw
	}
	cnyToUSD := encode(&CurrencyConversion{FromCurrency: "CNY", ToCurrency: "USD", Rate: 1 / 6.7351})
	usdToCNY := encode(&CurrencyConversion{FromCurrency: "USD", ToCurrency: "CNY", Rate: 6.7351})

	require.Equal(t, "", PaymentOrderAmountCurrency(&dbent.PaymentOrder{OrderType: "balance"}),
		"same-currency and pre-FX top-ups were credited in the balance unit")
	require.Equal(t, "USD", PaymentOrderAmountCurrency(&dbent.PaymentOrder{OrderType: "balance", CurrencyConversion: cnyToUSD}))
	require.Equal(t, "USD", PaymentOrderAmountCurrency(&dbent.PaymentOrder{OrderType: "subscription", CurrencyConversion: usdToCNY}),
		"a subscription order keeps the plan price in the plan currency")
	require.Equal(t, "CNY", PaymentOrderAmountCurrency(&dbent.PaymentOrder{OrderType: "subscription"}),
		"without a conversion the plan price was charged in the payment currency")
	require.Nil(t, PaymentOrderCurrencyConversion(&dbent.PaymentOrder{OrderType: "balance"}))
	require.Equal(t, "CNY", PaymentOrderCurrencyConversion(&dbent.PaymentOrder{CurrencyConversion: cnyToUSD}).FromCurrency)
}
