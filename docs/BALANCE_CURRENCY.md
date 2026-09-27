# 站内余额单位（balance_currency）

余额、模型价格、用量费用、各类限额（API Key 额度与速率限制、订阅分组限额、账号额度、平台额度）、兑换码与返利金额用的是**同一个站内单位**。系统设置 → 功能 → 「站内余额单位」声明这个单位按哪种货币计（ISO 4217 代码，默认 `USD`）。

它只决定这个单位**怎么写**，不做任何换算：把 `USD` 改成 `CNY` 之后，所有数值保持不变，只是写法从 `$12.00` 变成 `¥12.00`。按「1 单位 = 1 元」运营的站点应设为 `CNY`，并按人民币填写价格与额度。

| 用到的地方 | 写法 |
| --- | --- |
| 界面上的金额 | 由币种推导的符号（CLDR 窄符号：USD → `$`，CNY → `¥`，EUR → `€`），经公开设置 `balance_currency_symbol` 下发 |
| 邮件 | 模板变量 `{{currency_symbol}}`（所有事件可用；官方模板里的金额都带着它） |
| `/v1/usage` 的 `unit` | 币种代码（默认站点仍是 `USD`，客户端看到的值不变） |
| 批量生图任务的 `currency` 快照 | 提交时的币种代码 |

支付单的币种（支付通道收的 CNY / USD / HKD …）、上游账号的账单（如 xAI 预付余额）是真实货币，与此设置无关。

## 开发约定

- 规则只在一处：后端 `internal/service/setting_site_display.go`（校验 `NormalizeBalanceCurrency`、读取 `ReadBalanceCurrency`、写法 `BalanceCurrency.Format`），前端 `src/utils/balanceCurrency.ts`。
- 前端模板写 `{{ $currency }}{{ amount }}`，脚本用 `balanceCurrencySymbol()` / `withBalanceCurrencySymbol()` / `formatCurrency()`，文案用 `src/i18n/siteMessages.ts` 的 `CURRENCY`（vue-i18n 链接消息，渲染时取当前符号）。
- `src/__tests__/balanceCurrencyGuard.spec.ts` 遍历全部源码与文案，拦下写死的 `$` / USD / 美元；确属真实货币的地方登记在其中的豁免表并写明理由。后端 `TestEmailTemplatesHaveNoHardcodedCurrencySymbol` 对邮件模板做同样的检查。
