# 分组逐模型倍率（Per-model rate multipliers）

同一分组内对不同模型收不同倍率，例如 opus 按 2×、haiku 按 1×。对应 issue #9。

## 计费公式

token 计费的有效倍率只有一个组合公式（`internal/service/group_model_rate_multiplier.go` 的 `effectiveDownstreamMultiplier`）：

```
effective = (用户专属倍率 ?? 分组 rate_multiplier) × 高峰因子(pricingAt) × 逐模型因子(model)
actual_cost = total_cost × effective
```

- 逐模型因子只**乘入**，永远不替换基础倍率；用户覆盖与分组默认之间仍是"覆盖"关系，高峰因子仍是"乘"。
- 只作用于 `billing_mode = token`。图片/视频/按次/音频/网页搜索走各自既有的独立倍率，不受本表影响，对应行的 `model_rate_multiplier` 为 NULL。
- 价格仍跟随价格表同步：本功能改的是倍率，不是绝对价。`groups.model_pricing`（绝对价覆盖）保持原样，两者独立。

## 存储

| 位置 | 列 | 语义 |
|---|---|---|
| `groups` | `model_rate_multipliers JSONB` | 有序列表 `[{"model_pattern":"claude-opus-*","multiplier":2.0}, ...]` |
| `usage_logs` | `model_rate_multiplier NUMERIC(12,6)` | 本次请求实际乘入的因子快照；NULL = 未评估（按 1） |

迁移：`backend/migrations/229_maycluster_group_model_rate_multipliers.sql`。`usage_logs` 的新列无 DEFAULT、无回填，PostgreSQL 只改元数据。

`usage_logs.rate_multiplier` 保持既有含义（按 `billing_mode` 分别是 token/图片/视频倍率），不再叠加新含义；审计关系为
`actual_cost = total_cost × rate_multiplier × COALESCE(model_rate_multiplier, 1)`。

## 匹配规则

- 精确模型名或末尾 `*` 通配；大小写不敏感，claude 系列的 `.` 与 `-` 等价（与分组逐模型定价共用同一条 `matchModelPatternNormalized`）。
- **按列表顺序取第一条命中的规则**。更具体的模式应放在通配规则之前。
- 校验（创建/更新共用 `NormalizeGroupModelRateMultipliers`）：模式非空；`0 < multiplier ≤ 100`；归一化后不允许重复模式。
- 存量非法倍率（直改 SQL 绕过校验）按 1 处理并记录 `group_model_rate_multiplier_invalid_degraded_to_one` 告警（每个分组×模式每进程一条）。

## 生效位置（单一解析点）

`BillingService.CalculateCostUnified` 是带分组的 token 计费唯一入口；两条网关（Anthropic/Gemini/Antigravity 的 `gateway_usage_billing.go` 与 OpenAI/Grok 的 `openai_gateway_usage.go`）的所有 token 计费分支——含长上下文阈值拆分与无 Resolver 的内置定价兜底——都经过它。逐模型因子在 `applyGroupModelRateMultiplier` 这一处乘入，`CostBreakdown` 回填 `RateMultiplier` 与 `ModelRateMultiplier`，不变式 `ActualCost = TotalCost × RateMultiplier × ModelRateMultiplier` 由单测钉死。

认证快照 `APIKeyAuthGroupSnapshot` 携带 `model_rate_multipliers`（快照版本 v21），`TestAuthGroupSnapshotRoundTripCoversEveryField` 用反射遍历快照的每个字段做往返校验，以后新增快照字段漏映射会直接失败。

## 利润控制

准入阈值 `D × (1 − margin − buffer)` 中的 D 现在按请求模型计算（`newProfitControlGate` → `effectiveDownstreamMultiplier`），gateway 与 openai 两条装门路径共用同一构造函数；同分组不同模型的门不复用。WebSocket 长连接每个 turn 按该 turn 的模型重装门。

## 缓存失效

`groups` 触发器（`230_maycluster_group_auth_cache_row_diff_trigger.sql`）改为行差异规则：除 `updated_at` 外任何列变化都失效该分组密钥的认证快照。集成测试 `TestAuthCacheInvalidationTrigger_EveryGroupColumnExceptBookkeeping` 遍历 `information_schema.columns` 逐列验证，新列无需再手工加白名单。

## API

- 管理接口：`POST/PUT /api/v1/admin/groups` 接受 `model_rate_multipliers`（更新时省略 = 不修改，空数组 = 清空）；分组 DTO 返回该列表。
- `GET /v1/sub2api/billing[?model=<model>]`（schema_version 2）：总是返回 `model_rate_multipliers`；带 `model` 时额外返回 `model`、`model_rate_multiplier`、`matched_model_pattern`，且 `effective_rate_multiplier` 折入逐模型因子；不带 `model` 时语义与 v1 一致。上游倍率探测同时接受 v1/v2。
- 用量列表 DTO 新增 `model_rate_multiplier`；管理端用量表新增"模型倍率"列与费用明细行。

## 管理界面

分组创建/编辑表单的"分组逐模型定价"区块下方新增"分组逐模型倍率"编辑器：模式 + 倍率因子 + 最终倍率预览，逐行校验提示，顺序即优先级。
