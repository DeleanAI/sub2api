//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestPlanMigrations_AgainstMigratedDatabase 用真实库校验计划与运行器的一致性。
func TestPlanMigrations_AgainstMigratedDatabase(t *testing.T) {
	ctx := context.Background()
	names := sortedEmbeddedMigrationNames(t)

	plan, err := PlanMigrations(ctx, integrationDB, migrations.FS)
	require.NoError(t, err)
	require.Empty(t, plan.Pending, "the harness applied every embedded migration")
	require.Empty(t, plan.ChecksumMismatches)
	require.Empty(t, plan.UnknownApplied)
	require.Equal(t, len(names), plan.AppliedCount)
	require.Equal(t, names[len(names)-1], plan.LastApplied)
	require.Equal(t, migrations.Kind(""), plan.PendingKind())

	// 把最后两条记录删掉（事务内，测试结束回滚）：它们必须重新出现在 pending 里，判定与分类器一致。
	tx := testTx(t)
	forgotten := names[len(names)-2:]
	_, err = tx.ExecContext(ctx, "DELETE FROM schema_migrations WHERE filename = ANY($1)", pq.Array(forgotten))
	require.NoError(t, err)

	plan, err = PlanMigrations(ctx, tx, migrations.FS)
	require.NoError(t, err)
	require.Equal(t, len(names)-2, plan.AppliedCount)
	require.Equal(t, names[len(names)-3], plan.LastApplied)
	require.Len(t, plan.Pending, 2)
	for i, pending := range plan.Pending {
		require.Equal(t, forgotten[i], pending.Filename)
		data, err := migrations.FS.ReadFile(pending.Filename)
		require.NoError(t, err)
		kind, reasons, err := migrations.Classify(strings.TrimSpace(string(data)))
		require.NoError(t, err)
		require.Equal(t, kind, pending.Kind)
		require.Equal(t, reasons, pending.Reasons)
	}
	require.NotEqual(t, migrations.Kind(""), plan.PendingKind())
}

// TestRepairMigration_IsFlaggedAsDestructiveForOldDatabases 把「第一次升级到修复版本必须停机」
// 这条运维约束钉在测试里：修复迁移会有条件地 DROP COLUMN users.wechat，分类器只看文本，
// 无法证明旧副本不引用该列，所以必须判为 destructive；部署文档据此要求首轮升级缩到零副本。
func TestRepairMigration_IsFlaggedAsDestructiveForOldDatabases(t *testing.T) {
	data, err := migrations.FS.ReadFile(repairMigration)
	require.NoError(t, err)
	kind, reasons, err := migrations.Classify(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.Equal(t, migrations.KindDestructive, kind, fmt.Sprintf("reasons: %v", reasons))
	require.Contains(t, strings.Join(reasons, "\n"), "DROP COLUMN users.wechat")
}
