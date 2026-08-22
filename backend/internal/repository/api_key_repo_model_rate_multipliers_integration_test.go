//go:build integration

package repository

// 投影漏列回归（repository 半程）：GetByKeyForAuth 的分组显式投影必须带出
// model_rate_multipliers，否则认证快照里的逐模型倍率永远为空，扣费与利润门静默按 1 处理。
// 同时钉死降级语义：列里的坏 JSON 只告警并按"未配置"处理，不能让认证查询失败。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetByKeyForAuthCarriesModelRateMultipliersProjection(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("model-rate-proj-group-%d", suffix), Platform: service.PlatformAnthropic, RateMultiplier: 1.5,
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("model-rate-proj-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-model-rate-proj-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &groupID, Key: keyValue, Name: "model-rate-proj", Status: service.StatusActive}
	require.NoError(t, apiKeyRepo.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	// 走真实写路径：repository.Update 负责序列化 JSONB 列。
	groupRepo := NewGroupRepository(integrationEntClient, integrationDB)
	stored, err := groupRepo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	stored.ModelRateMultipliers = []service.GroupModelRateMultiplier{
		{ModelPattern: "claude-opus-*", Multiplier: 2},
		{ModelPattern: "claude-haiku-*", Multiplier: 0.5},
	}
	require.NoError(t, groupRepo.Update(ctx, stored))

	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group, "认证查询必须带出分组")
	require.Equal(t, stored.ModelRateMultipliers, got.Group.ModelRateMultipliers,
		"model_rate_multipliers 必须进入认证投影（投影漏列会让逐模型倍率静默失效）")
	require.Equal(t, 2.0, service.ResolveGroupModelRateMultiplier(got.Group, "claude-opus-4-1"))

	reloaded, err := groupRepo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, stored.ModelRateMultipliers, reloaded.ModelRateMultipliers, "普通读路径同样还原列表")

	// 坏 JSON：降级为"未配置"，不让认证查询失败。
	_, err = integrationDB.ExecContext(ctx, `UPDATE groups SET model_rate_multipliers = '{"model_pattern": 1}'::jsonb WHERE id = $1`, group.ID)
	require.NoError(t, err)
	got, err = apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group)
	require.Nil(t, got.Group.ModelRateMultipliers)
	require.Equal(t, 1.0, service.ResolveGroupModelRateMultiplier(got.Group, "claude-opus-4-1"))
}
