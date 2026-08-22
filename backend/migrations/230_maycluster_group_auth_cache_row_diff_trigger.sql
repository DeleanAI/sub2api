-- groups 认证快照失效触发器改为"行差异"规则（替换 193 的手工列白名单）。
--
-- 规则只有一句：分组行除记账列（updated_at）外的任何变化，都要失效该分组全部密钥的认证快照。
--
-- 193 的白名单需要每加一个参与计费/调度的列就补一行（model_pricing 就漏了，
-- model_rate_multipliers 又是一个），漏掉的列改动不产生 outbox 行，丢掉 30s 兜底后
-- 回填/失效竞态能把陈旧值在 Redis L2 留满 300s TTL。行差异比较用 to_jsonb(OLD/NEW)
-- 去掉 updated_at 后 IS DISTINCT FROM，新列自动纳入；代价是 name/description 等
-- 外观列的修改也会失效一次快照——分组行极少更新，一次额外的认证回源远比
-- 计费配置静默陈旧便宜。集成测试逐列遍历 information_schema.columns 钉死本规则。
CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND (to_jsonb(OLD) - 'updated_at') IS NOT DISTINCT FROM (to_jsonb(NEW) - 'updated_at') THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT encode(sha256(convert_to(k.key, 'UTF8')), 'hex')
    FROM api_keys AS k
    WHERE k.group_id = target_group_id
      AND k.deleted_at IS NULL
      AND k.key <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
