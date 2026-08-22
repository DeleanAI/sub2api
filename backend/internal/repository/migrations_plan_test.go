package repository

import (
	"context"
	"testing"
	"testing/fstest"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestPlanMigrations_NilDB(t *testing.T) {
	_, err := PlanMigrations(context.Background(), nil, fstest.MapFS{})
	require.Error(t, err)
}

func TestPlanMigrations_FreshDatabaseHasEverythingPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT EXISTS \\(").
		WithArgs("schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	fsys := fstest.MapFS{
		"002_drop.sql":  &fstest.MapFile{Data: []byte("DROP TABLE IF EXISTS legacy;")},
		"001_init.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE IF NOT EXISTS t (id INT);")},
		"003_empty.sql": &fstest.MapFile{Data: []byte("  \n")},
	}
	plan, err := PlanMigrations(context.Background(), db, fsys)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Equal(t, 0, plan.AppliedCount)
	require.Empty(t, plan.LastApplied)
	require.Empty(t, plan.UnknownApplied)
	require.Empty(t, plan.ChecksumMismatches)
	require.Len(t, plan.Pending, 2, "空文件与运行器一样被跳过")
	require.Equal(t, "001_init.sql", plan.Pending[0].Filename, "pending 必须按执行顺序")
	require.Equal(t, migrations.KindAdditive, plan.Pending[0].Kind)
	require.Equal(t, "002_drop.sql", plan.Pending[1].Filename)
	require.Equal(t, migrations.KindDestructive, plan.Pending[1].Kind)
	require.Equal(t, []string{"DROP TABLE legacy"}, plan.Pending[1].Reasons)
	require.Equal(t, migrations.KindDestructive, plan.PendingKind())
}

func TestPlanMigrations_AppliedRowsAreDiffedAgainstEmbeddedFiles(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	initSQL := "CREATE TABLE IF NOT EXISTS t (id INT);"
	editedSQL := "ALTER TABLE t ADD COLUMN IF NOT EXISTS c INT;"
	mock.ExpectQuery("SELECT EXISTS \\(").
		WithArgs("schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("SELECT filename, checksum FROM schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"filename", "checksum"}).
			AddRow("001_init.sql", migrationChecksum(initSQL)).
			AddRow("002_edited.sql", "checksum-recorded-before-the-file-was-edited").
			AddRow("900_from_newer_image.sql", "whatever"))

	fsys := fstest.MapFS{
		"001_init.sql":   &fstest.MapFile{Data: []byte(initSQL)},
		"002_edited.sql": &fstest.MapFile{Data: []byte(editedSQL)},
		"003_seed.sql":   &fstest.MapFile{Data: []byte("INSERT INTO t (id) VALUES (1) ON CONFLICT DO NOTHING;")},
	}
	plan, err := PlanMigrations(context.Background(), db, fsys)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Equal(t, 3, plan.AppliedCount)
	require.Equal(t, "900_from_newer_image.sql", plan.LastApplied)
	require.Equal(t, []string{"900_from_newer_image.sql"}, plan.UnknownApplied)
	require.Equal(t, []MigrationChecksumMismatch{{
		Filename:     "002_edited.sql",
		DBChecksum:   "checksum-recorded-before-the-file-was-edited",
		FileChecksum: migrationChecksum(editedSQL),
	}}, plan.ChecksumMismatches)
	require.Len(t, plan.Pending, 1)
	require.Equal(t, "003_seed.sql", plan.Pending[0].Filename)
	require.Equal(t, migrations.KindDataRewrite, plan.Pending[0].Kind)
	require.Equal(t, migrations.KindDataRewrite, plan.PendingKind())
}

func TestPlanMigrations_NothingPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	initSQL := "CREATE TABLE IF NOT EXISTS t (id INT);"
	mock.ExpectQuery("SELECT EXISTS \\(").
		WithArgs("schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("SELECT filename, checksum FROM schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"filename", "checksum"}).AddRow("001_init.sql", migrationChecksum(initSQL)))

	plan, err := PlanMigrations(context.Background(), db, fstest.MapFS{
		"001_init.sql": &fstest.MapFile{Data: []byte(initSQL)},
	})
	require.NoError(t, err)
	require.Empty(t, plan.Pending)
	require.Equal(t, migrations.Kind(""), plan.PendingKind())
	require.Equal(t, "001_init.sql", plan.LastApplied)
}

func TestPlanMigrations_MalformedPendingFileIsReported(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT EXISTS \\(").
		WithArgs("schema_migrations").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	_, err = PlanMigrations(context.Background(), db, fstest.MapFS{
		"001_bad.sql": &fstest.MapFile{Data: []byte("-- +goose Down\nDROP TABLE t;")},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "classify migration 001_bad.sql")
}
