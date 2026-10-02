import { describe, expect, it } from 'vitest'
import { conversionRate, creditedBalancePreview, multiplyRound, roundHalfUp, subscriptionGatewayBasePreview } from '../exchange'

// 与后端单测同一组数（payment_order_fx_test.go）：国庆期间沿用 09-30 中间价 6.7351。
const rates = {
  USD: { usd_per_unit: 1 },
  CNY: { usd_per_unit: 1 / 6.7351, rate_date: '2026-09-30', source: 'cfets_central_parity' },
}

describe('checkout exchange preview mirrors the backend order math', () => {
  it('converts a CNY top-up into the accounting currency, rounded to cents', () => {
    expect(creditedBalancePreview(1000, 'CNY', 'USD', 1, rates)).toBe(148.48)
    expect(creditedBalancePreview(1000, 'CNY', 'USD', 1.1, rates)).toBe(163.33)
    expect(creditedBalancePreview(88, 'CNY', 'CNY', 1, rates)).toBe(88)
    expect(creditedBalancePreview(88, 'CNY', 'CNY', 1.5, rates)).toBe(132)
  })

  it('converts plan prices into the gateway currency', () => {
    expect(subscriptionGatewayBasePreview(9.99, 'USD', 'CNY', 2, rates)).toBe(67.28)
    expect(subscriptionGatewayBasePreview(30, 'CNY', 'CNY', 2, rates)).toBe(30)
  })

  it('refuses to guess without a rate', () => {
    expect(conversionRate(rates, 'HKD', 'USD')).toBeNull()
    expect(creditedBalancePreview(100, 'HKD', 'USD', 1, rates)).toBeNull()
    expect(subscriptionGatewayBasePreview(10, 'USD', 'HKD', 2, rates)).toBeNull()
    expect(creditedBalancePreview(100, 'CNY', 'USD', 1, undefined)).toBeNull()
  })

  it('rounds like shopspring/decimal: shortest decimal representation, half away from zero', () => {
    expect(roundHalfUp(1.005, 2)).toBe(1.01) // 二进制里 1.005 略小于 1.005，Math.round(x*100)/100 会得到 1
    expect(roundHalfUp(148.475894938, 2)).toBe(148.48)
    expect(roundHalfUp(2.5, 0)).toBe(3)
    expect(roundHalfUp(1e-7, 2)).toBe(0)
    expect(multiplyRound(148.48, 1.1, 2)).toBe(163.33) // 浮点乘法得 163.32800000000003，十进制精确是 163.328
    expect(multiplyRound(0.145, 1, 2)).toBe(0.15)
    expect(multiplyRound(10, 0.14, 2)).toBe(1.4)
  })
})
