package service

import (
	"fmt"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func paymentProviderConfigCurrency(providerKey string, cfg map[string]string) string {
	switch strings.TrimSpace(providerKey) {
	case payment.TypeStripe, payment.TypeAirwallex:
		currency, err := payment.NormalizePaymentCurrency(cfg["currency"])
		if err == nil {
			return currency
		}
	}
	return payment.DefaultPaymentCurrency
}

func PaymentOrderCurrency(order *dbent.PaymentOrder) string {
	if snapshot := psOrderProviderSnapshot(order); snapshot != nil {
		if currency, err := payment.NormalizePaymentCurrency(snapshot.Currency); err == nil {
			return currency
		}
	}
	return payment.DefaultPaymentCurrency
}

// PaymentOrderAmountCurrency 是 payment_orders.amount 的币种，空串表示站内余额单位（记账币种）：
//   - 余额充值：amount 是入账金额。跨币种下单时取折算目标币种；同币种订单与折算上线前的老订单都按余额单位入账，返回空。
//   - 订阅：amount 是套餐价。跨币种下单时取折算来源币种（套餐币种）；否则套餐价就是按支付币种收的。
func PaymentOrderAmountCurrency(order *dbent.PaymentOrder) string {
	if order == nil {
		return ""
	}
	conversion := DecodeCurrencyConversion(order.CurrencyConversion, fmt.Sprintf("payment_order %d", order.ID))
	if order.OrderType == payment.OrderTypeSubscription {
		if conversion != nil {
			return conversion.FromCurrency
		}
		return PaymentOrderCurrency(order)
	}
	if conversion != nil {
		return conversion.ToCurrency
	}
	return ""
}

// PaymentOrderCurrencyConversion 返回订单下单时的折算依据（同币种为 nil），给订单接口展示。
func PaymentOrderCurrencyConversion(order *dbent.PaymentOrder) *CurrencyConversion {
	if order == nil {
		return nil
	}
	return DecodeCurrencyConversion(order.CurrencyConversion, fmt.Sprintf("payment_order %d", order.ID))
}
