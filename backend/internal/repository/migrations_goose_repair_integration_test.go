//go:build integration

package repository

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// repairMigration 是专门补救旧运行器执行 goose Down 段后果的迁移；本文件的测试围绕它展开。
const repairMigration = "229_maycluster_repair_goose_down_side_effects.sql"

// openScratchDatabase 在共享的 PostgreSQL 容器里建一个全新的库并返回连接。
// 需要模拟「某个历史状态的库」的测试不能用共享库，共享库已经跑完了全部迁移。
func openScratchDatabase(t *testing.T, name string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	quoted := pq.QuoteIdentifier(name)
	_, err := integrationDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)

	dsn, err := url.Parse(integrationDSN)
	require.NoError(t, err)
	dsn.Path = "/" + name
	db, err := openSQLWithRetry(ctx, dsn.String(), 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = integrationDB.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	return db
}

func sortedEmbeddedMigrationNames(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	sort.Strings(names)
	return names
}

func relationExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var regclass sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(), "SELECT to_regclass($1)", "public."+name).Scan(&regclass))
	return regclass.Valid
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1 FROM information_schema.columns
	WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
)`, table, column).Scan(&exists))
	return exists
}

// TestRepairMigration_RestoresWhatGooseDownSectionsUndid 重现旧运行器留下的库，再用当前运行器升级：
//  1. 应用除修复迁移外的全部迁移（新运行器：只跑 Up 段）；
//  2. 按声明遍历所有带 goose 注解的文件，把它们的 Down 段再执行一遍——这正是旧运行器多做的那部分；
//  3. 写入旧运行器时代会产生的那种数据；
//  4. 应用完整迁移集（此时只有修复迁移是新的）；
//  5. 断言 Up 段本应留下的状态被补回来，且修复迁移可重复执行。
func TestRepairMigration_RestoresWhatGooseDownSectionsUndid(t *testing.T) {
	ctx := context.Background()
	db := openScratchDatabase(t, "sub2api_goose_repair")
	names := sortedEmbeddedMigrationNames(t)
	require.Contains(t, names, repairMigration)

	// 1. 修复发布之前的世界
	beforeRepair := fstest.MapFS{}
	for _, name := range names {
		if name == repairMigration {
			continue
		}
		data, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		beforeRepair[name] = &fstest.MapFile{Data: data}
	}
	require.NoError(t, applyMigrationsFS(ctx, db, beforeRepair))
	require.True(t, relationExists(t, db, "ops_alert_silences"), "新运行器只跑 Up 段，表应当存在")
	require.False(t, columnExists(t, db, "users", "wechat"))

	// 2. 旧运行器的副作用：每个带注解文件的 Down 段都跟在 Up 段后面执行了
	replayed := 0
	for _, name := range names {
		if name == repairMigration {
			continue
		}
		data, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		sections, err := migrations.ParseSections(strings.TrimSpace(string(data)))
		require.NoError(t, err)
		if !sections.HasMarkers || strings.TrimSpace(migrations.StripComments(sections.Down)) == "" {
			continue
		}
		_, err = db.ExecContext(ctx, sections.Down)
		require.NoErrorf(t, err, "%s: replaying the Down section the old runner used to execute", name)
		replayed++
	}
	require.Greater(t, replayed, 0, "the embedded set is known to contain goose-marked files")
	require.False(t, relationExists(t, db, "ops_alert_silences"), "damage must be visible before the repair")
	require.True(t, columnExists(t, db, "users", "wechat"))

	// 3. 旧运行器时代的数据：wechat 留在列里，Gemini 账号没有 tier_id
	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
INSERT INTO users (email, password_hash, role, status, balance, concurrency, wechat)
VALUES ('goose-repair@example.com', 'hash', 'user', 'active', 0, 1, 'wx-repair-fixture')
RETURNING id`).Scan(&userID))
	var blankUserID int64
	require.NoError(t, db.QueryRowContext(ctx, `
INSERT INTO users (email, password_hash, role, status, balance, concurrency, wechat)
VALUES ('goose-repair-blank@example.com', 'hash', 'user', 'active', 0, 1, '')
RETURNING id`).Scan(&blankUserID))
	var geminiID, apiKeyID int64
	require.NoError(t, db.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, credentials)
VALUES ('goose-repair-gemini', 'gemini', 'oauth', '{"oauth_type":"code_assist"}'::jsonb)
RETURNING id`).Scan(&geminiID))
	require.NoError(t, db.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, credentials)
VALUES ('goose-repair-apikey', 'gemini', 'apikey', '{}'::jsonb)
RETURNING id`).Scan(&apiKeyID))

	// 4. 升级：完整迁移集里只有修复迁移尚未应用
	require.NoError(t, ApplyMigrations(ctx, db))
	var recorded bool
	require.NoError(t, db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)", repairMigration).Scan(&recorded))
	require.True(t, recorded)

	// 5. 037：表和索引回来了
	require.True(t, relationExists(t, db, "ops_alert_silences"))
	var indexExists bool
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'ops_alert_silences' AND indexname = 'idx_ops_alert_silences_lookup')`).Scan(&indexExists))
	require.True(t, indexExists)

	// 024：Up 段的 LEGACY 默认值补回来了，非 oauth 账号不受影响
	var tier sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, "SELECT credentials->>'tier_id' FROM accounts WHERE id = $1", geminiID).Scan(&tier))
	require.Equal(t, "LEGACY", tier.String)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT credentials->>'tier_id' FROM accounts WHERE id = $1", apiKeyID).Scan(&tier))
	require.False(t, tier.Valid)

	// 019：列没了，定义回来了，值迁到属性表里
	require.False(t, columnExists(t, db, "users", "wechat"))
	var attributeID int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT id FROM user_attribute_definitions WHERE key = 'wechat' AND deleted_at IS NULL").Scan(&attributeID))
	var value string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM user_attribute_values WHERE user_id = $1 AND attribute_id = $2", userID, attributeID).Scan(&value))
	require.Equal(t, "wx-repair-fixture", value)
	var blankRows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_attribute_values WHERE user_id = $1", blankUserID).Scan(&blankRows))
	require.Zero(t, blankRows, "empty wechat values are not migrated, matching the original Up section")

	// 6. 幂等：再跑一遍修复迁移的可执行部分，结果不变
	data, err := migrations.FS.ReadFile(repairMigration)
	require.NoError(t, err)
	execSQL, err := migrations.ExecutableSQL(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, execSQL)
	require.NoError(t, err)
	var valueRows, definitionRows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_attribute_values WHERE attribute_id = $1", attributeID).Scan(&valueRows))
	require.Equal(t, 1, valueRows)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_attribute_definitions WHERE key = 'wechat' AND deleted_at IS NULL").Scan(&definitionRows))
	require.Equal(t, 1, definitionRows)
}
