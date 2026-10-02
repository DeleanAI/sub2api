package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 两段响应是 2026-10-03 从 edge 容器里实际请求到的原文（CoinGecko 截掉了无关币种）。
const cfetsCentralParitySample = `{"head":{"version":"2.0","provider":"CWAP","req_code":"","rep_code":"200","rep_message":"","ts":1790965196719,"producer":"","tstext":"2026-10-03 02:19:56"},"data":{"head":["USD/CNY","EUR/CNY"],"total":3,"pageTotal":1,"searchlist":["USD/CNY"],"endDate":"2026-10-03","pageSize":20,"currency":"USD/CNY","pageNum":1,"flagMessage":"","startDate":"2026-09-25"},"records":[{"date":"2026-09-30","values":["6.7351"]},{"date":"2026-09-29","values":["6.7411"]},{"date":"2026-09-28","values":["6.7399"]}]}`

const coinGeckoTetherSample = `{"id":"tether","symbol":"usdt","name":"Tether","market_data":{"current_price":{"usd":0.9997228229236466,"cny":6.702841610856173}}}`

func TestExchangeRateFetcher_ParsesCFETSCentralParity(t *testing.T) {
	var gotQuery, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, cfetsCentralParityPath, r.URL.Path)
		gotQuery, gotReferer = r.URL.RawQuery, r.Header.Get("Referer")
		_, _ = w.Write([]byte(cfetsCentralParitySample))
	}))
	defer srv.Close()

	fetcher := NewExchangeRateFetcher(srv.Client(), srv.URL, srv.URL)
	rates, err := fetcher.FetchCNYCentralParity(context.Background(),
		time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 2, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Contains(t, gotQuery, "currency=USD%2FCNY")
	require.Contains(t, gotQuery, "startDate=2026-09-25")
	require.Contains(t, gotQuery, "endDate=2026-10-03")
	require.Equal(t, cfetsCentralParityPage, gotReferer)
	require.Len(t, rates, 3)

	r := rates[0]
	require.Equal(t, "CNY", r.Currency)
	require.Equal(t, "2026-09-30", r.RateDate)
	require.Equal(t, 6.7351, r.Quote)
	require.Equal(t, "CNY per USD", r.QuoteUnit)
	require.InDelta(t, 1/6.7351, r.USDPerUnit, 1e-15)
	require.Equal(t, service.FXSourceCFETSCentralParity, r.Source)
	require.Equal(t, time.Date(2026, 9, 30, 1, 15, 0, 0, time.UTC), r.PublishedAt, "published at 09:15 Asia/Shanghai")
}

func TestExchangeRateFetcher_ParsesCoinGeckoDailyPrice(t *testing.T) {
	var gotPath, gotDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotDate = r.URL.Path, r.URL.Query().Get("date")
		_, _ = w.Write([]byte(coinGeckoTetherSample))
	}))
	defer srv.Close()

	fetcher := NewExchangeRateFetcher(srv.Client(), srv.URL, srv.URL)
	rate, err := fetcher.FetchCoinGeckoDailyPrice(context.Background(), "USDT", "tether", time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "/api/v3/coins/tether/history", gotPath)
	require.Equal(t, "02-10-2026", gotDate)
	require.Equal(t, "USDT", rate.Currency)
	require.Equal(t, "2026-10-02", rate.RateDate)
	require.Equal(t, 0.9997228229236466, rate.USDPerUnit)
	require.Equal(t, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), rate.PublishedAt)
}

func TestExchangeRateFetcher_RejectsErrorsAndMalformedData(t *testing.T) {
	for name, body := range map[string]string{
		"rep_code error":   `{"head":{"rep_code":"500","rep_message":"busy"},"records":[]}`,
		"non-numeric rate": `{"head":{"rep_code":"200"},"records":[{"date":"2026-09-30","values":["n/a"]}]}`,
		"bad date":         `{"head":{"rep_code":"200"},"records":[{"date":"30/09/2026","values":["6.7"]}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := NewExchangeRateFetcher(srv.Client(), srv.URL, srv.URL).FetchCNYCentralParity(context.Background(), time.Now(), time.Now())
		require.Error(t, err, name)
		srv.Close()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := NewExchangeRateFetcher(srv.Client(), srv.URL, srv.URL).FetchCoinGeckoDailyPrice(context.Background(), "USDC", "usd-coin", time.Now())
	require.Error(t, err)

	noData := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"usd-coin"}`))
	}))
	defer noData.Close()
	_, err = NewExchangeRateFetcher(noData.Client(), noData.URL, noData.URL).FetchCoinGeckoDailyPrice(context.Background(), "USDC", "usd-coin", time.Now())
	require.Error(t, err)
}
