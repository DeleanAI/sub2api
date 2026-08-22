-- 分组逐模型倍率（issue #9）。
--
-- groups.model_rate_multipliers：有序 JSONB 列表 [{"model_pattern": "claude-opus-*", "multiplier": 2.0}, ...]，
-- token 计费时第一条命中请求模型的倍率乘入有效倍率：
--   effective = (用户覆盖 ?? 分组默认 rate_multiplier) × 高峰因子 × 逐模型因子
-- 与 model_pricing（绝对价覆盖）刻意分列：倍率条目绝不能进入定价解析链。
-- 无默认值：NULL 与 'null'::jsonb 在应用层都按"未配置"（因子 1）处理。
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB;

COMMENT ON COLUMN groups.model_rate_multipliers IS
    'Ordered per-model rate multiplier rules [{model_pattern, multiplier}]; first match multiplies the token billing multiplier';

-- usage_logs.model_rate_multiplier：本次请求实际乘入的逐模型因子快照。
-- NULL 表示该行未评估逐模型倍率（按次/图片/视频或历史行），读侧按 1 处理：
--   actual_cost = total_cost × rate_multiplier × COALESCE(model_rate_multiplier, 1)
-- rate_multiplier 保持既有含义（按 billing_mode 分别承载 token/图片/视频倍率），不再叠加新含义。
-- 生产 usage_logs 为百万级行：nullable、无 DEFAULT、无回填，PostgreSQL 仅改元数据，不重写表。
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS model_rate_multiplier NUMERIC(12,6);

COMMENT ON COLUMN usage_logs.model_rate_multiplier IS
    'Per-model group rate multiplier applied to this request (NULL = not evaluated, treat as 1)';
