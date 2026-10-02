package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 汇率源：
//   - 人民币对美元中间价：中国外汇交易中心「人民币汇率中间价」历史数据接口（工作日北京时间 9:15 公布），
//     https://www.chinamoney.com.cn/chinese/bkccpr/ 页面使用的 CcprHisNew 接口。
//   - 稳定币：CoinGecko /coins/{id}/history（指定 UTC 日期 00:00 的价格）。
const (
	defaultCFETSBaseURL     = "https://www.chinamoney.com.cn"
	cfetsCentralParityPath  = "/ags/ms/cm-u-bk-ccpr/CcprHisNew"
	cfetsCentralParityPage  = "https://www.chinamoney.com.cn/chinese/bkccpr/"
	defaultCoinGeckoBaseURL = "https://api.coingecko.com"
)

// beijing：外汇交易中心的日期按北京时间（与 service 的公布时刻同一时区）。
var beijing = service.CNYCentralParityZone

type exchangeRateFetcher struct {
	httpClient       *http.Client
	cfetsBaseURL     string
	coinGeckoBaseURL string
}

// NewExchangeRateFetcher 创建汇率源客户端；baseURL 为空时用官方地址（测试里指向本地桩）。
func NewExchangeRateFetcher(httpClient *http.Client, cfetsBaseURL, coinGeckoBaseURL string) service.ExchangeRateFetcher {
	if cfetsBaseURL == "" {
		cfetsBaseURL = defaultCFETSBaseURL
	}
	if coinGeckoBaseURL == "" {
		coinGeckoBaseURL = defaultCoinGeckoBaseURL
	}
	return &exchangeRateFetcher{
		httpClient:       httpClient,
		cfetsBaseURL:     strings.TrimRight(cfetsBaseURL, "/"),
		coinGeckoBaseURL: strings.TrimRight(coinGeckoBaseURL, "/"),
	}
}

// ProvideExchangeRateFetcher 按与价格目录相同的出站代理配置（update.proxy_url）创建汇率源客户端。
func ProvideExchangeRateFetcher(cfg *config.Config) service.ExchangeRateFetcher {
	proxyURL, allowDirect := cfg.Update.ProxyURL, cfg.Security.ProxyFallback.AllowDirectOnError
	client, err := httpclient.GetClient(httpclient.Options{Timeout: 20 * time.Second, ProxyURL: proxyURL})
	if err != nil {
		if strings.TrimSpace(proxyURL) != "" && !allowDirect {
			slog.Warn("proxy client init failed, exchange rate requests will fail", "service", "exchange_rate", "error", err)
			return &exchangeRateFetcherError{err: fmt.Errorf("proxy client init failed and direct fallback is disabled: %w", err)}
		}
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return NewExchangeRateFetcher(client, "", "")
}

type exchangeRateFetcherError struct{ err error }

func (f *exchangeRateFetcherError) FetchCNYCentralParity(context.Context, time.Time, time.Time) ([]service.ExchangeRate, error) {
	return nil, f.err
}

func (f *exchangeRateFetcherError) FetchCoinGeckoDailyPrice(context.Context, string, string, time.Time) (*service.ExchangeRate, error) {
	return nil, f.err
}

type cfetsCentralParityResponse struct {
	Head struct {
		RepCode    string `json:"rep_code"`
		RepMessage string `json:"rep_message"`
	} `json:"head"`
	Records []struct {
		Date   string   `json:"date"`
		Values []string `json:"values"`
	} `json:"records"`
}

func (f *exchangeRateFetcher) FetchCNYCentralParity(ctx context.Context, from, to time.Time) ([]service.ExchangeRate, error) {
	start := from.In(beijing).Format(time.DateOnly)
	end := to.In(beijing).Format(time.DateOnly)
	query := url.Values{
		"startDate": {start}, "endDate": {end}, "currency": {"USD/CNY"},
		"pageNum": {"1"}, "pageSize": {"100"},
	}
	endpoint := f.cfetsBaseURL + cfetsCentralParityPath + "?" + query.Encode()
	body, err := f.get(ctx, endpoint, map[string]string{"Referer": cfetsCentralParityPage})
	if err != nil {
		return nil, err
	}
	var parsed cfetsCentralParityResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode CFETS response: %w", err)
	}
	if parsed.Head.RepCode != "" && parsed.Head.RepCode != "200" {
		return nil, fmt.Errorf("CFETS rep_code %s: %s", parsed.Head.RepCode, parsed.Head.RepMessage)
	}
	fetchedAt := time.Now()
	rates := make([]service.ExchangeRate, 0, len(parsed.Records))
	for _, record := range parsed.Records {
		day, err := time.ParseInLocation(time.DateOnly, record.Date, beijing)
		if err != nil || len(record.Values) == 0 {
			return nil, fmt.Errorf("CFETS record %+v: unexpected shape", record)
		}
		cnyPerUSD, err := strconv.ParseFloat(strings.TrimSpace(record.Values[0]), 64)
		if err != nil || cnyPerUSD <= 0 {
			return nil, fmt.Errorf("CFETS record %s: invalid central parity %q", record.Date, record.Values[0])
		}
		rates = append(rates, service.ExchangeRate{
			Currency:    "CNY",
			RateDate:    record.Date,
			USDPerUnit:  1 / cnyPerUSD,
			Quote:       cnyPerUSD,
			QuoteUnit:   "CNY per USD",
			Source:      service.FXSourceCFETSCentralParity,
			SourceURL:   cfetsCentralParityPage,
			PublishedAt: service.CNYCentralParityPublishedAt(day), // 当天 9:15 起生效
			FetchedAt:   fetchedAt,
		})
	}
	return rates, nil
}

type coinGeckoHistoryResponse struct {
	MarketData *struct {
		CurrentPrice map[string]float64 `json:"current_price"`
	} `json:"market_data"`
}

func (f *exchangeRateFetcher) FetchCoinGeckoDailyPrice(ctx context.Context, currency, coinID string, date time.Time) (*service.ExchangeRate, error) {
	day := date.UTC()
	endpoint := fmt.Sprintf("%s/api/v3/coins/%s/history?%s", f.coinGeckoBaseURL, url.PathEscape(coinID),
		url.Values{"date": {day.Format("02-01-2006")}, "localization": {"false"}}.Encode())
	body, err := f.get(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var parsed coinGeckoHistoryResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode CoinGecko response: %w", err)
	}
	if parsed.MarketData == nil {
		return nil, fmt.Errorf("CoinGecko has no market data for %s on %s", coinID, day.Format(time.DateOnly))
	}
	usd, ok := parsed.MarketData.CurrentPrice["usd"]
	if !ok || usd <= 0 {
		return nil, fmt.Errorf("CoinGecko has no USD price for %s on %s", coinID, day.Format(time.DateOnly))
	}
	return &service.ExchangeRate{
		Currency:    currency,
		RateDate:    day.Format(time.DateOnly),
		USDPerUnit:  usd,
		Quote:       usd,
		QuoteUnit:   "USD per " + currency,
		Source:      service.FXSourceCoinGeckoDaily,
		SourceURL:   fmt.Sprintf("https://www.coingecko.com/en/coins/%s", coinID),
		PublishedAt: time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
		FetchedAt:   time.Now(),
	}, nil
}

func (f *exchangeRateFetcher) get(ctx context.Context, endpoint string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; sub2api exchange-rate)")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", req.URL.Host+req.URL.Path, resp.StatusCode)
	}
	return body, nil
}
