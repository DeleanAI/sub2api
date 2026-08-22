//go:build integration

package repository

// usage_logs.model_rate_multiplier 快照列（migration 229）的写读往返：
// 单条 INSERT、批量 CTE、best-effort 三条插入路径共用同一份列清单，任一路径漏列
// 都会在真实 PostgreSQL 上报列数不匹配；读侧 NULL 必须还原为 nil（按 1 处理）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUsageLogRepository_ModelRateMultiplierRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUsageLogRepository(client, integrationDB).(*usageLogRepository)
	suffix := time.Now().UnixNano()
	user := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("model-rate-usage-%d@example.com", suffix)})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-model-rate-usage-" + uuid.NewString(), Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-model-rate-usage-" + uuid.NewString()})
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM usage_logs WHERE api_key_id = $1", apiKey.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", apiKey.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
	})

	newLog := func(multiplier *float64) *service.UsageLog {
		return &service.UsageLog{
			UserID:              user.ID,
			APIKeyID:            apiKey.ID,
			AccountID:           account.ID,
			RequestID:           uuid.NewString(),
			Model:               "claude-opus-4-1",
			InputTokens:         10,
			OutputTokens:        20,
			TotalCost:           0.5,
			RateMultiplier:      1.5,
			ActualCost:          1.5,
			ModelRateMultiplier: multiplier,
		}
	}

	t.Run("batched create path persists the snapshot", func(t *testing.T) {
		two := 2.0
		log := newLog(&two)
		inserted, err := repo.Create(ctx, log)
		require.NoError(t, err)
		require.True(t, inserted)
		got, err := repo.GetByID(ctx, log.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ModelRateMultiplier)
		require.InDelta(t, 2.0, *got.ModelRateMultiplier, 1e-9)
	})

	t.Run("single create path persists the snapshot", func(t *testing.T) {
		half := 0.5
		log := newLog(&half)
		inserted, err := repo.createSingle(ctx, integrationDB, log)
		require.NoError(t, err)
		require.True(t, inserted)
		got, err := repo.GetByID(ctx, log.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ModelRateMultiplier)
		require.InDelta(t, 0.5, *got.ModelRateMultiplier, 1e-9)
	})

	t.Run("best-effort path persists the snapshot", func(t *testing.T) {
		three := 3.0
		log := newLog(&three)
		require.NoError(t, repo.CreateBestEffort(ctx, log))
		var stored float64
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			"SELECT model_rate_multiplier FROM usage_logs WHERE request_id = $1 AND api_key_id = $2", log.RequestID, apiKey.ID).Scan(&stored))
		require.InDelta(t, 3.0, stored, 1e-9)
	})

	t.Run("nil snapshot is stored as NULL and read back as nil", func(t *testing.T) {
		log := newLog(nil)
		inserted, err := repo.Create(ctx, log)
		require.NoError(t, err)
		require.True(t, inserted)
		got, err := repo.GetByID(ctx, log.ID)
		require.NoError(t, err)
		require.Nil(t, got.ModelRateMultiplier)
	})
}
