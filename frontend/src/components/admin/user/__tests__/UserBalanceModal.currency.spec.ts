import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import UserBalanceModal from '../UserBalanceModal.vue'
import type { AdminUser } from '@/types'

const mocks = vi.hoisted(() => ({
  users: { updateBalance: vi.fn() },
  exchangeRates: { currencies: vi.fn(), convert: vi.fn() },
  store: { showError: vi.fn(), showSuccess: vi.fn() },
}))
vi.mock('@/api/admin', () => ({ adminAPI: { users: mocks.users, exchangeRates: mocks.exchangeRates } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks.store }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key, locale: { value: 'en' } }),
}))
enableAutoUnmount(afterEach)

const conversion = {
  from_currency: 'CNY', to_currency: 'USD', from_amount: 1000, to_amount: 148.48, rate: 1 / 6.7351, at: '2026-10-02T02:00:00Z',
  legs: [{ currency: 'CNY', rate_date: '2026-09-30', usd_per_unit: 1 / 6.7351, quote: 6.7351, quote_unit: 'CNY per USD',
    source: 'cfets_central_parity', source_url: 'https://www.chinamoney.com.cn/chinese/bkccpr/', published_at: '2026-09-30T01:15:00Z', fetched_at: '2026-10-02T02:00:00Z' }],
}

function mountModal(operation: 'add' | 'subtract' = 'add', balance = 20) {
  return mount(UserBalanceModal, {
    props: { show: false, operation, user: { id: 7, email: 'shenyan@example.com', balance } as AdminUser },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } },
  })
}

async function open(wrapper: ReturnType<typeof mountModal>) {
  await flushPromises()
  await wrapper.setProps({ show: true })
  await flushPromises()
}

async function settle() {
  await flushPromises()
  vi.advanceTimersByTime(300)
  await flushPromises()
}

describe('UserBalanceModal currency', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    mocks.exchangeRates.currencies.mockResolvedValue({
      accounting_currency: 'USD', currencies: ['CNY', 'USD', 'USDC', 'USDT'], fiat_currencies: ['CNY', 'USD'], default_price_currency: 'USD',
    })
  })
  afterEach(() => vi.useRealTimers())

  it('offers the backend currencies and defaults to the accounting currency', async () => {
    const wrapper = mountModal()
    await open(wrapper)
    const select = wrapper.get<HTMLSelectElement>('[data-testid="balance-currency"]')
    expect(select.findAll('option').map(o => o.text())).toEqual(['CNY', 'USD', 'USDC', 'USDT'])
    expect(select.element.value).toBe('USD')
  })

  it('previews a CNY deposit at the current rate and sends the currency along', async () => {
    mocks.exchangeRates.convert.mockResolvedValue({ amount: 1000, currency: 'CNY', to: 'USD', accounting_currency: 'USD', converted: 148.48, conversion })
    mocks.users.updateBalance.mockResolvedValue({})
    const wrapper = mountModal()
    await open(wrapper)

    await wrapper.get('[data-testid="balance-currency"]').setValue('CNY')
    await wrapper.get('input[type="number"]').setValue('1000')
    await settle()

    expect(mocks.exchangeRates.convert).toHaveBeenLastCalledWith(1000, 'CNY')
    expect(wrapper.get('[data-testid="balance-credited"]').text()).toContain('148.48')
    expect(wrapper.text()).toContain('1 USD = 6.7351 CNY')
    expect(wrapper.text()).toContain('168.48') // 20 + 148.48

    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(mocks.users.updateBalance).toHaveBeenCalledWith(7, 1000, 'add', '', 'CNY')
  })

  it('blocks the adjustment when the rate cannot be confirmed', async () => {
    mocks.exchangeRates.convert.mockRejectedValue({ message: 'exchange rate unavailable' })
    const wrapper = mountModal()
    await open(wrapper)

    await wrapper.get('[data-testid="balance-currency"]').setValue('USDT')
    await wrapper.get('input[type="number"]').setValue('100')
    await settle()

    expect(wrapper.get('[data-testid="balance-preview-error"]').text()).toContain('exchange rate unavailable')
    expect(wrapper.get('button[type="submit"]').attributes('disabled')).toBeDefined()
  })

  it('checks a foreign-currency withdrawal against the converted amount', async () => {
    mocks.exchangeRates.convert.mockResolvedValue({ amount: 1000, currency: 'CNY', to: 'USD', accounting_currency: 'USD', converted: 148.48, conversion })
    const wrapper = mountModal('subtract', 100)
    await open(wrapper)

    await wrapper.get('[data-testid="balance-currency"]').setValue('CNY')
    await wrapper.get('input[type="number"]').setValue('1000')
    await settle()
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(mocks.store.showError).toHaveBeenCalledWith('admin.users.insufficientBalance')
    expect(mocks.users.updateBalance).not.toHaveBeenCalled()
  })
})
