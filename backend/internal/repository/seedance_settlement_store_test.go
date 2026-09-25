//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 结算索引的四个操作对着真实的 Redis 协议走一遍：到期按分数取、限量、改期覆盖分数、移除后不再待结。
func TestSeedanceSettlementStoreSchedulesByDueTime(t *testing.T) {
	mr := miniredis.RunT(t)
	store := NewSeedanceSettlementStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)

	require.NoError(t, store.ScheduleSeedanceSettlement(ctx, "early", now.Add(-2*time.Minute)))
	require.NoError(t, store.ScheduleSeedanceSettlement(ctx, "due-now", now))
	require.NoError(t, store.ScheduleSeedanceSettlement(ctx, "later", now.Add(time.Minute)))

	due, err := store.DueSeedanceSettlements(ctx, now, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"early", "due-now"}, due, "到期（含恰好到期）的按到期先后返回，未到期的不返回")

	due, err = store.DueSeedanceSettlements(ctx, now, 1)
	require.NoError(t, err)
	require.Equal(t, []string{"early"}, due, "一次最多取 limit 个")

	// 改期是覆盖分数，不是追加第二个成员。
	require.NoError(t, store.ScheduleSeedanceSettlement(ctx, "early", now.Add(time.Hour)))
	due, err = store.DueSeedanceSettlements(ctx, now, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"due-now"}, due)

	pending, err := store.SeedanceSettlementPending(ctx, "later")
	require.NoError(t, err)
	require.True(t, pending)
	require.NoError(t, store.RemoveSeedanceSettlement(ctx, "later"))
	pending, err = store.SeedanceSettlementPending(ctx, "later")
	require.NoError(t, err)
	require.False(t, pending, "移除后不再待结")

	mr.SetError("boom")
	_, err = store.SeedanceSettlementPending(ctx, "due-now")
	require.Error(t, err, "Redis 出错必须报出来，不能当成「不在索引里」")
}
