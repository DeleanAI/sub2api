/**
 * 管理端币种选项（记账币种、可折算币种、可标价法币、默认标价币种）的唯一来源：
 * GET /admin/exchange-rates/currencies。全站共用一份，第一次用到时取；取失败不缓存，下次再取。
 * 记账币种在设置页保存后会变，保存后调用 invalidateCurrencyOptions()。
 */
import { readonly, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { ExchangeRateCurrencies } from '@/api/admin/exchangeRates'

const options = ref<ExchangeRateCurrencies | null>(null)
let pending: Promise<ExchangeRateCurrencies> | null = null

export function loadCurrencyOptions(): Promise<ExchangeRateCurrencies> {
  if (!pending) {
    pending = Promise.resolve().then(() => adminAPI.exchangeRates.currencies()).then(
      (data) => {
        options.value = data
        return data
      },
      (err) => {
        pending = null
        throw err
      }
    )
  }
  return pending
}

export function invalidateCurrencyOptions(): void {
  pending = null
  void loadCurrencyOptions().catch((err) => console.error('Failed to reload currency options:', err))
}

export function useCurrencyOptions() {
  void loadCurrencyOptions().catch((err) => console.error('Failed to load currency options:', err))
  return { options: readonly(options) }
}
