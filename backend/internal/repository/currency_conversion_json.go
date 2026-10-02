package repository

import "github.com/Wei-Shaw/sub2api/internal/service"

// currencyConversionArg 是 raw SQL 写 currency_conversion（JSONB）的参数：nil → NULL，否则 JSON 文本。
// 编解码本身只在 service.EncodeCurrencyConversion / service.DecodeCurrencyConversion。
func currencyConversionArg(conversion *service.CurrencyConversion) (any, error) {
	raw, err := service.EncodeCurrencyConversion(conversion)
	if err != nil || raw == nil {
		return nil, err
	}
	return string(raw), nil
}
