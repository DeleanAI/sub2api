//go:build unit

package service

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

func TestNormalizeGroupModelRateMultipliers(t *testing.T) {
	t.Run("nil stays nil and empty stays empty", func(t *testing.T) {
		out, err := NormalizeGroupModelRateMultipliers(nil)
		require.NoError(t, err)
		require.Nil(t, out)

		out, err = NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{})
		require.NoError(t, err)
		require.NotNil(t, out)
		require.Empty(t, out)
	})

	t.Run("trims patterns and keeps order", func(t *testing.T) {
		out, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{
			{ModelPattern: "  claude-opus-* ", Multiplier: 2},
			{ModelPattern: "claude-haiku-*", Multiplier: 0.5},
		})
		require.NoError(t, err)
		require.Equal(t, []GroupModelRateMultiplier{
			{ModelPattern: "claude-opus-*", Multiplier: 2},
			{ModelPattern: "claude-haiku-*", Multiplier: 0.5},
		}, out)
	})

	reasonOf := func(t *testing.T, err error) string {
		t.Helper()
		require.Error(t, err)
		appErr := infraerrors.FromError(err)
		require.Equal(t, int32(http.StatusBadRequest), appErr.Code)
		return appErr.Reason
	}

	t.Run("rejects empty pattern", func(t *testing.T) {
		_, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{{ModelPattern: "   ", Multiplier: 2}})
		require.Equal(t, "GROUP_MODEL_RATE_MULTIPLIER_PATTERN_REQUIRED", reasonOf(t, err))
	})

	t.Run("rejects out-of-range multipliers", func(t *testing.T) {
		for _, bad := range []float64{0, -1, MaxGroupModelRateMultiplier + 0.001, math.NaN(), math.Inf(1)} {
			_, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{{ModelPattern: "gpt-5*", Multiplier: bad}})
			require.Equal(t, "GROUP_MODEL_RATE_MULTIPLIER_OUT_OF_RANGE", reasonOf(t, err), "multiplier %v", bad)
		}
		out, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{{ModelPattern: "gpt-5*", Multiplier: MaxGroupModelRateMultiplier}})
		require.NoError(t, err)
		require.Equal(t, MaxGroupModelRateMultiplier, out[0].Multiplier)
	})

	t.Run("rejects duplicates after normalization", func(t *testing.T) {
		_, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{
			{ModelPattern: "Claude-Opus-4.1", Multiplier: 2},
			{ModelPattern: "claude-opus-4-1", Multiplier: 3},
		})
		require.Equal(t, "GROUP_MODEL_RATE_MULTIPLIER_DUPLICATE_PATTERN", reasonOf(t, err))
	})
}

// TestGroupModelRateMultiplierFor 钉死查表规则：按列表顺序第一条命中生效（更宽泛的通配排在前面
// 会遮蔽后面的精确条目——这是有意的顺序语义），匹配规则与分组逐模型定价共用同一条归一化
// 精确/末尾通配匹配。
func TestGroupModelRateMultiplierFor(t *testing.T) {
	group := &Group{ModelRateMultipliers: []GroupModelRateMultiplier{
		{ModelPattern: "claude-opus-*", Multiplier: 2},
		{ModelPattern: "claude-opus-4-1", Multiplier: 3},
		{ModelPattern: "GPT-5.4", Multiplier: 1.5},
	}}

	entry, ok := group.ModelRateMultiplierFor("claude-opus-4-1")
	require.True(t, ok)
	require.Equal(t, "claude-opus-*", entry.ModelPattern, "列表顺序即优先级：第一条命中的通配遮蔽后面的精确条目")

	entry, ok = group.ModelRateMultiplierFor("Claude-Opus-4.5")
	require.True(t, ok, "模型名归一化（大小写、claude 系列 . → -）后再匹配")
	require.Equal(t, 2.0, entry.Multiplier)

	entry, ok = group.ModelRateMultiplierFor("gpt-5.4")
	require.True(t, ok, "模式同样归一化")
	require.Equal(t, 1.5, entry.Multiplier)

	_, ok = group.ModelRateMultiplierFor("gpt-5.4-mini")
	require.False(t, ok, "精确模式不做前缀匹配")
	_, ok = group.ModelRateMultiplierFor("")
	require.False(t, ok)
	_, ok = (*Group)(nil).ModelRateMultiplierFor("claude-opus-4-1")
	require.False(t, ok)

	// 与分组逐模型定价共用同一条匹配规则：同一模式对同一模型的命中结论一致。
	pricingGroup := &Group{ModelPricing: []ChannelModelPricing{{Models: []string{"claude-opus-*"}, BillingMode: BillingModeToken}}}
	require.NotNil(t, matchGroupModelPricing(pricingGroup, "Claude-Opus-4.5"))
	require.Nil(t, matchGroupModelPricing(pricingGroup, "claude-sonnet-4"))
}

func TestResolveGroupModelRateMultiplier_DegradesInvalidStoredValueToOneAndLogs(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	group := &Group{ID: 404, ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: "gpt-5*", Multiplier: 0}}}
	require.Equal(t, 1.0, resolveGroupModelRateMultiplier(group, "gpt-5.4"), "存量非法倍率按 1 处理，不能按非法值扣费")
	require.Contains(t, logs.String(), "group_model_rate_multiplier_invalid_degraded_to_one")
	require.Contains(t, logs.String(), "group_id=404")

	require.Equal(t, 1.0, resolveGroupModelRateMultiplier(nil, "gpt-5.4"))
	require.Equal(t, 1.0, resolveGroupModelRateMultiplier(&Group{}, "gpt-5.4"))
	require.Equal(t, 2.0, resolveGroupModelRateMultiplier(&Group{ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: "gpt-5*", Multiplier: 2}}}, "gpt-5.4"))
}

func modelRateTestAPIKey(group *Group) *APIKey {
	groupID := group.ID
	return &APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: group}
}

// requireBreakdownInvariant 钉死 CostBreakdown 不变式：ActualCost = TotalCost × RateMultiplier × ModelRateMultiplier。
func requireBreakdownInvariant(t *testing.T, cost *CostBreakdown, wantRate, wantModelRate float64) {
	t.Helper()
	require.NotNil(t, cost)
	require.Positive(t, cost.TotalCost)
	require.InDelta(t, wantRate, cost.RateMultiplier, 1e-12)
	require.InDelta(t, wantModelRate, cost.ModelRateMultiplier, 1e-12)
	require.InDelta(t, cost.TotalCost*cost.RateMultiplier*cost.ModelRateMultiplier, cost.ActualCost, 1e-12)
}

// TestModelRateMultiplierInvariantAcrossGatewayPaths 覆盖两条网关所有计算 token 费用的分支：
// 每一条都必须经过 CalculateCostUnified，把逐模型因子乘入 ActualCost 并写回 breakdown。
// 任何绕过统一入口的新分支都会让对应子测试的 ModelRateMultiplier 落回 0/1 而失败。
func TestModelRateMultiplierInvariantAcrossGatewayPaths(t *testing.T) {
	billing := newTestBillingService()
	resolver := NewModelPricingResolver(nil, billing)
	group := &Group{
		ID: 7, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true, RateMultiplier: 1.5,
		LongContextPricingEnabled: true,
		ModelRateMultipliers: []GroupModelRateMultiplier{
			{ModelPattern: "claude-opus-*", Multiplier: 2},
			{ModelPattern: "gpt-5.4", Multiplier: 3},
		},
	}
	apiKey := modelRateTestAPIKey(group)
	now := timezone.Now()
	const rate = 0.8

	t.Run("anthropic resolver path", func(t *testing.T) {
		svc := &GatewayService{billingService: billing, resolver: resolver}
		result := &ForwardResult{Usage: ClaudeUsage{InputTokens: 1000, OutputTokens: 500}}
		requireBreakdownInvariant(t, svc.calculateTokenCost(context.Background(), result, apiKey, "claude-opus-4-1", rate, now), rate, 2)
		requireBreakdownInvariant(t, svc.calculateTokenCost(context.Background(), result, apiKey, "claude-sonnet-4", rate, now), rate, 1)
	})

	t.Run("anthropic built-in fallback path without resolver", func(t *testing.T) {
		svc := &GatewayService{billingService: billing}
		result := &ForwardResult{Usage: ClaudeUsage{InputTokens: 1000, OutputTokens: 500}}
		requireBreakdownInvariant(t, svc.calculateTokenCost(context.Background(), result, apiKey, "claude-opus-4-1", rate, now), rate, 2)
	})

	t.Run("openai resolver path", func(t *testing.T) {
		svc := &OpenAIGatewayService{billingService: billing, resolver: resolver}
		tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500}
		cost, err := svc.calculateOpenAIRecordUsageTokenCost(context.Background(), apiKey, "gpt-5.4", rate, now, tokens, "", "", nil)
		require.NoError(t, err)
		requireBreakdownInvariant(t, cost, rate, 3)
		cost, err = svc.calculateOpenAIRecordUsageTokenCost(context.Background(), apiKey, "gpt-5.4-mini", rate, now, tokens, "", "", nil)
		require.NoError(t, err)
		requireBreakdownInvariant(t, cost, rate, 1)
	})

	t.Run("openai built-in fallback path without resolver", func(t *testing.T) {
		svc := &OpenAIGatewayService{billingService: billing}
		tokens := UsageTokens{InputTokens: 1000, OutputTokens: 500}
		cost, err := svc.calculateOpenAIRecordUsageTokenCost(context.Background(), apiKey, "gpt-5.4", rate, now, tokens, "", "", nil)
		require.NoError(t, err)
		requireBreakdownInvariant(t, cost, rate, 3)
	})

	t.Run("per-request billing mode ignores the model factor", func(t *testing.T) {
		price := 0.01
		perRequestGroup := &Group{
			ID: 8, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 1,
			ModelPricing:         []ChannelModelPricing{{Models: []string{"img-model"}, BillingMode: BillingModePerRequest, PerRequestPrice: &price}},
			ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: "img-*", Multiplier: 5}},
		}
		gid := perRequestGroup.ID
		cost, err := billing.CalculateCostUnified(CostInput{
			Ctx: context.Background(), Model: "img-model", GroupID: &gid, Group: perRequestGroup,
			RequestCount: 2, RateMultiplier: rate, Resolver: resolver,
		})
		require.NoError(t, err)
		require.Equal(t, string(BillingModePerRequest), cost.BillingMode)
		requireBreakdownInvariant(t, cost, rate, 1)
	})
}

// TestEffectiveDownstreamMultiplierAgreesWithBilling 钉死利润门/账单口径与扣费口径恒等：
// effectiveDownstreamMultiplier（单函数组合）必须等于两条网关真实扣费得到的 ActualCost/TotalCost。
func TestEffectiveDownstreamMultiplierAgreesWithBilling(t *testing.T) {
	billing := newTestBillingService()
	resolver := NewModelPricingResolver(nil, billing)
	group := &Group{
		ID: 9, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true,
		RateMultiplier: 1.2, SubscriptionType: SubscriptionTypeSubscription,
		PeakRateEnabled: true, PeakStart: "00:00", PeakEnd: "23:59", PeakRateMultiplier: 1.5,
		LongContextPricingEnabled: true,
		ModelRateMultipliers:      []GroupModelRateMultiplier{{ModelPattern: "claude-opus-*", Multiplier: 2}, {ModelPattern: "gpt-*", Multiplier: 0.5}},
	}
	apiKey := modelRateTestAPIKey(group)
	at := time.Date(2026, time.July, 12, 10, 0, 0, 0, timezone.Location())
	const resolvedRate = 0.9

	textMultiplier, _ := computePeakAwareMultipliers(apiKey, resolvedRate, at)

	anthropic := &GatewayService{billingService: billing, resolver: resolver}
	result := &ForwardResult{Usage: ClaudeUsage{InputTokens: 1000, OutputTokens: 500}}
	cost := anthropic.calculateTokenCost(context.Background(), result, apiKey, "claude-opus-4-1", textMultiplier, at)
	require.InDelta(t, effectiveDownstreamMultiplier(group, resolvedRate, at, "claude-opus-4-1"), cost.ActualCost/cost.TotalCost, 1e-9)
	require.InDelta(t, 0.9*1.5*2, cost.ActualCost/cost.TotalCost, 1e-9)

	openai := &OpenAIGatewayService{billingService: billing, resolver: resolver}
	openaiCost, err := openai.calculateOpenAIRecordUsageTokenCost(context.Background(), apiKey, "gpt-5.4", textMultiplier, at, UsageTokens{InputTokens: 1000, OutputTokens: 500}, "", "", nil)
	require.NoError(t, err)
	require.InDelta(t, effectiveDownstreamMultiplier(group, resolvedRate, at, "gpt-5.4"), openaiCost.ActualCost/openaiCost.TotalCost, 1e-9)
	require.InDelta(t, 0.9*1.5*0.5, openaiCost.ActualCost/openaiCost.TotalCost, 1e-9)

	require.InDelta(t, 0.9*1.5, effectiveDownstreamMultiplier(group, resolvedRate, at, ""), 1e-12, "未知模型时逐模型因子按 1")
	require.InDelta(t, EffectiveDownstreamMultiplier(group, resolvedRate, at, "claude-opus-4-1"), effectiveDownstreamMultiplier(group, resolvedRate, at, "claude-opus-4-1"), 0)
}

// TestProfitControlGateIsModelAwareOnBothPaths 钉死两条装门路径对同一输入给出同一阈值，
// 且阈值含逐模型因子：正是本功能瞄准的加价模型，利润门必须按加价后的 D 计算。
func TestProfitControlGateIsModelAwareOnBothPaths(t *testing.T) {
	newGroup := func(platform string) *Group {
		return &Group{
			ID: 31, Platform: platform, Status: StatusActive, Hydrated: true,
			RateMultiplier: 0.5, SubscriptionType: SubscriptionTypeStandard,
			ProfitControlEnabled: true, ProfitMinMargin: 0.2, ProfitSafetyBuffer: 0.05,
			ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: "premium-*", Multiplier: 2}},
		}
	}

	gatewayGroup := newGroup(PlatformAnthropic)
	gatewayCtx := gatewayProfitTestContext(gatewayGroup)
	pricingAt, _ := gatewayTokenRequestPricingAtFromContext(gatewayCtx)
	gatewaySvc := &GatewayService{}
	gatewayGate, _ := gatewaySvc.withGatewayProfitControlGate(gatewayCtx, &gatewayGroup.ID, "premium-model").Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gatewayGate)

	openAIGroup := newGroup(PlatformOpenAI)
	openAICtx := context.WithValue(profitControlTestCtx(openAIGroup), openAIPricingAtCtxKey{}, pricingAt)
	openAIGate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(openAICtx, &openAIGroup.ID, "premium-model")
	require.NotNil(t, openAIGate)

	want := effectiveDownstreamMultiplier(gatewayGroup, 0.5, pricingAt, "premium-model") * (1 - 0.2 - 0.05)
	require.InDelta(t, 0.5*2*0.75, want, 1e-12)
	require.InDelta(t, want, gatewayGate.threshold, 1e-12)
	require.InDelta(t, want, openAIGate.threshold, 1e-12, "两条装门路径必须对同一输入给出同一阈值")
	require.Equal(t, "premium-model", gatewayGate.model)
	require.Equal(t, "premium-model", openAIGate.model)

	plainGate := (&OpenAIGatewayService{}).resolveOpenAIProfitControlGate(openAICtx, &openAIGroup.ID, "plain-model")
	require.InDelta(t, 0.5*0.75, plainGate.threshold, 1e-12, "未命中逐模型倍率的模型按基础 D")

	// 同分组不同模型的既有门不得复用：阈值不同。
	reusedCtx := gatewaySvc.withGatewayProfitControlGate(context.WithValue(gatewayCtx, openAIProfitControlGateCtxKey{}, gatewayGate), &gatewayGroup.ID, "plain-model")
	reusedGate, _ := reusedCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, reusedGate)
	require.NotSame(t, gatewayGate, reusedGate)
	require.InDelta(t, 0.5*0.75, reusedGate.threshold, 1e-12)
	sameCtx := gatewaySvc.withGatewayProfitControlGate(reusedCtx, &gatewayGroup.ID, "plain-model")
	sameGate, _ := sameCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.Same(t, reusedGate, sameGate, "同分组同模型的门在请求内复用")

	// 模型感知的门真实改变准入结论：上游倍率 0.6 在 premium 模型下放行（阈值 0.75），plain 模型下否决（阈值 0.375）。
	account := gatewayProfitTestAccount(1, PlatformAnthropic, 0.6, gatewayGroup.ID)
	require.True(t, gatewaySvc.isGatewayAccountProfitEligible(context.WithValue(gatewayCtx, openAIProfitControlGateCtxKey{}, gatewayGate), &account))
	require.False(t, gatewaySvc.isGatewayAccountProfitEligible(reusedCtx, &account))
}
