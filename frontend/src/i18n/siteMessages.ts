/**
 * 站点消息：值由站点配置决定、与语言无关的文案片段。
 *
 * 各语言文案用链接消息引用它们（`@:{'site.<name>'}`，vue-i18n 链接消息的字面量键写法：后面紧跟
 * 全角括号、数字、斜杠都不会被吞进键名）。值由 i18n/index.ts 的 defineSiteMessage 以消息函数的形式
 * 并入每个语言包，渲染时现取，站点配置更新后界面随之刷新。
 *
 * 本文件不依赖任何模块，语言包可以放心引用。
 */
export const SITE_MESSAGE_NAMESPACE = 'site'

export type SiteMessageName = 'currencySymbol'

export function siteMessageRef(name: SiteMessageName): string {
  return `@:{'${SITE_MESSAGE_NAMESPACE}.${name}'}`
}

/**
 * 站内余额单位的符号（如 $、¥，由系统设置 balance_currency 决定，见 utils/balanceCurrency.ts）。
 * 文案里写站内金额的单位一律用它，不要写死 $ / USD / 美元。
 */
export const CURRENCY = siteMessageRef('currencySymbol')
