/**
 * Admin exchange-rate API.
 *
 * 站内只有一个记账币种；以其它币种支付或标价的金额按交易时刻汇率折算进来（人民币按中国外汇交易中心中间价，
 * USDT / USDC 按 CoinGecko 当日价）。币种选项只从 currencies() 来，界面不自己写一份。
 */

import { apiClient } from '../client'
import type { CurrencyConversion, ExchangeRateLeg } from '@/types'

export interface ExchangeRateCurrencies {
  /** 站内记账币种 */
  accounting_currency: string
  /** 可折算币种（含稳定币）：调整余额可选 */
  currencies: string[]
  /** 能作记账 / 标价 / 折算目标的法币 */
  fiat_currencies: string[]
  /** 价格没写币种时按它 */
  default_price_currency: string
}

export interface ExchangeRateConversion {
  amount: number
  currency: string
  to: string
  accounting_currency: string
  /** 折算后取到分，与实际入账同一取整 */
  converted: number
  /** 同币种时为 null */
  conversion: CurrencyConversion | null
}

export interface ExchangeRateArchive {
  accounting_currency: string
  rates: ExchangeRateLeg[]
}

export async function currencies(): Promise<ExchangeRateCurrencies> {
  const { data } = await apiClient.get<ExchangeRateCurrencies>('/admin/exchange-rates/currencies')
  return data
}

/** 按当前汇率把 amount（currency）折算成 to（缺省记账币种）；确认不了当前汇率时后端报错。 */
export async function convert(amount: number, currency: string, to?: string): Promise<ExchangeRateConversion> {
  const { data } = await apiClient.get<ExchangeRateConversion>('/admin/exchange-rates/convert', {
    params: { amount, currency, ...(to ? { to } : {}) }
  })
  return data
}

export async function list(currency = 'CNY', limit = 30): Promise<ExchangeRateArchive> {
  const { data } = await apiClient.get<ExchangeRateArchive>('/admin/exchange-rates', { params: { currency, limit } })
  return data
}

export const exchangeRatesAPI = { currencies, convert, list }

export default exchangeRatesAPI
