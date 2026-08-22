package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/redis/go-redis/v9"
)

// instanceHeartbeatKey 是实例注册表的 Redis key。
//
// 结构选择：单个 sorted set，score = 心跳的 unix 秒，member = JSON{id,hostname,version}。
// 没有选「每个实例一个带 TTL 的 key」：那样列出活跃实例需要 SCAN 整个 keyspace，多次往返
// 且非原子；sorted set 一次 ZRANGEBYSCORE 就拿到窗口内的全部实例，ZREMRANGEBYSCORE 顺手
// 淘汰过期成员，整个 key 再挂一个 2×窗口 的 EXPIRE，整套部署下线后不留垃圾。
const instanceHeartbeatKey = "instances:heartbeat"

// instanceMember 是 sorted set 成员的编码形式。字段在进程生命周期内不变，所以同一实例
// 每次心跳编码出的字符串完全一致，ZADD 只会更新 score 而不会产生重复成员。
type instanceMember struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
}

type instanceHeartbeatStore struct {
	rdb *redis.Client
}

// NewInstanceHeartbeatStore 返回 service.InstanceHeartbeatStore 的 Redis 实现。
func NewInstanceHeartbeatStore(rdb *redis.Client) service.InstanceHeartbeatStore {
	return &instanceHeartbeatStore{rdb: rdb}
}

func encodeInstanceMember(inst service.InstanceInfo) (string, error) {
	raw, err := json.Marshal(instanceMember{ID: inst.ID, Hostname: inst.Hostname, Version: inst.Version})
	if err != nil {
		return "", fmt.Errorf("encode instance member: %w", err)
	}
	return string(raw), nil
}

func (s *instanceHeartbeatStore) Heartbeat(ctx context.Context, inst service.InstanceInfo, now time.Time, window time.Duration) error {
	member, err := encodeInstanceMember(inst)
	if err != nil {
		return err
	}

	cutoff := strconv.FormatInt(now.Add(-window).Unix(), 10)
	pipe := s.rdb.TxPipeline()
	pipe.ZAdd(ctx, instanceHeartbeatKey, redis.Z{Score: float64(now.Unix()), Member: member})
	pipe.ZRemRangeByScore(ctx, instanceHeartbeatKey, "-inf", "("+cutoff)
	pipe.Expire(ctx, instanceHeartbeatKey, 2*window)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *instanceHeartbeatStore) ListActive(ctx context.Context, since time.Time) ([]service.InstanceInfo, error) {
	entries, err := s.rdb.ZRangeByScoreWithScores(ctx, instanceHeartbeatKey, &redis.ZRangeBy{
		Min: strconv.FormatInt(since.Unix(), 10),
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, err
	}

	out := make([]service.InstanceInfo, 0, len(entries))
	for _, entry := range entries {
		raw, ok := entry.Member.(string)
		if !ok {
			return nil, fmt.Errorf("instance registry member has unexpected type %T", entry.Member)
		}
		var m instanceMember
		// 解析不了的成员不能跳过：它很可能来自另一个版本的进程，而「有一个我不认识的实例
		// 在跑」恰恰是必须拒绝原地更新的情形。返回错误让调用方按「数量未知」处理。
		if err := json.Unmarshal([]byte(raw), &m); err != nil || m.ID == "" {
			return nil, fmt.Errorf("instance registry member %q is not decodable: %w", raw, err)
		}
		out = append(out, service.InstanceInfo{
			ID:       m.ID,
			Hostname: m.Hostname,
			Version:  m.Version,
			LastSeen: time.Unix(int64(entry.Score), 0),
		})
	}
	return out, nil
}

func (s *instanceHeartbeatStore) Remove(ctx context.Context, inst service.InstanceInfo) error {
	member, err := encodeInstanceMember(inst)
	if err != nil {
		return err
	}
	return s.rdb.ZRem(ctx, instanceHeartbeatKey, member).Err()
}
