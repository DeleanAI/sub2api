//go:build unit

package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 真实文件：issue #5 里人工核对过的样本，分类器必须给出同样的结论。
func TestClassify_RealMigrations(t *testing.T) {
	cases := map[string]Kind{
		"090_drop_sora.sql":                             KindDestructive,
		"136_remove_ops_retry_replay.sql":               KindDestructive,
		"127_drop_channel_monitor_deleted_at.sql":       KindDestructive,
		"037_ops_alert_silences.sql":                    KindAdditive,
		"222_group_usage_daily_rollups.sql":             KindAdditive,
		"224_user_platform_quotas_add_cn_providers.sql": KindAdditive,
		"227_composite_routes_add_cn_providers.sql":     KindAdditive,
		"225_backfill_codex_fingerprint_seed.sql":       KindDataRewrite,
		"024_add_gemini_tier_id.sql":                    KindDataRewrite,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			kind, reasons, err := Classify(readEmbeddedMigration(t, name))
			require.NoError(t, err)
			require.Equalf(t, want, kind, "reasons: %v", reasons)
			if want == KindAdditive {
				require.Empty(t, reasons, "additive 文件不应带理由")
			} else {
				require.NotEmpty(t, reasons)
			}
		})
	}
}

func TestClassify_037DownSectionMustNotInfluenceVerdict(t *testing.T) {
	content := readEmbeddedMigration(t, "037_ops_alert_silences.sql")
	require.Contains(t, content, "DROP TABLE IF EXISTS ops_alert_silences", "样本前提：Down 段里确实有 DROP TABLE")
	kind, reasons, err := Classify(content)
	require.NoError(t, err)
	require.Equal(t, KindAdditive, kind, "Down 段永不执行，不能让文件看起来是破坏性的: %v", reasons)
}

// 遍历声明：每个嵌入文件都能分类；*_notx.sql 只允许并发建删索引，必须是 additive。
func TestClassify_EveryEmbeddedMigration(t *testing.T) {
	for _, name := range embeddedMigrationNames(t) {
		kind, reasons, err := Classify(readEmbeddedMigration(t, name))
		require.NoErrorf(t, err, "%s", name)
		require.Containsf(t, []Kind{KindAdditive, KindDataRewrite, KindDestructive}, kind, "%s", name)
		if kind == KindAdditive {
			require.Emptyf(t, reasons, "%s", name)
		} else {
			require.NotEmptyf(t, reasons, "%s: non-additive verdict must explain itself", name)
		}
		if strings.HasSuffix(name, "_notx.sql") {
			require.Equalf(t, KindAdditive, kind, "%s: DROP/CREATE INDEX CONCURRENTLY never breaks a running replica: %v", name, reasons)
		}
	}
}

func TestClassify_Patterns(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		want   Kind
		reason string
	}{
		{"create table", "CREATE TABLE IF NOT EXISTS t (id INT);", KindAdditive, ""},
		{"add column", "ALTER TABLE t ADD COLUMN IF NOT EXISTS c INT;", KindAdditive, ""},
		{"drop index", "DROP INDEX IF EXISTS idx_t; DROP INDEX CONCURRENTLY IF EXISTS idx_u;", KindAdditive, ""},
		{"drop trigger and recreate", "DROP TRIGGER IF EXISTS trg ON t; CREATE TRIGGER trg AFTER INSERT ON t FOR EACH ROW EXECUTE FUNCTION f();", KindAdditive, ""},
		{"drop not null", "ALTER TABLE t ALTER COLUMN c DROP NOT NULL;", KindAdditive, ""},
		{"constraint swap", "ALTER TABLE t DROP CONSTRAINT IF EXISTS t_check; ALTER TABLE t ADD CONSTRAINT t_check CHECK (c > 0);", KindAdditive, ""},
		{"bare drop constraint", "ALTER TABLE t DROP CONSTRAINT IF EXISTS t_check;", KindDestructive, "DROP CONSTRAINT t_check on t"},
		{"drop table", "DROP TABLE IF EXISTS a, b CASCADE;", KindDestructive, "DROP TABLE b"},
		{"drop view", "DROP VIEW IF EXISTS v;", KindDestructive, "DROP VIEW v"},
		{"drop column", "ALTER TABLE t DROP COLUMN IF EXISTS c, DROP COLUMN d;", KindDestructive, "DROP COLUMN t.d"},
		{"drop column without keyword", "ALTER TABLE t DROP IF EXISTS c;", KindDestructive, "DROP COLUMN t.c"},
		{"alter column type", "ALTER TABLE t ALTER COLUMN c TYPE VARCHAR(128);", KindDestructive, "ALTER COLUMN t.c TYPE"},
		{"alter column set data type", "ALTER TABLE t ALTER c SET DATA TYPE TEXT;", KindDestructive, "ALTER COLUMN t.c TYPE"},
		{"rename table", "ALTER TABLE t RENAME TO u;", KindDestructive, "ALTER t RENAME TO u"},
		{"rename column", "ALTER TABLE t RENAME COLUMN a TO b;", KindDestructive, "RENAME COLUMN a TO b"},
		{"truncate", "TRUNCATE TABLE t;", KindDestructive, "TRUNCATE t"},
		{"delete without where", "DELETE FROM t;", KindDestructive, "DELETE FROM t without WHERE"},
		{"delete with where", "DELETE FROM t WHERE id < 10;", KindDataRewrite, "DELETE FROM t (filtered)"},
		{"update", "UPDATE t SET c = 1 WHERE c IS NULL;", KindDataRewrite, "UPDATE t"},
		{"update with alias", "UPDATE t AS x SET c = 1;", KindDataRewrite, "UPDATE t"},
		{"insert", "INSERT INTO settings (key, value) VALUES ('a', 'b') ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;", KindDataRewrite, "INSERT INTO settings"},
		{"seed of a table created here", "CREATE TABLE IF NOT EXISTS s (id INT PRIMARY KEY); INSERT INTO s (id) VALUES (1) ON CONFLICT DO NOTHING; UPDATE s SET id = 1;", KindAdditive, ""},
		{"drop of a table created here", "CREATE TEMP TABLE staging (id INT); INSERT INTO staging SELECT 1; DROP TABLE staging;", KindAdditive, ""},
		{"helper function created and dropped", "CREATE OR REPLACE FUNCTION public.__helper(TEXT) RETURNS BOOLEAN LANGUAGE sql AS $$ SELECT TRUE $$; DROP FUNCTION IF EXISTS public.__helper(TEXT);", KindAdditive, ""},
		{"drop foreign function", "DROP FUNCTION IF EXISTS public.other(TEXT, INT);", KindDestructive, "DROP FUNCTION other"},
		{"function body is not executed", "CREATE OR REPLACE FUNCTION f() RETURNS TRIGGER LANGUAGE plpgsql AS $$ BEGIN UPDATE t SET c = 1; DELETE FROM t; END $$;", KindAdditive, ""},
		{"do block is executed", "DO $$ BEGIN IF TRUE THEN ALTER TABLE t DROP COLUMN c; END IF; END $$;", KindDestructive, "DROP COLUMN t.c"},
		{"do block constraint swap", "DO $$ BEGIN ALTER TABLE t DROP CONSTRAINT IF EXISTS t_check; ALTER TABLE t ADD CONSTRAINT t_check CHECK (c > 0); END $$;", KindAdditive, ""},
		{"dynamic ddl in execute string", "DO $$ BEGIN EXECUTE 'ALTER TABLE t DROP COLUMN c'; END $$;", KindDestructive, "DROP COLUMN t.c"},
		{"unknown drop kind is destructive", "DROP ROLE IF EXISTS reporting;", KindDestructive, "DROP ROLE reporting"},
		{"comment mentioning drop is ignored", "-- we will DROP TABLE t one day\nCREATE INDEX IF NOT EXISTS idx ON t (c); /* DELETE FROM t */", KindAdditive, ""},
		{"for update lock is not an update", "DO $$ DECLARE v INT; BEGIN SELECT c INTO v FROM t WHERE id = 1 FOR UPDATE; END $$;", KindAdditive, ""},
		{"trigger on update is not an update", "CREATE TRIGGER trg AFTER UPDATE OF a, b ON t FOR EACH ROW EXECUTE FUNCTION f();", KindAdditive, ""},
		{"down section is ignored", "-- +goose Up\nCREATE TABLE IF NOT EXISTS t (id INT);\n-- +goose Down\nDROP TABLE IF EXISTS t;", KindAdditive, ""},
		{"max over statements", "CREATE TABLE IF NOT EXISTS t (id INT); UPDATE u SET c = 1; DROP TABLE IF EXISTS legacy;", KindDestructive, "DROP TABLE legacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, reasons, err := Classify(tc.sql)
			require.NoError(t, err)
			require.Equalf(t, tc.want, kind, "reasons: %v", reasons)
			if tc.reason == "" {
				require.Empty(t, reasons)
				return
			}
			require.Contains(t, strings.Join(reasons, "\n"), tc.reason)
		})
	}
}

func TestClassify_MalformedGooseFileIsAnError(t *testing.T) {
	_, _, err := Classify("-- +goose Down\nDROP TABLE t;")
	require.Error(t, err)
}
