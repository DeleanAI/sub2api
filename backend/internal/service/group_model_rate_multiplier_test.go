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

	// 精确命中优先于通配，与上下相邻的逐模型定价编辑器同一条优先级规则
	// （两者共用 selectByModelPattern）。此前倍率是"命中即返回"、定价是"精确优先"，
	// 同一个 claude-opus-4-1 请求在两个编辑器里会得到 2× 和 3× 两个答案。
	entry, ok := group.ModelRateMultiplierFor("claude-opus-4-1")
	require.True(t, ok)
	require.Equal(t, "claude-opus-4-1", entry.ModelPattern, "精确命中优先于通配")
	require.Equal(t, 3.0, entry.Multiplier)

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

	// 与分组逐模型定价共用同一条匹配规则与同一条优先级规则。
	pricingGroup := &Group{ModelPricing: []ChannelModelPricing{{Models: []string{"claude-opus-*"}, BillingMode: BillingModeToken}}}
	require.NotNil(t, matchGroupModelPricing(pricingGroup, "Claude-Opus-4.5"))
	require.Nil(t, matchGroupModelPricing(pricingGroup, "claude-sonnet-4"))

	// 同一份规则表喂给两个编辑器，对同一个模型必须给出同一条结论。
	shared := []string{"claude-*", "claude-haiku-4"}
	multiplierGroup := &Group{ModelRateMultipliers: []GroupModelRateMultiplier{
		{ModelPattern: shared[0], Multiplier: 2},
		{ModelPattern: shared[1], Multiplier: 1},
	}}
	pricingTwo := &Group{ModelPricing: []ChannelModelPricing{
		{Models: []string{shared[0]}, BillingMode: BillingModeToken, PerRequestPrice: floatPtr(2)},
		{Models: []string{shared[1]}, BillingMode: BillingModeToken, PerRequestPrice: floatPtr(1)},
	}}
	multiplierEntry, ok := multiplierGroup.ModelRateMultiplierFor("claude-haiku-4")
	require.True(t, ok)
	pricingEntry := matchGroupModelPricing(pricingTwo, "claude-haiku-4")
	require.NotNil(t, pricingEntry)
	require.Equal(t, multiplierEntry.Multiplier, *pricingEntry.PerRequestPrice,
		"倍率编辑器与定价编辑器对同一模型必须选中同一条规则")
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

	t.Run("per-request billing mode also takes the model factor", func(t *testing.T) {
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
		requireBreakdownInvariant(t, cost, rate, 5)
	})
}

// TestModelRateMultiplierInvariantHoldsOnRecordedCost 钉死"真正落进 usage_logs 的那个
// CostBreakdown"，而不是中间的 token 小计。
//
// 差别是实打实的：web search surcharge 由 CalculateSearchCost 单独算出、走不到
// CalculateCostUnified，一旦直接相加，混合行的 ActualCost 里 token 吃了逐模型因子而
// surcharge 没吃，落账记下的 model_rate_multiplier 就不再描述本行金额；usage_logs 里
// 既没有 search_count 也没有分项成本，这种行事后与纯 token 行完全无法区分。
// 只钉 calculateTokenCost 的测试对此是瞎的——它看不到 surcharge 那一步。
func TestModelRateMultiplierInvariantHoldsOnRecordedCost(t *testing.T) {
	billing := newTestBillingService()
	resolver := NewModelPricingResolver(nil, billing)
	searchPrice := 12.0
	group := &Group{
		ID: 21, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true, RateMultiplier: 1,
		LongContextPricingEnabled: true,
		SearchPricePer1k:          &searchPrice,
		ModelRateMultipliers: []GroupModelRateMultiplier{
			{ModelPattern: "claude-opus-*", Multiplier: 2},
			{ModelPattern: "gpt-5.4", Multiplier: 3},
		},
	}
	apiKey := modelRateTestAPIKey(group)
	now := timezone.Now()
	const rate = 0.8
	ctx := context.Background()

	t.Run("anthropic tokens plus search surcharge", func(t *testing.T) {
		svc := &GatewayService{billingService: billing, resolver: resolver}
		result := &ForwardResult{Usage: ClaudeUsage{InputTokens: 1000, OutputTokens: 500}, SearchCount: 10}
		cost := svc.calculateRecordUsageCost(ctx, result, apiKey, "claude-opus-4-1", rate, rate, now)
		requireBreakdownInvariant(t, cost, rate, 2)

		// surcharge 确实计进来了：否则这条断言与纯 token 行无法区分。
		noSearch := svc.calculateRecordUsageCost(ctx, &ForwardResult{Usage: result.Usage}, apiKey, "claude-opus-4-1", rate, rate, now)
		require.Greater(t, cost.TotalCost, noSearch.TotalCost, "search surcharge 必须叠加在 token 之上")
	})

	t.Run("anthropic search only", func(t *testing.T) {
		svc := &GatewayService{billingService: billing, resolver: resolver}
		result := &ForwardResult{SearchCount: 10}
		cost := svc.calculateRecordUsageCost(ctx, result, apiKey, "claude-opus-4-1", rate, rate, now)
		requireBreakdownInvariant(t, cost, rate, 2)
	})

	t.Run("openai tokens plus search surcharge", func(t *testing.T) {
		svc := &OpenAIGatewayService{billingService: billing, resolver: resolver}
		result := &OpenAIForwardResult{SearchCount: 10}
		cost, err := svc.calculateOpenAIRecordUsageCost(ctx, result, apiKey, []string{"gpt-5.4"},
			rate, rate, rate, rate, UsageTokens{InputTokens: 1000, OutputTokens: 500}, "", nil, now)
		require.NoError(t, err)
		requireBreakdownInvariant(t, cost, rate, 3)
	})

	// 按次类分支（web search 按次、图片、视频、音频）没有一条经过 CalculateCostUnified，
	// 因子与 RateMultiplier 都靠 newPerUnitCost + finalizeRecordedCost 补齐。
	// 遍历 AllBillingModes 并断言"每种计费模式都被产出过"：新增一种模式而忘了接线，
	// 这里当天变红，而不是等到某一行金额对不上。
	t.Run("every billing mode records a self-consistent row", func(t *testing.T) {
		imgPrice, vidPrice, perCall := 0.02, 0.03, 0.04
		modeGroup := &Group{
			ID: 22, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 1,
			ImagePrice1K:              &imgPrice,
			VideoPrice720P:            &vidPrice,
			WebSearchPricePerCall:     &perCall,
			ModelRateMultipliers:      []GroupModelRateMultiplier{{ModelPattern: "*", Multiplier: 4}},
			LongContextPricingEnabled: true,
		}
		modeKey := modelRateTestAPIKey(modeGroup)
		svc := &OpenAIGatewayService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}

		cases := []struct {
			name   string
			result *OpenAIForwardResult
			tokens UsageTokens
		}{
			{"token", &OpenAIForwardResult{}, UsageTokens{InputTokens: 1000, OutputTokens: 500}},
			{"web search per call", &OpenAIForwardResult{WebSearchCalls: 3}, UsageTokens{}},
			{"image", &OpenAIForwardResult{ImageCount: 2}, UsageTokens{}},
			{"video", &OpenAIForwardResult{VideoCount: 1}, UsageTokens{}},
		}
		seen := map[string]bool{}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				models := []string{"gpt-5.4"}
				if tc.name == "video" {
					models = []string{"grok-imagine-video-01"}
				}
				cost, err := svc.calculateOpenAIRecordUsageCost(ctx, tc.result, modeKey, models,
					rate, rate, rate, rate, tc.tokens, "", nil, now)
				require.NoError(t, err)
				require.NotNil(t, cost)
				require.InDelta(t, 4, cost.ModelRateMultiplier, 1e-12, "逐模型因子必须记在行上")
				require.InDelta(t, cost.TotalCost*cost.RateMultiplier*cost.ModelRateMultiplier, cost.ActualCost, 1e-12)
				require.Positive(t, cost.TotalCost)
				seen[cost.BillingMode] = true
			})
		}
		for _, mode := range AllBillingModes {
			require.True(t, seen[string(mode)], "计费模式 %s 没有被落账不变式覆盖：新增模式必须在这里加一条用例", mode)
		}
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

// TestProfitControlGateResolvesCompositeModelOnBothPaths 钉死 composite 分组下两条装门
// 路径解析出同一个模型。
//
// 这条护栏是必要的，因为"两条路径给同一输入同一阈值"的测试对本缺陷是瞎的：两条调度
// 入口曾经一个在 composite 解析之后装门（拿到上游模型）、另一个在解析之前装门
// （拿到客户端别名），同一分组同一请求走哪条入口，阈值就不一样。把别名和上游模型
// 都喂进去、断言门取的是上游模型，才能看见这个差异。
func TestProfitControlGateResolvesCompositeModelOnBothPaths(t *testing.T) {
	newGroup := func(platform string) *Group {
		return &Group{
			ID: 32, Platform: platform, Status: StatusActive, Hydrated: true,
			RateMultiplier: 0.5, SubscriptionType: SubscriptionTypeStandard,
			ProfitControlEnabled: true, ProfitMinMargin: 0.2, ProfitSafetyBuffer: 0.05,
			ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: "premium-*", Multiplier: 2}},
		}
	}
	const publicAlias = "plain-alias"   // 客户端请求的公开别名，未命中逐模型倍率
	const upstreamModel = "premium-max" // composite 真正转发/计费的上游模型，倍率 2×

	gatewayGroup := newGroup(PlatformAnthropic)
	baseCtx := gatewayProfitTestContext(gatewayGroup)
	pricingAt, _ := gatewayTokenRequestPricingAtFromContext(baseCtx)
	compositeCtx := WithCompositeRouteDecision(baseCtx, CompositeRouteDecision{
		Matched: true, TargetPlatform: PlatformAnthropic, UpstreamModel: upstreamModel, PublicModel: publicAlias,
	})

	gatewayGate, _ := (&GatewayService{}).withGatewayProfitControlGate(compositeCtx, &gatewayGroup.ID, publicAlias).
		Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, gatewayGate)

	openAIGroup := newGroup(PlatformOpenAI)
	openAICompositeCtx := WithCompositeRouteDecision(
		context.WithValue(profitControlTestCtx(openAIGroup), openAIPricingAtCtxKey{}, pricingAt),
		CompositeRouteDecision{Matched: true, TargetPlatform: PlatformOpenAI, UpstreamModel: upstreamModel, PublicModel: publicAlias},
	)
	openAIGateCtx := (&OpenAIGatewayService{}).withOpenAIProfitControlGate(openAICompositeCtx, &openAIGroup.ID, publicAlias)
	openAIGate, _ := openAIGateCtx.Value(openAIProfitControlGateCtxKey{}).(*openAIProfitControlGate)
	require.NotNil(t, openAIGate)

	require.Equal(t, upstreamModel, gatewayGate.model, "门必须按 composite 真正转发的上游模型解析倍率")
	require.Equal(t, upstreamModel, openAIGate.model)
	want := 0.5 * 2 * (1 - 0.2 - 0.05)
	require.InDelta(t, want, gatewayGate.threshold, 1e-12)
	require.InDelta(t, want, openAIGate.threshold, 1e-12, "两条装门路径对同一 composite 请求必须给出同一阈值")

	// 按别名解析会得到 0.375，正是本缺陷放行 0.6× 上游的那个阈值。
	require.Greater(t, gatewayGate.threshold, 0.5*0.75+1e-9)
}

// TestNormalizeGroupModelRateMultipliersRejectsUnhonourablePatterns 钉死"保存时接受的
// 模式，匹配器必须真的能兑现"。
//
// matchModelPattern 只支持末尾一个 *，而校验从前只看非空：claude-*-thinking 能保存
// 成功、在编辑器里预览成 2.0×、还会出现在 /v1/sub2api/billing 的响应里，然后永远
// 不匹配任何模型。保存成功 + 界面确认 + 实际不生效，是最难被发现的一类错。
func TestNormalizeGroupModelRateMultipliersRejectsUnhonourablePatterns(t *testing.T) {
	rejected := []string{"claude-*-thinking", "*-thinking", "*claude*", "cl*ude-*", "**"}
	for _, pattern := range rejected {
		t.Run(pattern, func(t *testing.T) {
			_, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{{ModelPattern: pattern, Multiplier: 2}})
			require.Errorf(t, err, "%q 永远匹配不到任何模型，不能让它保存成功", pattern)

			// 若真被放行，它对任何模型都不会命中——这正是"保存了却不生效"的形状。
			group := &Group{ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: pattern, Multiplier: 2}}}
			for _, model := range []string{"claude-opus-4-1-thinking", "claude-3-thinking", "claude-haiku-4"} {
				_, ok := group.ModelRateMultiplierFor(model)
				require.Falsef(t, ok, "%q 竟然匹配到了 %q：匹配器和校验的口径又分叉了", pattern, model)
			}
		})
	}

	accepted := []string{"claude-opus-*", "claude-opus-4-1", "*"}
	for _, pattern := range accepted {
		t.Run("accepted/"+pattern, func(t *testing.T) {
			out, err := NormalizeGroupModelRateMultipliers([]GroupModelRateMultiplier{{ModelPattern: pattern, Multiplier: 2}})
			require.NoError(t, err)
			require.Len(t, out, 1)
		})
	}
}
