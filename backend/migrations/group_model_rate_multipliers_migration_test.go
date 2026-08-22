package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 229：两列都必须幂等新增；usage_logs 列必须保持 nullable、无 DEFAULT、无回填——
// 生产表百万级行，带 DEFAULT/UPDATE 的改列会重写整表。
func TestGroupModelRateMultipliersMigration(t *testing.T) {
	content, err := FS.ReadFile("229_maycluster_group_model_rate_multipliers.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(stripSQLLineComments(string(content))), " ")

	require.Contains(t, sql, "ALTER TABLE groups ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB;")
	require.Contains(t, sql, "ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS model_rate_multiplier NUMERIC(12,6);")

	upper := strings.ToUpper(sql)
	require.NotContains(t, upper, "DEFAULT", "新增列不得带 DEFAULT（usage_logs 需保持仅改元数据）")
	require.NotContains(t, upper, "UPDATE USAGE_LOGS", "不得回填 usage_logs")
	require.NotContains(t, upper, "NOT NULL", "快照列必须可空：NULL 表示未评估，读侧按 1 处理")
}

// stripSQLLineComments 去掉 "--" 行注释，让断言只针对可执行语句（注释里解释"无 DEFAULT"不算违规）。
func stripSQLLineComments(sql string) string {
	lines := strings.Split(sql, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// 230：groups 触发器改为行差异规则，不再维护列白名单。
func TestGroupAuthCacheRowDiffTriggerMigration(t *testing.T) {
	content, err := FS.ReadFile("230_maycluster_group_auth_cache_row_diff_trigger.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()")
	require.Contains(t, sql, "(to_jsonb(OLD) - 'updated_at') IS NOT DISTINCT FROM (to_jsonb(NEW) - 'updated_at')",
		"规则只有一句：除 updated_at 外任何列变化都失效快照")
	require.NotContains(t, sql, "OLD.status IS NOT DISTINCT FROM NEW.status", "不得回退到列白名单")
	require.NotContains(t, sql, "OLD.rate_multiplier IS NOT DISTINCT FROM", "不得回退到列白名单")
	require.Contains(t, sql, "INSERT INTO auth_cache_invalidation_outbox (cache_key)")
	require.Contains(t, sql, "IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;")
}
