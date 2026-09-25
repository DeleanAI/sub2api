package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// seedanceSettlementKey 是 Seedance「已创建、未计费」任务的索引：有序集合，成员是任务身份，
// 分数是下一次后台补查的 Unix 秒。
const seedanceSettlementKey = "seedance:settlement"

type seedanceSettlementStore struct {
	rdb *redis.Client
}

func NewSeedanceSettlementStore(rdb *redis.Client) service.SeedanceSettlementStore {
	return &seedanceSettlementStore{rdb: rdb}
}

func (s *seedanceSettlementStore) ScheduleSeedanceSettlement(ctx context.Context, member string, dueAt time.Time) error {
	return s.rdb.ZAdd(ctx, seedanceSettlementKey, redis.Z{Score: float64(dueAt.Unix()), Member: member}).Err()
}

func (s *seedanceSettlementStore) DueSeedanceSettlements(ctx context.Context, now time.Time, limit int) ([]string, error) {
	return s.rdb.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:     seedanceSettlementKey,
		Start:   "-inf",
		Stop:    strconv.FormatInt(now.Unix(), 10),
		ByScore: true,
		Count:   int64(limit),
	}).Result()
}

func (s *seedanceSettlementStore) SeedanceSettlementPending(ctx context.Context, member string) (bool, error) {
	err := s.rdb.ZScore(ctx, seedanceSettlementKey, member).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	return err == nil, err
}

func (s *seedanceSettlementStore) RemoveSeedanceSettlement(ctx context.Context, member string) error {
	return s.rdb.ZRem(ctx, seedanceSettlementKey, member).Err()
}
