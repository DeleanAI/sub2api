package repository

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// usage_logs INSERT 的测试辅助：不带构建标签，单测与默认构建的测试共用。

var usageLogStaticInsertShapeRe = regexp.MustCompile(`(?s)INSERT INTO usage_logs \((.*?)\) VALUES \((.*?)\)`)

// newSQLCapturingMock 返回把实际下发 SQL 记录到 captured 的 sqlmock；语句一律视为匹配，
// 参数仍由 WithArgs 校验。
func newSQLCapturingMock(t *testing.T, captured *[]string) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	matcher := sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
		*captured = append(*captured, actualSQL)
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// usageLogInsertColumnNames 返回 createSingle 实际下发的 INSERT 列清单。
func usageLogInsertColumnNames(t *testing.T) []string {
	t.Helper()
	var captured []string
	db, mock := newSQLCapturingMock(t, &captured)
	repo := &usageLogRepository{sql: db}
	log := &service.UsageLog{UserID: 1, APIKeyID: 2, RequestID: "client:columns", Model: "m", CreatedAt: time.Now().UTC()}
	mock.ExpectQuery("INSERT INTO usage_logs").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), log.CreatedAt))
	_, err := repo.Create(context.Background(), log)
	require.NoError(t, err)
	require.Len(t, captured, 1)
	m := usageLogStaticInsertShapeRe.FindStringSubmatch(captured[0])
	require.Len(t, m, 3, "unrecognised INSERT shape:\n%s", captured[0])
	var columns []string
	for _, col := range strings.Split(m[1], ",") {
		if c := strings.TrimSpace(col); c != "" {
			columns = append(columns, c)
		}
	}
	return columns
}

// usageLogInsertColumnIndex 返回 column 在 INSERT 列清单里的下标：测试按列名找参数位置，而不是写死「下标 39」
// 「倒数第 3 个」——加列不会让无关的断言失效。
func usageLogInsertColumnIndex(t *testing.T, column string) int {
	t.Helper()
	for i, c := range usageLogInsertColumnNames(t) {
		if c == column {
			return i
		}
	}
	t.Fatalf("column %q is not in the usage_logs INSERT column list", column)
	return -1
}
