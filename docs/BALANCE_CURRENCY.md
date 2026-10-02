# 站内记账币种（balance_currency）与汇率折算

余额、用量费用、各类限额（API Key 额度与速率限制、订阅分组限额、账号额度、平台额度）、兑换码与返利金额都以**同一个记账币种**计。系统设置 → 功能 → 「站内记账币种」决定它，只能选有汇率来源的法币（当前 `USD`、`CNY`，默认 `USD`；选项来自 `GET /api/v1/admin/exchange-rates/currencies`）。

以其它币种支付或标价的金额，在交易发生时按当时的汇率折算成记账币种，所用汇率连同来源一起记在该笔记录上（`currency_conversion`），事后可复现：

| 场景 | 折算 | 记在哪里 |
| --- | --- | --- |
| 模型计费（官方价目录以美元标价；定价条目可写 `currency`，如方舟 Seedance 按人民币） | 价卡币种 → 记账币种，使用时刻的汇率；倍率在折算之后乘 | `usage_logs.currency_conversion` |
| 批量生图 | 提交时把单价折算进价格快照 | `batch_image_jobs.currency_conversion`，结算写用量日志时带上 |
| 管理员调整余额（可选 USD / CNY / USDT / USDC） | 原币 → 记账币种，取到分 | 调整流水 `redeem_codes.currency_conversion` |
| 在线充值 | 支付金额（支付币种）→ 记账币种并取到分，再乘充值倍率 | `payment_orders.currency_conversion`，入账时随充值码进余额流水 |
| 订阅套餐 | 套餐价（套餐币种，未填即 `USD`）→ 支付币种，取到该币种最小单位 | `payment_orders.currency_conversion` |

汇率口径：

- **CNY**：中国外汇交易中心公布的人民币对美元中间价（工作日北京时间 9:15 公布），取交易时刻之前最近一次公布的一条；节假日不公布，沿用上一次公布的中间价。
- **USDT / USDC**：CoinGecko 当日（UTC 日期）00:00 的美元价格。
- 每条抓到的汇率原样存进 `exchange_rates`；没有后台任务，用到时按需刷新：平时每小时最多一次，工作日跨过 9:15 立即重取，9:15 之后两小时内存档还没有当天中间价时每 5 分钟重试。
- 资金入账（调整余额、在线充值、订阅下单）要求汇率确认是当前有效的，确认不了就拒绝；计费不因汇率源暂时不可用而丢单，沿用最近一次存档的中间价并在折算记录上标 `stale`。
- 没写币种的价格一律按 `USD`；非 `USD` 的定价条目不继承美元目录价，token 计费须写明输入价和输出价，缓存价不写按输入价计。

**改记账币种不会换算已有数值**：余额、限额、按记账币种填的配置数字原样保留、换一种写法。切换前要先把已有余额等数值按切换当天的汇率重述（迁移 241 会把当时没写币种的定价条目回填成当时的记账币种，它们的含义不变）。

| 用到记账币种的地方 | 写法 |
| --- | --- |
| 界面上的金额 | 由币种推导的符号（CLDR 窄符号：USD → `$`，CNY → `¥`），经公开设置 `balance_currency_symbol` 下发 |
| 邮件 | 模板变量 `{{currency_symbol}}`（所有事件可用；官方模板里的金额都带着它） |
| `/v1/usage` 的 `unit` | 币种代码 |
| 批量生图任务的 `currency` 快照 | 提交时的币种代码 |

## 开发约定

- 规则只在一处：后端汇率与折算在 `internal/service/exchange_rate.go`（`ExchangeRateService.Convert`、`RoundCreditedAmount`、`EncodeCurrencyConversion` / `DecodeCurrencyConversion`），计费的唯一折算点是 `BillingService.convertToAccountingCurrency`，下单金额在 `payment_order.go` 的 `computeCreateOrderAmounts`；记账币种的校验 / 读取 / 写法在 `internal/service/setting_site_display.go`。
- 前端：站内符号 `src/utils/balanceCurrency.ts`（接线在 `balanceCurrencyInstall.ts`），折算记录与真实货币金额的写法 `src/utils/currencyConversion.ts` / `components/common/CurrencyConversionNote.vue`，结算页预览 `src/components/payment/exchange.ts`（与后端下单同一算法），币种选项 `src/composables/useCurrencyOptions.ts`。
- 前端模板写 `{{ $currency }}{{ amount }}`，脚本用 `balanceCurrencySymbol()` / `withBalanceCurrencySymbol()` / `formatCurrency()`，文案用 `src/i18n/siteMessages.ts` 的 `CURRENCY`（vue-i18n 链接消息，渲染时取当前符号）。
- `src/__tests__/balanceCurrencyGuard.spec.ts` 遍历全部源码与文案，拦下写死的 `$` / USD / 美元；确属真实货币的地方登记在其中的豁免表并写明理由。后端 `TestEmailTemplatesHaveNoHardcodedCurrencySymbol` 对邮件模板做同样的检查。
