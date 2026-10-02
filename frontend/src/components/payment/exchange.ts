/**
 * 结算页的折算预览，与后端下单同一套算法（backend/internal/service/payment_order.go）：
 *   - 折算率 rate(from→to) = usd_per_unit[from] / usd_per_unit[to]，金额 × rate（同币种为 1）；
 *   - 余额充值到账 = 取到分(取到分(支付金额 × rate(支付币种→记账币种)) × 充值倍率)；
 *   - 订阅扣款基数 = 取到网关币种最小单位(套餐价 × rate(套餐币种→网关币种))。
 * 取整与后端 shopspring/decimal 一致：先取浮点数的最短十进制表示，再四舍五入（half away from zero），
 * 倍率乘法按十进制精确计算。实际金额以下单结果为准；缺某个币种的汇率时返回 null，页面不给预览（下单同样会被拒）。
 */
import type { CheckoutExchangeRate } from '@/types/payment'

export type CheckoutExchangeRates = Record<string, CheckoutExchangeRate>

/** 1 单位 from 合多少 to；同币种为 1，缺汇率为 null。 */
export function conversionRate(rates: CheckoutExchangeRates | undefined, from: string, to: string): number | null {
  if (from === to) return 1
  const fromUSD = rates?.[from]?.usd_per_unit
  const toUSD = rates?.[to]?.usd_per_unit
  if (!fromUSD || !toUSD || !Number.isFinite(fromUSD) || !Number.isFinite(toUSD)) return null
  return fromUSD / toUSD
}

/** 浮点数的最短十进制表示 → (整数, 小数位数)，如 148.48 → (14848n, 2)。 */
function toScaled(value: number): { digits: bigint; scale: number } {
  const [mantissa, exponentPart] = String(value).toLowerCase().split('e')
  const exponent = exponentPart ? Number(exponentPart) : 0
  const negative = mantissa.startsWith('-')
  const [intPart, fracPart = ''] = mantissa.replace('-', '').split('.')
  let digits = BigInt(intPart + fracPart || '0')
  let scale = fracPart.length - exponent
  if (scale < 0) {
    digits *= 10n ** BigInt(-scale)
    scale = 0
  }
  return { digits: negative ? -digits : digits, scale }
}

/** 把 (整数, 小数位数) 四舍五入到 places 位（half away from zero）。 */
function roundScaled(digits: bigint, scale: number, places: number): number {
  if (scale <= places) return Number(`${digits}e-${scale}`)
  const divisor = 10n ** BigInt(scale - places)
  const negative = digits < 0n
  const abs = negative ? -digits : digits
  let quotient = abs / divisor
  if ((abs % divisor) * 2n >= divisor) quotient += 1n
  return Number(`${negative ? -quotient : quotient}e-${places}`)
}

/** 与 decimal.NewFromFloat(value).Round(places) 一致的取整。 */
export function roundHalfUp(value: number, places: number): number {
  if (!Number.isFinite(value)) return 0
  const { digits, scale } = toScaled(value)
  return roundScaled(digits, scale, places)
}

/** 与 decimal.NewFromFloat(a).Mul(decimal.NewFromFloat(b)).Round(places) 一致：十进制精确相乘后取整。 */
export function multiplyRound(a: number, b: number, places: number): number {
  if (!Number.isFinite(a) || !Number.isFinite(b)) return 0
  const x = toScaled(a)
  const y = toScaled(b)
  return roundScaled(x.digits * y.digits, x.scale + y.scale, places)
}

/** 折算后入账到余额的金额取到分（后端 creditedAmountPlaces）。 */
export const CREDITED_AMOUNT_PLACES = 2

/** 余额充值的预计到账（记账币种）；缺汇率时为 null。 */
export function creditedBalancePreview(
  paymentAmount: number,
  paymentCurrency: string,
  accountingCurrency: string,
  multiplier: number,
  rates: CheckoutExchangeRates | undefined,
): number | null {
  if (!(paymentAmount > 0)) return 0
  const rate = conversionRate(rates, paymentCurrency, accountingCurrency)
  if (rate === null) return null
  const converted = paymentCurrency === accountingCurrency ? paymentAmount : roundHalfUp(paymentAmount * rate, CREDITED_AMOUNT_PLACES)
  const effectiveMultiplier = Number.isFinite(multiplier) && multiplier > 0 ? multiplier : 1
  return multiplyRound(converted, effectiveMultiplier, CREDITED_AMOUNT_PLACES)
}

/** 订阅在网关币种下的扣款基数（不含手续费）；缺汇率时为 null。fractionDigits 是网关币种的最小单位位数。 */
export function subscriptionGatewayBasePreview(
  price: number,
  planCurrency: string,
  gatewayCurrency: string,
  fractionDigits: number,
  rates: CheckoutExchangeRates | undefined,
): number | null {
  if (planCurrency === gatewayCurrency) return price
  const rate = conversionRate(rates, planCurrency, gatewayCurrency)
  if (rate === null) return null
  return roundHalfUp(price * rate, fractionDigits)
}
