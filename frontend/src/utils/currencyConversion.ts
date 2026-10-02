/**
 * 折算依据（CurrencyConversion）的写法，余额流水、调整余额预览、用量明细共用。
 * 这里的金额都是真实货币（折算的两端），不是站内单位，所以按币种代码写符号。
 */
import type { CurrencyConversion, ExchangeRateLeg } from '@/types'
import { withBalanceCurrencySymbol } from '@/utils/balanceCurrency'

/** 真实货币金额：法币按 Intl 写成 ¥1,000.00 / $148.48；Intl 不认识的代码（USDT、USDC）写成「1,000.00 USDT」。 */
export function formatMoney(amount: number, currency: string, locale?: string): string {
  const code = String(currency || '').trim().toUpperCase()
  const value = Number.isFinite(amount) ? amount : 0
  try {
    return new Intl.NumberFormat(locale || undefined, {
      style: 'currency',
      currency: code,
      currencyDisplay: 'narrowSymbol',
      minimumFractionDigits: 2,
      maximumFractionDigits: 2
    }).format(value)
  } catch {
    return `${value.toLocaleString(locale || undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })} ${code}`
  }
}

const QUOTE_UNIT = /^([A-Z]+) per ([A-Z]+)$/

/** 报价原样写成「1 USD = 6.7351 CNY」/「1 USDT = 0.9997 USD」（取自 quote_unit，不在前端换算）。 */
export function formatQuote(leg: ExchangeRateLeg): string {
  const match = QUOTE_UNIT.exec(leg.quote_unit || '')
  const quote = Number(leg.quote)
  const digits = quote >= 1 ? 4 : 6
  if (!match) return `${quote.toFixed(digits)} ${leg.quote_unit}`
  return `1 ${match[2]} = ${quote.toFixed(digits)} ${match[1]}`
}

/** 报价来源对应的文案键（currencyConversion.source.*）；不认识的来源原样显示。 */
export function quoteSourceKey(source: string): string | null {
  switch (source) {
    case 'cfets_central_parity':
      return 'currencyConversion.source.cfets'
    case 'coingecko_daily_price':
      return 'currencyConversion.source.coingecko'
    default:
      return null
  }
}

/**
 * 订单金额（payment_orders.amount 及按同一币种计的退款金额）的写法：amount_currency 为空表示站内余额单位，
 * 写成站内符号；否则按真实币种写（订阅订单的套餐价、跨币种充值的入账币种）。
 */
export function formatOrderAmount(amount: number, amountCurrency?: string | null, locale?: string): string {
  const value = Number.isFinite(amount) ? amount : 0
  if (!amountCurrency) return withBalanceCurrencySymbol(value.toFixed(2))
  return formatMoney(value, amountCurrency, locale)
}

/** 有原币金额与目标金额时返回「¥1,000.00 → $148.48」，否则 null（计费的折算只有汇率）。 */
export function formatConversionAmounts(conversion: CurrencyConversion, locale?: string): string | null {
  if (conversion.from_amount == null || conversion.to_amount == null) return null
  return `${formatMoney(conversion.from_amount, conversion.from_currency, locale)} → ${formatMoney(conversion.to_amount, conversion.to_currency, locale)}`
}
