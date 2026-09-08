-- 收窄 230 的行差异触发器：排除不进认证快照的纯外观/记账列。
--
-- 230 只排除 updated_at，于是任何 groups 行变化都会给该分组的每一把在用密钥写一条
-- outbox 行。管理页拖动分组排序（groups.sort_order，UpdateSortOrders 一次批量更新）
-- 是纯外观操作：10 个分组 × 5000 把密钥 = 5 万条 outbox 行加 5 万次认证缓存回源，
-- 旧的列白名单时代这是 0。
--
-- 排除的判据只有一条：这一列不进 APIKeyAuthGroupSnapshot（认证快照读不到它，
-- 因此它变了也不可能让快照过期）。判据由集成测试 逐列 校验，两边不许各写一份：
-- 测试从本文件里解析出排除列表，再逐列断言"排除的不入队、其余的入队"。
--
-- 仍然保持 230 的方向：默认全排除之外的列都失效。新增一个参与计费/调度的列不需要
-- 改这里，只有新增"确实不进快照"的列时才需要，而漏改的后果是多一次回源，不是错价。
CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND (to_jsonb(OLD) - 'updated_at' - 'sort_order' - 'description' - 'duplicate_operation_id')
           IS NOT DISTINCT FROM
           (to_jsonb(NEW) - 'updated_at' - 'sort_order' - 'description' - 'duplicate_operation_id') THEN
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
