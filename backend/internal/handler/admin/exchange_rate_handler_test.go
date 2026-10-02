package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 汇率存档桩：只记 CNY 中间价。
type stubExchangeRateRepo struct{ rates []service.ExchangeRate }

func (r *stubExchangeRateRepo) UpsertExchangeRates(_ context.Context, rates []service.ExchangeRate) error {
	r.rates = append(r.rates, rates...)
	return nil
}

func (r *stubExchangeRateRepo) LatestExchangeRatePublishedBy(_ context.Context, currency string, at time.Time) (*service.ExchangeRate, error) {
	var best *service.ExchangeRate
	for i := range r.rates {
		rate := r.rates[i]
		if rate.Currency == currency && !rate.PublishedAt.After(at) && (best == nil || rate.PublishedAt.After(best.PublishedAt)) {
			best = &rate
		}
	}
	return best, nil
}

func (r *stubExchangeRateRepo) GetExchangeRate(context.Context, string, string) (*service.ExchangeRate, error) {
	return nil, nil
}

func (r *stubExchangeRateRepo) ListExchangeRates(context.Context, string, int) ([]service.ExchangeRate, error) {
	return r.rates, nil
}

// 汇率源桩：返回「昨天」公布的 6.7351。
type stubExchangeRateFetcher struct{}

func (stubExchangeRateFetcher) FetchCNYCentralParity(context.Context, time.Time, time.Time) ([]service.ExchangeRate, error) {
	published := time.Now().Add(-24 * time.Hour)
	return []service.ExchangeRate{{Currency: "CNY", RateDate: published.Format(time.DateOnly), USDPerUnit: 1 / 6.7351, Quote: 6.7351,
		QuoteUnit: "CNY per USD", Source: service.FXSourceCFETSCentralParity, PublishedAt: published}}, nil
}

func (stubExchangeRateFetcher) FetchCoinGeckoDailyPrice(context.Context, string, string, time.Time) (*service.ExchangeRate, error) {
	return nil, context.DeadlineExceeded
}

type stubAccounting string

func (a stubAccounting) BalanceCurrency(context.Context) service.BalanceCurrency {
	return service.BalanceCurrency{Code: string(a)}
}

func newExchangeRateRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewExchangeRateHandler(service.NewExchangeRateService(&stubExchangeRateRepo{}, stubExchangeRateFetcher{}, stubAccounting("USD")))
	router := gin.New()
	router.GET("/exchange-rates", h.List)
	router.GET("/exchange-rates/currencies", h.Currencies)
	router.GET("/exchange-rates/convert", h.Convert)
	return router
}

func getExchangeRateJSON(t *testing.T, router *gin.Engine, url string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body
}

func TestExchangeRateHandler_CurrenciesIsTheSingleSourceOfOptions(t *testing.T) {
	code, body := getExchangeRateJSON(t, newExchangeRateRouter(), "/exchange-rates/currencies")
	require.Equal(t, http.StatusOK, code)
	data := body["data"].(map[string]any)
	require.Equal(t, "USD", data["accounting_currency"])
	require.Equal(t, []any{"CNY", "USD", "USDC", "USDT"}, data["currencies"])
	require.Equal(t, []any{"CNY", "USD"}, data["fiat_currencies"])
	require.Equal(t, service.DefaultPriceCurrency, data["default_price_currency"])
}

func TestExchangeRateHandler_ConvertPreviewsTheCreditedAmount(t *testing.T) {
	router := newExchangeRateRouter()

	code, body := getExchangeRateJSON(t, router, "/exchange-rates/convert?amount=1000&currency=cny")
	require.Equal(t, http.StatusOK, code, body)
	data := body["data"].(map[string]any)
	require.Equal(t, "CNY", data["currency"])
	require.Equal(t, "USD", data["to"])
	require.Equal(t, 148.48, data["converted"], "1000 / 6.7351, rounded to cents like the real credit")
	conversion := data["conversion"].(map[string]any)
	require.Equal(t, service.FXSourceCFETSCentralParity, conversion["legs"].([]any)[0].(map[string]any)["source"])

	code, body = getExchangeRateJSON(t, router, "/exchange-rates/convert?amount=9.99&currency=USD&to=CNY")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 67.28, body["data"].(map[string]any)["converted"], "plan price preview in the gateway currency")

	for _, bad := range []string{
		"/exchange-rates/convert?amount=0&currency=CNY",
		"/exchange-rates/convert?amount=1&currency=EUR",
		"/exchange-rates/convert?amount=1&currency=CNY&to=USDT",
	} {
		code, _ = getExchangeRateJSON(t, router, bad)
		require.Equal(t, http.StatusBadRequest, code, bad)
	}

	code, _ = getExchangeRateJSON(t, router, "/exchange-rates/convert?amount=1&currency=USDT")
	require.Equal(t, http.StatusServiceUnavailable, code, "no stablecoin price for today: refuse rather than guess")
}
