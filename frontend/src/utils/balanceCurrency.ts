/**
 * 站内记账币种在界面上的写法。
 *
 * 余额、价格、用量费用、限额都以同一个记账币种计。系统设置 balance_currency 决定它（只能是有汇率来源的法币，
 * 默认 USD），后端据此推导符号并经公开设置下发（balance_currency_symbol）。以其它币种支付或标价的金额由后端
 * 按交易时刻汇率折算成它（折算依据见 utils/currencyConversion.ts）；界面这里只决定怎么写，不做换算。
 *
 * 界面上凡是站内金额都经这里取符号：模板里用全局属性 `$currency`，脚本里用 balanceCurrencySymbol()，
 * 文案里用链接消息 CURRENCY（i18n/siteMessages.ts）；这两处接线在 utils/balanceCurrencyInstall.ts，
 * 本模块不依赖 i18n 实例，任何组件 / 工具都能直接用。支付单的真实币种（CNY / USD …）、上游账号的账单
 * 不是这个单位，分别走 components/payment/currency.ts 与各自的数据。
 * 源码与文案里写死 "$" / USD 的站内金额会被 __tests__/balanceCurrencyGuard.spec.ts 拦下。
 */
import { useAppStore } from '@/stores/app'

/** 公开设置到达之前的写法，与后端默认币种（USD）一致。 */
export const DEFAULT_BALANCE_CURRENCY = 'USD'
export const DEFAULT_BALANCE_CURRENCY_SYMBOL = '$'

function configured(value: unknown, fallback: string): string {
  const trimmed = typeof value === 'string' ? value.trim() : ''
  return trimmed || fallback
}

/** 站内余额单位的符号（"$"、"¥" …），写在金额前面。 */
export function balanceCurrencySymbol(): string {
  return configured(useAppStore().cachedPublicSettings?.balance_currency_symbol, DEFAULT_BALANCE_CURRENCY_SYMBOL)
}

/** 站内余额单位的币种代码（"USD"、"CNY" …），给需要代码的地方（如客户端导入脚本里的 unit）。 */
export function balanceCurrencyCode(): string {
  return configured(useAppStore().cachedPublicSettings?.balance_currency, DEFAULT_BALANCE_CURRENCY)
}

/** 把已格式化好的数字写成站内金额；负数写成 "-¥1.00" 而不是 "¥-1.00"（与后端 BalanceCurrency.Format 同一写法）。 */
export function withBalanceCurrencySymbol(formatted: string | number): string {
  const text = String(formatted)
  const symbol = balanceCurrencySymbol()
  return text.startsWith('-') ? `-${symbol}${text.slice(1)}` : `${symbol}${text}`
}
