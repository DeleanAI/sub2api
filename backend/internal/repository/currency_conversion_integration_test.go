//go:build integration

package repository

// 汇率存档与 currency_conversion（migration 241）在真实 PostgreSQL 上的读写：
//   - exchange_rates 按「交易时刻之前最近一次公布」取数，同日报价以后抓到的为准；
//   - 兑换码 / 支付订单 / 用量日志（三条插入路径）/ 批量图片任务的折算依据原样往返，没有时是 NULL；
//   - 迁移重放只给没写币种的存量定价条目回填迁移时的记账币种，写了的不动。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func cfetsFixing(date string, cnyPerUSD float64) service.ExchangeRate {
	day, _ := time.Parse(time.DateOnly, date)
	return service.ExchangeRate{
		Currency: "CNY", RateDate: date, USDPerUnit: 1 / cnyPerUSD, Quote: cnyPerUSD, QuoteUnit: "CNY per USD",
		Source: service.FXSourceCFETSCentralParity, SourceURL: "https://www.chinamoney.com.cn/chinese/bkccpr/",
		PublishedAt: day.Add(time.Hour + 15*time.Minute), // 北京时间 09:15
		FetchedAt:   time.Date(2026, 10, 3, 2, 19, 56, 0, time.UTC),
	}
}

func testCurrencyConversion() *service.CurrencyConversion {
	from, to := 1000.0, 148.48
	leg := cfetsFixing("2026-09-30", 6.7351)
	return &service.CurrencyConversion{
		FromCurrency: "CNY", ToCurrency: "USD", FromAmount: &from, ToAmount: &to, Rate: 1 / 6.7351,
		At: time.Date(2026, 9, 30, 5, 55, 31, 0, time.UTC), Legs: []service.ExchangeRate{leg},
	}
}

func TestExchangeRateRepository_ArchiveRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := &exchangeRateRepository{db: testTx(t)}

	require.NoError(t, repo.UpsertExchangeRates(ctx, []service.ExchangeRate{
		cfetsFixing("2026-09-28", 6.7399), cfetsFixing("2026-09-29", 6.7411), cfetsFixing("2026-09-30", 6.7300),
		{Currency: "USDT", RateDate: "2026-10-02", USDPerUnit: 0.9997228229236466, Quote: 0.9997228229236466, QuoteUnit: "USD per USDT",
			Source: service.FXSourceCoinGeckoDaily, SourceURL: "https://www.coingecko.com/en/coins/tether",
			PublishedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
	}))
	// 同日再抓到的报价覆盖旧值。
	require.NoError(t, repo.UpsertExchangeRates(ctx, []service.ExchangeRate{cfetsFixing("2026-09-30", 6.7351)}))

	beforePublish, err := repo.LatestExchangeRatePublishedBy(ctx, "CNY", time.Date(2026, 9, 30, 1, 14, 59, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "2026-09-29", beforePublish.RateDate, "09:14:59 Beijing time still uses the previous fixing")

	atPublish, err := repo.LatestExchangeRatePublishedBy(ctx, "CNY", time.Date(2026, 9, 30, 1, 15, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "2026-09-30", atPublish.RateDate)
	require.Equal(t, 6.7351, atPublish.Quote)
	require.InDelta(t, 1/6.7351, atPublish.USDPerUnit, 1e-12)
	require.Equal(t, service.FXSourceCFETSCentralParity, atPublish.Source)

	holiday, err := repo.LatestExchangeRatePublishedBy(ctx, "CNY", time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, "2026-09-30", holiday.RateDate, "no fixing during the National Day holiday")

	usdt, err := repo.GetExchangeRate(ctx, "USDT", "2026-10-02")
	require.NoError(t, err)
	require.InDelta(t, 0.9997228229236466, usdt.USDPerUnit, 1e-12)
	missing, err := repo.GetExchangeRate(ctx, "USDC", "2026-10-02")
	require.NoError(t, err)
	require.Nil(t, missing)

	listed, err := repo.ListExchangeRates(ctx, "CNY", 2)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, []string{"2026-09-30", "2026-09-29"}, []string{listed[0].RateDate, listed[1].RateDate})
}

func TestCurrencyConversion_RoundTripsThroughEveryTable(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	conversion := testCurrencyConversion()
	suffix := time.Now().UnixNano()
	user := mustCreateUser(t, client, &service.User{Email: fmt.Sprintf("fx-roundtrip-%d@example.com", suffix)})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-fx-roundtrip-" + uuid.NewString(), Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "acc-fx-roundtrip-" + uuid.NewString()})
	t.Cleanup(func() {
		for _, stmt := range []string{
			"DELETE FROM usage_logs WHERE api_key_id = $1",
			"DELETE FROM api_keys WHERE id = $1",
		} {
			_, err := integrationDB.ExecContext(ctx, stmt, apiKey.ID)
			require.NoError(t, err)
		}
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM payment_orders WHERE user_id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", account.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
	})

	t.Run("usage logs, all three insert paths", func(t *testing.T) {
		repo := NewUsageLogRepository(client, integrationDB).(*usageLogRepository)
		newLog := func(c *service.CurrencyConversion) *service.UsageLog {
			return &service.UsageLog{UserID: user.ID, APIKeyID: apiKey.ID, AccountID: account.ID, RequestID: uuid.NewString(),
				Model: "doubao-seedance-2-5", OutputTokens: 100_000, TotalCost: 1.03932, ActualCost: 0.83146, RateMultiplier: 0.8,
				CurrencyConversion: c}
		}
		readBack := func(log *service.UsageLog) *service.CurrencyConversion {
			var id int64
			require.NoError(t, integrationDB.QueryRowContext(ctx,
				"SELECT id FROM usage_logs WHERE request_id = $1 AND api_key_id = $2", log.RequestID, apiKey.ID).Scan(&id))
			got, err := repo.GetByID(ctx, id)
			require.NoError(t, err)
			return got.CurrencyConversion
		}

		batched := newLog(conversion)
		_, err := repo.Create(ctx, batched)
		require.NoError(t, err)
		require.Equal(t, conversion, readBack(batched))

		single := newLog(conversion)
		_, err = repo.createSingle(ctx, integrationDB, single)
		require.NoError(t, err)
		require.Equal(t, conversion, readBack(single))

		bestEffort := newLog(conversion)
		require.NoError(t, repo.CreateBestEffort(ctx, bestEffort))
		require.Equal(t, conversion, readBack(bestEffort))

		usd := newLog(nil)
		_, err = repo.Create(ctx, usd)
		require.NoError(t, err)
		require.Nil(t, readBack(usd))
		var isNull bool
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			"SELECT currency_conversion IS NULL FROM usage_logs WHERE request_id = $1", usd.RequestID).Scan(&isNull))
		require.True(t, isNull)
	})

	t.Run("redeem codes", func(t *testing.T) {
		tx := testEntTx(t)
		repo := NewRedeemCodeRepository(tx.Client())
		code := &service.RedeemCode{Code: fmt.Sprintf("FX%d", suffix), Type: service.RedeemTypeBalance, Value: 148.48,
			Status: service.StatusUsed, Notes: "admin add 1000 CNY", CurrencyConversion: conversion}
		require.NoError(t, repo.Create(ctx, code))
		got, err := repo.GetByCode(ctx, code.Code)
		require.NoError(t, err)
		require.Equal(t, conversion, got.CurrencyConversion)
	})

	t.Run("payment orders", func(t *testing.T) {
		raw, err := service.EncodeCurrencyConversion(conversion)
		require.NoError(t, err)
		order, err := client.PaymentOrder.Create().
			SetUserID(user.ID).SetUserEmail(user.Email).SetUserName("fx").
			SetAmount(148.48).SetPayAmount(1000).SetFeeRate(0).SetRechargeCode("").
			SetOutTradeNo("fx" + fmt.Sprint(suffix)).SetPaymentType("alipay").SetPaymentTradeNo("").
			SetOrderType("balance").SetStatus(service.OrderStatusPending).SetExpiresAt(time.Now().Add(time.Hour)).
			SetClientIP("127.0.0.1").SetSrcHost("example.com").SetCurrencyConversion(raw).
			Save(ctx)
		require.NoError(t, err)
		reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
		require.NoError(t, err)
		require.Equal(t, conversion, service.DecodeCurrencyConversion(reloaded.CurrencyConversion, "test"))
	})

	t.Run("batch image jobs", func(t *testing.T) {
		repo := newBatchImageRepositoryWithSQL(testTx(t))
		job, err := repo.CreateBatchImageJob(ctx, service.CreateBatchImageJobParams{
			BatchID: batchImageTestID(t, "fx"), UserID: user.ID, Provider: service.BatchImageProviderVertex,
			Model: "gemini-2.5-flash-image", ItemCount: 1, BillableUnitPrice: 0.0371, PricingSnapshotVersion: 1,
			CurrencyConversion: conversion,
		})
		require.NoError(t, err)
		require.Equal(t, conversion, job.CurrencyConversion)
		got, err := repo.GetBatchImageJobByBatchID(ctx, job.BatchID)
		require.NoError(t, err)
		require.Equal(t, conversion, got.CurrencyConversion)
	})
}

func TestMigration241_ReplayBackfillsOnlyPricingWithoutACurrency(t *testing.T) {
	ctx := context.Background()
	group := mustCreateGroup(t, testEntClient(t), &service.Group{Name: "fx-migration-" + uuid.NewString(), RateMultiplier: 1})
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	tx := testTx(t)
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := tx.ExecContext(ctx, query, args...)
		require.NoError(t, err)
	}
	// 回到迁移前的形态：记账币种是 CNY，定价条目没写币种。
	exec(`INSERT INTO settings (key, value) VALUES ('balance_currency', 'CNY')
	      ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	exec(`ALTER TABLE channel_model_pricing ALTER COLUMN currency DROP NOT NULL`)
	var channelID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO channels (name) VALUES ($1) RETURNING id`, "fx-migration-"+uuid.NewString()).Scan(&channelID))
	var legacyID, explicitID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO channel_model_pricing (channel_id, platform, models, billing_mode, output_price, currency)
		VALUES ($1, 'openai', '["seedance"]', 'token', 0.00007, NULL) RETURNING id`, channelID).Scan(&legacyID))
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO channel_model_pricing (channel_id, platform, models, billing_mode, output_price, currency)
		VALUES ($1, 'openai', '["gpt-5.4"]', 'token', 0.000015, 'USD') RETURNING id`, channelID).Scan(&explicitID))
	exec(`UPDATE groups SET model_pricing = $1::jsonb WHERE id = $2`,
		`[{"models":["doubao-seedance-2-5"],"output_price":0.00007},{"models":["gpt-5.4"],"output_price":0.000015,"currency":"USD"}]`, group.ID)

	content, err := migrations.FS.ReadFile("241_maycluster_exchange_rates_and_currency_conversion.sql")
	require.NoError(t, err)
	exec(string(content))

	currencyOf := func(id int64) string {
		var c string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT currency FROM channel_model_pricing WHERE id = $1`, id).Scan(&c))
		return c
	}
	require.Equal(t, "CNY", currencyOf(legacyID), "existing entries were entered in the accounting currency of the time")
	require.Equal(t, "USD", currencyOf(explicitID), "entries that already say their currency are left alone")

	var nullable string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_name = 'channel_model_pricing' AND column_name = 'currency'`).Scan(&nullable))
	require.Equal(t, "NO", nullable, "the replay restores NOT NULL")

	var raw []byte
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT model_pricing FROM groups WHERE id = $1`, group.ID).Scan(&raw))
	var entries []map[string]any
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.Len(t, entries, 2)
	require.Equal(t, []any{"doubao-seedance-2-5"}, entries[0]["models"], "order is preserved")
	require.Equal(t, "CNY", entries[0]["currency"])
	require.Equal(t, "USD", entries[1]["currency"])
}
