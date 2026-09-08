//go:build integration

package repository

// migration 230 回归：groups 触发器改为行差异规则——除 updated_at 外任何列的变化都要
// 为该分组的密钥入队认证快照失效。本测试不点名任何列：遍历 information_schema.columns
// 里 groups 的全部列（排除 id / updated_at），逐列做一次真实 UPDATE 并断言 outbox 有行，
// 所以新加的列当天就被覆盖；漏在白名单里的旧故障形状（model_pricing 没入单）不可能再出现。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"github.com/Wei-Shaw/sub2api/migrations"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type groupsColumnProbe struct {
	name        string
	udtName     string
	dataType    string
	fkReference string // 外键引用的表名；空表示非外键列
}

// changedValueSQL 为一列生成"一定与当前值不同"的赋值表达式；类型未知时返回 ok=false，
// 让测试显式失败，提醒扩展探针而不是静默跳过。
func (p groupsColumnProbe) changedValueSQL(ownID int64) (string, bool) {
	if p.fkReference != "" {
		// 外键列只能指向已存在的行：在 NULL 与自身 id 之间切换（groups 自引用）。
		if p.fkReference != "groups" {
			return "", false
		}
		return fmt.Sprintf("CASE WHEN %s IS NULL THEN %d ELSE NULL END", p.name, ownID), true
	}
	switch p.udtName {
	case "bool":
		return fmt.Sprintf("NOT COALESCE(%s, false)", p.name), true
	case "int2", "int4", "int8", "numeric", "float4", "float8":
		return fmt.Sprintf("COALESCE(%s, 0) + 1", p.name), true
	case "varchar", "text", "bpchar":
		// 5 个十六进制字符满足最短的 varchar(5) 列，且与任何既有值相等的概率可忽略。
		return "left(md5(random()::text), 5)", true
	case "timestamptz", "timestamp":
		return fmt.Sprintf("COALESCE(%s, NOW()) + interval '1 second'", p.name), true
	case "jsonb":
		return fmt.Sprintf(`CASE WHEN %s IS DISTINCT FROM '{"probe":1}'::jsonb THEN '{"probe":1}'::jsonb ELSE '{"probe":2}'::jsonb END`, p.name), true
	case "json":
		return fmt.Sprintf(`CASE WHEN %s::text IS DISTINCT FROM '{"probe":1}' THEN '{"probe":1}'::json ELSE '{"probe":2}'::json END`, p.name), true
	}
	return "", false
}

func loadGroupsColumnProbes(t *testing.T, ctx context.Context, db *sql.DB) []groupsColumnProbe {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT c.column_name, c.udt_name, c.data_type,
		       COALESCE((
		           SELECT ccu.table_name
		           FROM information_schema.table_constraints tc
		           JOIN information_schema.key_column_usage kcu
		             ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema
		           JOIN information_schema.constraint_column_usage ccu
		             ON ccu.constraint_name = tc.constraint_name AND ccu.table_schema = tc.table_schema
		           WHERE tc.table_schema = c.table_schema AND tc.table_name = c.table_name
		             AND tc.constraint_type = 'FOREIGN KEY' AND kcu.column_name = c.column_name
		           LIMIT 1
		       ), '') AS fk_reference
		FROM information_schema.columns c
		WHERE c.table_schema = current_schema() AND c.table_name = 'groups'
		ORDER BY c.ordinal_position`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var probes []groupsColumnProbe
	for rows.Next() {
		var p groupsColumnProbe
		require.NoError(t, rows.Scan(&p.name, &p.udtName, &p.dataType, &p.fkReference))
		probes = append(probes, p)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, probes)
	return probes
}

func TestAuthCacheInvalidationTrigger_EveryGroupColumnExceptBookkeeping(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("row-diff-trigger-group-%d", suffix), Platform: service.PlatformOpenAI, RateMultiplier: 1,
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("row-diff-trigger-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-row-diff-trigger-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &groupID, Key: keyValue, Name: "row-diff-trigger", Status: service.StatusActive}
	require.NoError(t, apiKeyRepo.Create(ctx, key))

	sum := sha256.Sum256([]byte(keyValue))
	cacheKey := hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = $1", cacheKey)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})
	_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = $1", cacheKey)
	require.NoError(t, err)

	// 每个探针都在独立事务里执行并回滚：触发器插入的 outbox 行在事务内可见、回滚后消失，
	// 列值也回到初始状态，探针之间互不影响。
	outboxRowsAfter := func(t *testing.T, statement string) int {
		t.Helper()
		tx, err := integrationDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, statement, group.ID)
		require.NoError(t, err, statement)
		var count int
		require.NoError(t, tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM auth_cache_invalidation_outbox WHERE cache_key = $1", cacheKey).Scan(&count))
		return count
	}

	// 排除列表从迁移 SQL 里解析出来，不在这里重抄一份：两边各写一份必然分叉，
	// 而分叉的方向是"以为排除了其实没排除"（多余回源，能忍）或"以为没排除其实排除了"
	// （计费配置静默陈旧，不能忍）。id 是主键，不参与更新。
	excluded := parseRowDiffExcludedColumns(t)
	require.Contains(t, excluded, "updated_at", "记账列必须在排除列表里")
	bookkeeping := map[string]bool{"id": true}
	for _, name := range excluded {
		bookkeeping[name] = true
	}
	walked := 0
	for _, probe := range loadGroupsColumnProbes(t, ctx, integrationDB) {
		if bookkeeping[probe.name] {
			continue
		}
		probe := probe
		t.Run(probe.name, func(t *testing.T) {
			expr, ok := probe.changedValueSQL(group.ID)
			require.True(t, ok, "column %s (%s/%s, fk=%q) has no probe; extend changedValueSQL", probe.name, probe.dataType, probe.udtName, probe.fkReference)
			count := outboxRowsAfter(t, fmt.Sprintf("UPDATE groups SET %s = %s WHERE id = $1", probe.name, expr))
			require.Equal(t, 1, count, "changing groups.%s must enqueue exactly one invalidation for the group's key", probe.name)
		})
		walked++
	}
	require.Greater(t, walked, 20, "column walk must cover the real groups table, not a handful of columns")

	// 排除列逐个验证真的不入队：光在 SQL 里写下 - 'col' 不等于它生效
	//（列名写错、迁移没跑到，都会静默变成"照常入队"）。
	for _, name := range excluded {
		name := name
		t.Run("excluded/"+name, func(t *testing.T) {
			var probe *groupsColumnProbe
			for _, p := range loadGroupsColumnProbes(t, ctx, integrationDB) {
				if p.name == name {
					p := p
					probe = &p
					break
				}
			}
			require.NotNilf(t, probe, "排除列表里的 %s 在 groups 表里不存在：列名写错了", name)
			expr, ok := probe.changedValueSQL(group.ID)
			require.True(t, ok, "column %s has no probe; extend changedValueSQL", name)
			require.Zerof(t, outboxRowsAfter(t, fmt.Sprintf("UPDATE groups SET %s = %s WHERE id = $1", name, expr)),
				"groups.%s 在排除列表里，改它不应该产生失效", name)
		})
	}

	t.Run("sort order drag does not invalidate every key", func(t *testing.T) {
		// 管理页拖动排序（UpdateSortOrders）是纯外观操作：10 个分组 × 5000 把密钥
		// 曾经等于 5 万条 outbox 行和 5 万次认证回源。
		require.Zero(t, outboxRowsAfter(t, "UPDATE groups SET sort_order = sort_order + 1 WHERE id = $1"))
	})
	t.Run("no-op update does not enqueue", func(t *testing.T) {
		require.Zero(t, outboxRowsAfter(t, "UPDATE groups SET rate_multiplier = rate_multiplier, name = name WHERE id = $1"))
	})
	t.Run("model_rate_multipliers change enqueues", func(t *testing.T) {
		require.Equal(t, 1, outboxRowsAfter(t, `UPDATE groups SET model_rate_multipliers = '[{"model_pattern":"claude-opus-*","multiplier":2}]'::jsonb WHERE id = $1`))
	})
	t.Run("delete enqueues", func(t *testing.T) {
		require.Equal(t, 1, outboxRowsAfter(t, "DELETE FROM groups WHERE id = $1"))
	})
}

// rowDiffExcludedColumn 匹配触发器里 to_jsonb(OLD) - 'col' 形式的排除项。
var rowDiffExcludedColumn = regexp.MustCompile(`- '([a-z_]+)'`)

// parseRowDiffExcludedColumns 从内嵌迁移里读出行差异触发器排除了哪些列。
//
// 读 SQL 而不是在测试里抄一份：排除列表是"这一列不进 APIKeyAuthGroupSnapshot"的
// 判断结果，只应该有一处。抄一份的话，改了 SQL 忘了改测试，测试还会继续为一个
// 早已不存在的规则背书。
func parseRowDiffExcludedColumns(t *testing.T) []string {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	var latest string
	for _, entry := range entries {
		// 目录按文件名字典序遍历，最后一个匹配的就是最新一版触发器定义。
		if strings.Contains(entry.Name(), "group_auth_cache_row_diff") {
			latest = entry.Name()
		}
	}
	require.NotEmpty(t, latest, "找不到行差异触发器的迁移文件，护栏和迁移目录脱节了")
	body, err := migrations.FS.ReadFile(latest)
	require.NoError(t, err)

	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, m := range rowDiffExcludedColumn.FindAllStringSubmatch(string(body), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	require.NotEmpty(t, out, "%s 里没解析出任何排除列，正则和 SQL 写法脱节了", latest)
	return out
}
