/**
 * 站内记账币种符号的接线（写法规则见 utils/balanceCurrency.ts）。单独成模块，是为了让 balanceCurrency.ts
 * 不依赖 i18n 实例：取符号的工具到处都用，只有这里需要往 i18n 注册站点消息。
 */
import type { App } from 'vue'
import { defineSiteMessage } from '@/i18n'
import { balanceCurrencySymbol } from '@/utils/balanceCurrency'

/**
 * 接线：模板全局属性 `$currency`（getter，渲染时读 store，设置更新后界面随之刷新）与文案里的
 * 站点消息 currencySymbol。在 main.ts 里 pinia 装好之后、加载语言包之前调用。
 */
export function installBalanceCurrency(app: App): void {
  Object.defineProperty(app.config.globalProperties, '$currency', {
    get: balanceCurrencySymbol,
    enumerable: true,
    configurable: true
  })
  defineSiteMessage('currencySymbol', balanceCurrencySymbol)
}
