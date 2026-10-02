package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ExchangeRateHandler 管理端汇率接口：折算预览（调整余额前看一眼会入账多少）与汇率存档。
type ExchangeRateHandler struct {
	fx *service.ExchangeRateService
}

// NewExchangeRateHandler 创建管理端汇率处理器。
func NewExchangeRateHandler(fx *service.ExchangeRateService) *ExchangeRateHandler {
	return &ExchangeRateHandler{fx: fx}
}

type exchangeRateCurrenciesResponse struct {
	// AccountingCurrency 是站内记账币种；Currencies 是可折算币种（含稳定币，调整余额可选）；
	// FiatCurrencies 是能作记账 / 标价 / 折算目标的法币；DefaultPriceCurrency 是价格没写币种时的币种。
	AccountingCurrency   string   `json:"accounting_currency"`
	Currencies           []string `json:"currencies"`
	FiatCurrencies       []string `json:"fiat_currencies"`
	DefaultPriceCurrency string   `json:"default_price_currency"`
}

// Currencies 是管理端各处币种选项的唯一来源（设置页的记账币种、调整余额、定价条目的标价币种）。
// GET /api/v1/admin/exchange-rates/currencies
func (h *ExchangeRateHandler) Currencies(c *gin.Context) {
	response.Success(c, exchangeRateCurrenciesResponse{
		AccountingCurrency:   h.fx.AccountingCurrency(c.Request.Context()),
		Currencies:           service.SupportedFXCurrencies(false),
		FiatCurrencies:       service.SupportedFXCurrencies(true),
		DefaultPriceCurrency: service.DefaultPriceCurrency,
	})
}

type exchangeRateConvertResponse struct {
	Amount             float64                     `json:"amount"`
	Currency           string                      `json:"currency"`
	To                 string                      `json:"to"`
	AccountingCurrency string                      `json:"accounting_currency"`
	Converted          float64                     `json:"converted"`
	Conversion         *service.CurrencyConversion `json:"conversion"`
}

// Convert 按当前汇率把 amount（currency）折算成 to（默认站内记账币种）并取到分，口径与调整余额、下单一致
// （FXStrict：确认不了当前汇率就报错）。管理端用它预览调整余额的入账金额、套餐价在网关币种下的扣款。
// GET /api/v1/admin/exchange-rates/convert?amount=1000&currency=CNY[&to=USD]
func (h *ExchangeRateHandler) Convert(c *gin.Context) {
	amount, err := strconv.ParseFloat(strings.TrimSpace(c.Query("amount")), 64)
	if err != nil || amount <= 0 {
		response.BadRequest(c, "amount must be a positive number")
		return
	}
	ctx := c.Request.Context()
	accounting := h.fx.AccountingCurrency(ctx)
	currency, err := service.NormalizeFXCurrency(c.DefaultQuery("currency", accounting))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	to, err := service.NormalizeFiatCurrency(c.DefaultQuery("to", accounting))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	converted, conversion, err := h.fx.Convert(ctx, amount, currency, to, time.Now(), service.FXStrict)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, exchangeRateConvertResponse{
		Amount:             amount,
		Currency:           currency,
		To:                 to,
		AccountingCurrency: accounting,
		Converted:          service.RoundCreditedAmount(converted),
		Conversion:         conversion,
	})
}

// List 返回某个币种最近的存档汇率。
// GET /api/v1/admin/exchange-rates?currency=CNY&limit=30
func (h *ExchangeRateHandler) List(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	rates, err := h.fx.ListRates(c.Request.Context(), c.DefaultQuery("currency", "CNY"), limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"accounting_currency": h.fx.AccountingCurrency(c.Request.Context()),
		"rates":               rates,
	})
}
