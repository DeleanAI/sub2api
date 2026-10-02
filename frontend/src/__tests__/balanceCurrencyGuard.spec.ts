/**
 * 站内余额单位的护栏（规则见 utils/balanceCurrency.ts）。
 *
 * 余额、价格、用量费用、限额在界面上的符号由系统设置 balance_currency 决定。这里遍历全部源码与
 * 全部文案（不点名具体字段），拦下写死的 "$" / USD / 美元；再在生产所用的 JIT 编译模式下验证接线：
 * 站点设成 CNY 时，模板全局属性与每一条引用了 CURRENCY 的文案都写成 "¥"，改设置后随之刷新。
 * 确实是真实币种（支付单、上游账单）或根本不是金额的地方登记在豁免表里并写明理由；
 * 豁免不再命中任何内容时测试也会失败，豁免表不会悄悄过期。
 */
import { describe, expect, it, vi } from 'vitest'

// 与生产构建一致：vue-i18n 运行时版本 + JIT 编译（vite.config.ts 的 __INTLIFY_JIT_COMPILATION__）。
vi.hoisted(() => {
  Object.assign(globalThis, { __INTLIFY_JIT_COMPILATION__: true })
})

import { createApp, defineComponent, h, nextTick } from 'vue'
import { createPinia, setActivePinia } from 'pinia'
import en from '../i18n/locales/en'
import zh from '../i18n/locales/zh'
import { i18n, loadLocaleMessages } from '../i18n'
import { SITE_MESSAGE_NAMESPACE } from '../i18n/siteMessages'
import { useAppStore } from '../stores/app'
import { installBalanceCurrency } from '../utils/balanceCurrencyInstall'
import type { PublicSettings } from '../types'

// ---------------------------------------------------------------- 源码

const SOURCE_RULES: Array<[string, RegExp]> = [
  ['模板里写死的 "$" 前缀，用 {{ $currency }}', /\$\{\{/],
  ['模板字符串里写死的 "$" 前缀，用 balanceCurrencySymbol()', /\$\$\{/],
  ['值为 "$" 的字符串，用 balanceCurrencySymbol() / $currency', /(['"`])[-+]?\$\1/],
  ['以 "$" 开头的金额字符串，用 withBalanceCurrencySymbol()', /(['"`])[-+]?\$(?:0|\d+\.\d)/],
  ['模板里单独的 "$" 标签，用 {{ $currency }}', />\s*[-+]?\$\s*</],
  ['写死的 "$" 单位标注（如 ($)、$/M），用 {{ $currency }}', /[(>\s]\$(?=[/)])/],
  ['按 USD 格式化站内金额，用 formatCurrency() / withBalanceCurrencySymbol()', /currency:\s*['"]USD['"]/],
  ['把站内金额当成 USD 支付币种，用 $currency', /currencySymbol\(\s*['"]USD['"]\s*\)/]
]

interface SourceExemption {
  file: string
  /** 行内标记；不写表示整个文件 */
  marker?: string
  reason: string
}

const SOURCE_EXEMPTIONS: SourceExemption[] = [
  { file: 'components/payment/currency.ts', reason: '支付单币种的符号表：真实货币，不是站内单位' },
  { file: 'utils/balanceCurrency.ts', reason: '默认符号的唯一出处' },
  { file: 'components/account/AccountUsageCell.vue', marker: 'grokPrepaidMoneyLine.prepaid', reason: 'xAI 预付余额：上游账单的真实美元' },
  { file: 'views/HomeView.vue', marker: 'code-prompt', reason: '命令行示例里的 shell 提示符，不是金额' }
]

function sourceFiles(): Array<[string, string]> {
  const files = import.meta.glob('../**/*.{ts,vue}', { query: '?raw', import: 'default', eager: true }) as Record<string, string>
  return Object.entries(files)
    .map(([path, source]) => [path.replace(/^\.\.\//, ''), source] as [string, string])
    .filter(([path]) => !path.startsWith('./') && !path.includes('__tests__/') && !/\.(spec|test)\.ts$/.test(path) && !path.endsWith('.d.ts') && !path.startsWith('i18n/locales/'))
}

// ---------------------------------------------------------------- 文案

const MESSAGE_RULES: Array<[string, RegExp]> = [
  ['写死的 "$"，用 CURRENCY（i18n/siteMessages.ts）', /\$(?![A-Za-z_])/],
  ['写死的 USD，用 CURRENCY', /\bUSD\b/],
  ['写死的 美元 / 美金，用 CURRENCY', /美元|美金/],
  ['写死的 dollar，用 CURRENCY', /\bdollars?\b/i]
]

interface MessageExemption {
  key: string
  /** 只放行这些片段；不写表示整条放行 */
  allow?: string[]
  reason: string
}

const MESSAGE_EXEMPTIONS: MessageExemption[] = [
  { key: 'payment.admin.currencyPlaceholder', reason: '支付通道的币种代码示例（真实货币）' },
  { key: 'admin.settings.payment.field_paymentCurrencyHint', reason: '支付通道可选的币种（真实货币）' },
  { key: 'admin.accounts.usageWindow.grokUsed', reason: 'xAI 账单的真实美元' },
  { key: 'admin.accounts.usageWindow.grokBalance', reason: 'xAI 账单的真实美元' },
  { key: 'admin.accounts.usageWindow.grokMonthlyLimit', reason: 'xAI 账单的真实美元' },
  { key: 'admin.accounts.usageWindow.grokOverageShort', reason: 'xAI 账单的真实美元' },
  { key: 'admin.accounts.headerOverride.invalidName', reason: 'HTTP 头名允许的字符集，不是金额' },
  { key: 'admin.groups.webSearchPricing.pricePerCallHint', allow: ['$10'], reason: 'OpenAI 官方价格以美元标价' },
]

type Tree = Record<string, unknown>

function leaves(tree: unknown, prefix = ''): Array<[string, string]> {
  if (typeof tree === 'string') return [[prefix, tree]]
  if (!tree || typeof tree !== 'object' || Array.isArray(tree)) return []
  return Object.entries(tree as Tree).flatMap(([k, v]) => leaves(v, prefix ? `${prefix}.${k}` : k))
}

// ---------------------------------------------------------------- tests

describe('站内余额单位护栏', () => {
  it('源码不写死站内金额的 "$" / USD', () => {
    const used = new Set<SourceExemption>()
    const violations: string[] = []
    for (const [file, source] of sourceFiles()) {
      const wholeFile = SOURCE_EXEMPTIONS.find((e) => e.file === file && !e.marker)
      source.split('\n').forEach((line, i) => {
        for (const [rule, rx] of SOURCE_RULES) {
          if (!rx.test(line)) continue
          const exemption = wholeFile ?? SOURCE_EXEMPTIONS.find((e) => e.file === file && e.marker && line.includes(e.marker))
          if (exemption) {
            used.add(exemption)
            continue
          }
          violations.push(`${file}:${i + 1} ${rule}\n    ${line.trim()}`)
        }
      })
    }
    expect(violations).toEqual([])
    expect(SOURCE_EXEMPTIONS.filter((e) => !used.has(e)).map((e) => `${e.file} ${e.marker ?? ''}`)).toEqual([])
  })

  it('文案不写死站内金额的 "$" / USD / 美元', () => {
    const used = new Set<MessageExemption>()
    const violations: string[] = []
    for (const [locale, messages] of [['en', en], ['zh', zh]] as const) {
      for (const [key, message] of leaves(messages)) {
        const exemption = MESSAGE_EXEMPTIONS.find((e) => e.key === key)
        let checked = message
        if (exemption?.allow) {
          for (const fragment of exemption.allow) {
            if (checked.includes(fragment)) used.add(exemption)
            checked = checked.split(fragment).join('')
          }
        }
        for (const [rule, rx] of MESSAGE_RULES) {
          if (!rx.test(checked)) continue
          if (exemption && !exemption.allow) {
            used.add(exemption)
            continue
          }
          violations.push(`${locale}:${key} ${rule}\n    ${message.slice(0, 160)}`)
        }
      }
    }
    expect(violations).toEqual([])
    expect(MESSAGE_EXEMPTIONS.filter((e) => !used.has(e)).map((e) => e.key)).toEqual([])
  })

  it('语言包不占用站点消息的命名空间', () => {
    expect(Object.keys(en)).not.toContain(SITE_MESSAGE_NAMESPACE)
    expect(Object.keys(zh)).not.toContain(SITE_MESSAGE_NAMESPACE)
  })

  it('接线：站点设为 CNY 时模板与文案都写成 "¥"，改设置后随之刷新', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useAppStore()
    store.cachedPublicSettings = { balance_currency: 'CNY', balance_currency_symbol: '¥' } as PublicSettings

    const app = createApp(defineComponent({ render() { return h('span', `${this.$currency}1.00`) } }))
    app.use(pinia)
    installBalanceCurrency(app)
    await loadLocaleMessages('en')
    await loadLocaleMessages('zh')

    const el = document.createElement('div')
    app.mount(el)
    expect(el.textContent).toBe('¥1.00')

    // 每一条引用了站内单位符号的文案，渲染出来都带着站点的符号、不残留链接语法。
    let rendered = 0
    for (const locale of ['en', 'zh'] as const) {
      for (const [key, message] of leaves(locale === 'en' ? en : zh)) {
        if (!message.includes(`@:{'${SITE_MESSAGE_NAMESPACE}.`)) continue
        const text = i18n.global.t(key, {}, { locale })
        expect(text, `${locale}:${key}`).toContain('¥')
        expect(text, `${locale}:${key}`).not.toContain('@:')
        expect(text, `${locale}:${key}`).not.toContain(`${SITE_MESSAGE_NAMESPACE}.`)
        rendered++
      }
    }
    expect(rendered).toBeGreaterThan(0)

    store.cachedPublicSettings = { balance_currency: 'EUR', balance_currency_symbol: '€' } as PublicSettings
    await nextTick()
    expect(el.textContent).toBe('€1.00')
    expect(i18n.global.t('keys.quotaAmount', {}, { locale: 'zh' })).toContain('€')
    app.unmount()
  })
})
