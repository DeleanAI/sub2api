import { describe, expect, it } from 'vitest'
import { formatConversionAmounts, formatMoney, formatOrderAmount, formatQuote, quoteSourceKey } from '../currencyConversion'

const cnyLeg = {
  currency: 'CNY', rate_date: '2026-09-30', usd_per_unit: 1 / 6.7351, quote: 6.7351, quote_unit: 'CNY per USD',
  source: 'cfets_central_parity', source_url: '', published_at: '', fetched_at: '',
}

describe('currency conversion formatting', () => {
  it('writes real currencies with their own symbol and unknown codes with the code', () => {
    expect(formatMoney(1000, 'CNY', 'en-US')).toBe('¥1,000.00')
    expect(formatMoney(148.48, 'USD', 'en-US')).toBe('$148.48')
    expect(formatMoney(100, 'USDT', 'en-US')).toBe('100.00 USDT')
  })

  it('writes quotes the way the source quotes them', () => {
    expect(formatQuote(cnyLeg)).toBe('1 USD = 6.7351 CNY')
    expect(formatQuote({ ...cnyLeg, currency: 'USDT', quote: 0.9997228229236466, quote_unit: 'USD per USDT' })).toBe('1 USDT = 0.999723 USD')
    expect(quoteSourceKey('cfets_central_parity')).toBe('currencyConversion.source.cfets')
    expect(quoteSourceKey('something_else')).toBeNull()
  })

  it('shows original and converted amounts only when both are recorded', () => {
    const conversion = { from_currency: 'CNY', to_currency: 'USD', rate: 1 / 6.7351, at: '', legs: [cnyLeg] }
    expect(formatConversionAmounts({ ...conversion, from_amount: 1000, to_amount: 148.48 }, 'en-US')).toBe('¥1,000.00 → $148.48')
    expect(formatConversionAmounts(conversion, 'en-US')).toBeNull()
  })

  it('writes order amounts in their own currency, or in the site unit when none is given', () => {
    expect(formatOrderAmount(9.99, 'USD', 'en-US')).toBe('$9.99')
    expect(formatOrderAmount(30, 'CNY', 'en-US')).toBe('¥30.00')
    expect(formatOrderAmount(148.48, '')).toBe('$148.48') // 测试环境的站内单位是默认 USD
  })
})
