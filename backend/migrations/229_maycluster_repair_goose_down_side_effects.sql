-- 229_maycluster_repair_goose_down_side_effects
--
-- 修复旧运行器执行 goose Down 段留下的后果（fork issue #1）。
--
-- 机制：019 / 024 / 037 三个文件带有 `-- +goose Up` / `-- +goose Down` 注解，而 v0.1.179 及
-- 之前的运行器（internal/repository/migrations_runner.go）不识别注解，把整份文件一次交给
-- PostgreSQL。注解只是 SQL 注释，于是每个文件的 Down 段紧跟着 Up 段在同一个事务里执行，
-- 把 Up 刚做的事原地撤销：
--   * 037：ops_alert_silences 建完即删。表从未存在过，告警静默接口在所有环境都返回 500。
--   * 024：刚写入的 Gemini tier_id 被删掉。Down 段的条件是 `credentials ? 'tier_id'`，所以
--     迁移之前就存在的 tier_id 也一并被清掉，那部分无法恢复，只能补回 Up 段的 LEGACY 默认值。
--   * 019：users.wechat 列被删后又加回来并从属性值回填，属性值随后被删除，wechat 属性定义被软删。
--     净效果是数据留在 users.wechat 列里，而代码（ent/schema/user.go 已无 wechat 字段）只读属性表。
--
-- 运行器现在只执行 Up 段（migrations.ExecutableSQL），新库不会再出这个问题；本迁移负责把
-- 已经跑过旧运行器的库补回 Up 段本应留下的状态。全部语句幂等：在未受影响的库上是空操作。

-- ============================================================
-- 037：恢复 ops_alert_silences，与 037 的 Up 段完全一致
-- ============================================================
CREATE TABLE IF NOT EXISTS ops_alert_silences (
    id BIGSERIAL PRIMARY KEY,

    rule_id BIGINT NOT NULL,
    platform VARCHAR(64) NOT NULL,
    group_id BIGINT,
    region VARCHAR(64),

    until TIMESTAMPTZ NOT NULL,
    reason TEXT,

    created_by BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ops_alert_silences_lookup
    ON ops_alert_silences (rule_id, platform, group_id, region, until);

-- ============================================================
-- 024：重新执行 Up 段的 UPDATE（`credentials->>'tier_id' IS NULL` 保证幂等）
-- ============================================================
UPDATE accounts
SET credentials = jsonb_set(
    credentials,
    '{tier_id}',
    '"LEGACY"',
    true
)
WHERE platform = 'gemini'
  AND type = 'oauth'
  AND jsonb_typeof(credentials) = 'object'
  AND credentials->>'tier_id' IS NULL
  AND (
    credentials->>'oauth_type' = 'code_assist'
    OR (credentials->>'oauth_type' IS NULL AND credentials->>'project_id' IS NOT NULL)
  );

-- ============================================================
-- 019：只在 users.wechat 列仍然存在（即旧运行器跑过 Down 段）的库上重做 Up 段。
-- 没有这个列的库（新运行器建的，或者已经修复过的）整段跳过，不碰属性表。
-- PL/pgSQL 在首次执行时才准备各条语句，RETURN 之后引用 users.wechat 的语句不会被解析，
-- 所以列不存在时这里不会报「column does not exist」。
-- ============================================================
DO $$
DECLARE
    wechat_attribute_id BIGINT;
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'users'
          AND column_name = 'wechat'
    ) THEN
        RETURN;
    END IF;

    -- Step 1：属性定义。Down 段把它软删了；key 上的唯一索引是 partial 的（deleted_at IS NULL），
    -- 允许重新插入一条未删除的记录，与 019 Up 段的做法一致。
    INSERT INTO user_attribute_definitions (key, name, description, type, options, required, validation, placeholder, display_order, enabled, created_at, updated_at)
    SELECT 'wechat', '微信', '用户微信号', 'text', '[]'::jsonb, false, '{}'::jsonb, '请输入微信号', 0, true, NOW(), NOW()
    WHERE NOT EXISTS (
        SELECT 1 FROM user_attribute_definitions WHERE key = 'wechat' AND deleted_at IS NULL
    );

    SELECT id
      INTO wechat_attribute_id
      FROM user_attribute_definitions
     WHERE key = 'wechat' AND deleted_at IS NULL
     ORDER BY id
     LIMIT 1;

    -- Step 2：把列里的非空值迁回属性值表（Down 段回填到列里的值就在这里）。
    INSERT INTO user_attribute_values (user_id, attribute_id, value, created_at, updated_at)
    SELECT u.id, wechat_attribute_id, u.wechat, NOW(), NOW()
      FROM users u
     WHERE u.wechat IS NOT NULL
       AND u.wechat <> ''
       AND u.deleted_at IS NULL
       AND NOT EXISTS (
           SELECT 1 FROM user_attribute_values uav
            WHERE uav.user_id = u.id AND uav.attribute_id = wechat_attribute_id
       );

    -- Step 3：与 019 一致，让 wechat 排在最前并从 0 起重新编号。
    UPDATE user_attribute_definitions
       SET display_order = -1
     WHERE id = wechat_attribute_id;

    WITH ordered AS (
        SELECT id, ROW_NUMBER() OVER (ORDER BY display_order, id) - 1 AS new_order
          FROM user_attribute_definitions
         WHERE deleted_at IS NULL
    )
    UPDATE user_attribute_definitions
       SET display_order = ordered.new_order
      FROM ordered
     WHERE user_attribute_definitions.id = ordered.id;

    -- Step 4：删除冗余列。ent schema 早已没有 wechat 字段，代码看不见这个列，删掉是安全的。
    ALTER TABLE users DROP COLUMN IF EXISTS wechat;
END $$;
