-- 合并上游 v0.2.5 后补回 Qwen：上游的 237_add_minimax_platform.sql 与
-- 238_opencode_go_platform.sql 重写了这两个 CHECK 以加入 MiniMax / OpenCode，但上游
-- 不知道 fork 加过 Qwen（229_add_qwen_token_plan.sql），于是按迁移顺序把 'qwen' 从
-- 白名单里静默抹掉了。
--
-- 后果是静默的：生产当前 user_platform_quotas 的 16 行里没有 qwen、composite_model_routes
-- 是空表，所以上游迁移本身不会失败；但后端 AllowedQuotaPlatforms 与
-- isConcreteRequestPlatform 都含 PlatformQwen，声明与约束已经分叉——等到谁给 Qwen 配
-- 用户配额或加一条 composite 路由，才会撞上 CHECK 违反。
--
-- 只补这两张表。channel_monitors / channel_monitor_request_templates 的 provider
-- 白名单不加 qwen 是对的：Qwen 没有公开用量/余额端点（Token Plan 靠推理 429 停调），
-- 见 internal/service/channel_monitor_validate.go 的说明。
--
-- 新名单 = 上游的 10 个 + qwen，与后端两处声明一致。DROP ... IF EXISTS 保证可重入。
-- 本仓库的迁移不写 Down（297 个里只有 5 个有），与既有惯例保持一致。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'qwen', 'minimax', 'opencode_go'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'qwen', 'minimax', 'opencode_go'));
