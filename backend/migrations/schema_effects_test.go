//go:build unit

package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTablesAffected(t *testing.T) {
	effects, err := TablesAffected(`
-- +goose Up
CREATE TABLE IF NOT EXISTS a (id INT);
CREATE UNLOGGED TABLE b (id INT);
DROP TABLE IF EXISTS legacy_one, legacy_two CASCADE;
ALTER TABLE old_name RENAME TO new_name;
ALTER TABLE a RENAME COLUMN x TO y;
DO $$ BEGIN EXECUTE format('CREATE TABLE IF NOT EXISTS part_%s PARTITION OF a FOR VALUES FROM (1) TO (2)', 'x'); END $$;
CREATE OR REPLACE FUNCTION f() RETURNS VOID LANGUAGE plpgsql AS $$ BEGIN CREATE TABLE inside_function (id INT); END $$;
-- +goose Down
DROP TABLE a;
`)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a", "b", "new_name"}, effects.Created)
	require.ElementsMatch(t, []string{"legacy_one", "legacy_two", "old_name"}, effects.Dropped)
	require.ElementsMatch(t, []string{"part_%s"}, effects.Dynamic)

	net, err := TablesAffected("CREATE TEMP TABLE staging (id INT); DROP TABLE staging; DROP TABLE IF EXISTS rebuilt; CREATE TABLE rebuilt (id INT);")
	require.NoError(t, err)
	require.Equal(t, []string{"rebuilt"}, net.Created, "先删后建是重建，执行完应当存在")
	require.Equal(t, []string{"staging"}, net.Dropped, "先建后删的临时表执行完不存在，只出现在 Dropped")
}
