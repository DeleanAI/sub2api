package repository

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const gooseMarkedMigration = `-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS t (id INT);
CREATE INDEX IF NOT EXISTS idx_t ON t (id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS t;
-- +goose StatementEnd`

func TestApplyMigrationsFS_ExecutesOnlyGooseUpSection(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	prepareMigrationsBootstrapExpectations(mock)
	mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").
		WithArgs("001_goose.sql").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	// 锚定整条语句：执行文本必须恰好是 Up 段，既没有注解行，也没有 Down 段的 DROP TABLE。
	mock.ExpectExec("^CREATE TABLE IF NOT EXISTS t \\(id INT\\); CREATE INDEX IF NOT EXISTS idx_t ON t \\(id\\);$").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// checksum 仍然覆盖整份文件（含 Down 段），已应用库的记录才不会因为运行器改动而失配。
	mock.ExpectExec("INSERT INTO schema_migrations \\(filename, checksum\\) VALUES \\(\\$1, \\$2\\)").
		WithArgs("001_goose.sql", migrationChecksum(gooseMarkedMigration)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectExec("SELECT pg_advisory_unlock\\(\\$1\\)").
		WithArgs(migrationsAdvisoryLockID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fsys := fstest.MapFS{
		"001_goose.sql": &fstest.MapFile{Data: []byte(gooseMarkedMigration)},
	}
	require.NoError(t, applyMigrationsFS(context.Background(), db, fsys))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestApplyMigrationsFS_RejectsMalformedGooseFile(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	prepareMigrationsBootstrapExpectations(mock)
	mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").
		WithArgs("001_down_only.sql").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("SELECT pg_advisory_unlock\\(\\$1\\)").
		WithArgs(migrationsAdvisoryLockID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	fsys := fstest.MapFS{
		"001_down_only.sql": &fstest.MapFile{Data: []byte("-- +goose Down\nDROP TABLE IF EXISTS t;")},
	}
	err = applyMigrationsFS(context.Background(), db, fsys)
	require.Error(t, err, "一个只有 Down 段的文件等于什么都不做，必须显式失败而不是静默跳过")
	require.Contains(t, err.Error(), "validate migration 001_down_only.sql")
	require.NoError(t, mock.ExpectationsWereMet(), "不合法的文件不能执行任何 SQL，也不能被记录为已应用")
}

func TestMigrationChecksum_IsStableAcrossSurroundingWhitespace(t *testing.T) {
	require.Equal(t, migrationChecksum("SELECT 1;"), migrationChecksum("\n  SELECT 1;\n\n"))
	require.NotEqual(t, migrationChecksum("SELECT 1;"), migrationChecksum("SELECT 2;"))
	require.Len(t, migrationChecksum("SELECT 1;"), 64)
}
