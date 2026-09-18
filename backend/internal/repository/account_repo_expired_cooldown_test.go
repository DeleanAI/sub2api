package repository

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// accountCooldownColumns 是「带到期时间的冷却列」这份声明。
// ListAccountsWithExpiredCooldown 的判据必须覆盖其中每一列——新增一种冷却只要加进
// 这个列表，本测试当天就会因为 SQL 没覆盖它而失败，而不是等线上账号卡死才发现。
var accountCooldownColumns = []string{
	"rate_limit_reset_at",
	"temp_unschedulable_until",
	"overload_until",
}

func TestListAccountsWithExpiredCooldownCoversEveryCooldownColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	var captured string
	mock.ExpectQuery(`(?s)SELECT id.*FROM accounts`).
		WithArgs(now, 200).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)).AddRow(int64(9)))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	ids, err := repo.ListAccountsWithExpiredCooldown(context.Background(), now, 200)
	require.NoError(t, err)
	require.Equal(t, []int64{7, 9}, ids)
	require.NoError(t, mock.ExpectationsWereMet())

	// 直接检查 SQL 文本：每一列都必须同时出现「非空」与「已到期」两个条件，
	// 否则该冷却类型到期后不会被捞回。
	captured = listAccountsWithExpiredCooldownQuery
	normalized := strings.Join(strings.Fields(captured), " ")
	for _, column := range accountCooldownColumns {
		require.Containsf(t, normalized, column+" IS NOT NULL",
			"判据漏了 %s 的非空检查，该冷却到期后无法自愈", column)
		require.Containsf(t, normalized, column+" <= $1",
			"判据漏了 %s 的到期比较，该冷却到期后无法自愈", column)
	}
	// 只恢复实际不可调度的账号，避免每轮重复清理健康账号。
	require.Contains(t, normalized, "status = 'error' OR schedulable = FALSE")
	require.Contains(t, normalized, "deleted_at IS NULL")
}

func TestListAccountsWithExpiredCooldownDefaultsLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now()
	// limit <= 0 必须落到一个有界默认值，不能退化成无上限扫描。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id")).
		WithArgs(now, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	repo := newAccountRepositoryWithSQL(nil, db, nil)
	ids, err := repo.ListAccountsWithExpiredCooldown(context.Background(), now, 0)
	require.NoError(t, err)
	require.Empty(t, ids)
	require.NoError(t, mock.ExpectationsWereMet())
}
