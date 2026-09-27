//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const seedanceVariantTestModel = "doubao-seedance-2-5-260628"

// seedanceVariantTestGroup 按方舟模型价格文档（2026-09-24 版）配 Seedance 2.5 的分档价（元/百万 token，站内 1 单位 = 1 元），
// 与 edge 上「Seedance 2.5（百度）」分组的配置同形：模型本身一条兜底价，加四个计价档。
func seedanceVariantTestGroup(withVariants bool) *Group {
	perM := func(v float64) *float64 { p := v / 1e6; return &p }
	zero := 0.0
	entry := func(price float64, models ...string) ChannelModelPricing {
		return ChannelModelPricing{Platform: PlatformOpenAI, BillingMode: BillingModeToken, Models: models, InputPrice: &zero, OutputPrice: perM(price)}
	}
	pricing := []ChannelModelPricing{entry(70, seedanceVariantTestModel)}
	if withVariants {
		m := seedanceVariantTestModel
		pricing = append(pricing,
			entry(70, m+"@480p", m+"@720p"),
			entry(42, m+"@480p+video", m+"@720p+video"),
			entry(77, m+"@1080p"),
			entry(46, m+"@1080p+video"),
		)
	}
	return &Group{ID: 6, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 1, ModelPricing: pricing}
}

func seedanceVariantTestKey(group *Group) *APIKey {
	return &APIKey{ID: 8, UserID: 3, GroupID: &group.ID, Group: group}
}

func TestBillingVariantModelRoundTrip(t *testing.T) {
	require.Equal(t, "m@720p+video", BillingVariantModel("m", "720p+video"))
	require.Equal(t, "m", BillingVariantModel("m", " "))
	base, ok := billingVariantBase("m@720p+video")
	require.True(t, ok)
	require.Equal(t, "m", base)
	for _, notVariant := range []string{"m", "m@", "@720p", ""} {
		_, ok := billingVariantBase(notVariant)
		require.False(t, ok, notVariant)
	}
}

// 计价档来自方舟的两个维度：输出分辨率（上游报告的最终值，样片按 480p）× 输入是否包含视频。
func TestSeedanceBillingVariantFollowsOfficialDimensions(t *testing.T) {
	require.Equal(t, "720p", seedanceBillingVariant("720p", false))
	require.Equal(t, "1080p+video", seedanceBillingVariant(" 1080P ", true))
	require.Empty(t, seedanceBillingVariant("", true), "分辨率未知时不分档")

	status := parseSeedanceTaskStatus([]byte(`{"status":"succeeded","resolution":"1080p","draft":false,"usage":{"completion_tokens":9}}`))
	require.Equal(t, "1080p", status.billingResolution())
	draft := parseSeedanceTaskStatus([]byte(`{"status":"succeeded","resolution":"720p","draft":true}`))
	require.Equal(t, "480p", draft.billingResolution(), "样片（Step 1）按 480p 计费")
	require.Empty(t, parseSeedanceTaskStatus([]byte(`{"status":"succeeded"}`)).billingResolution())

	info, err := ParseSeedanceRequest([]byte(`{"model":"m","content":[{"type":"text","text":"x"},
		{"type":"video_url","video_url":{"url":"https://v/1.mp4"},"role":"reference_video"},
		{"type":"image_url","image_url":{"url":"https://i/1.png"}}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"https://v/1.mp4"}, info.InputVideoURLs)
}

// 端到端：成功任务按它的计价档取价，再乘调用方的倍率；用量行的 model 写成档位名。
func TestSeedanceBillsTheOfficialPriceTier(t *testing.T) {
	billing := newTestBillingService()
	gateway := &OpenAIGatewayService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
	key := seedanceVariantTestKey(seedanceVariantTestGroup(true))
	const tokens, rate = 108900, 0.8

	cases := []struct {
		resolution string
		inputVideo bool
		wantModel  string
		perMillion float64
	}{
		{"480p", false, seedanceVariantTestModel + "@480p", 70},
		{"720p", false, seedanceVariantTestModel + "@720p", 70},
		{"720p", true, seedanceVariantTestModel + "@720p+video", 42},
		{"1080p", false, seedanceVariantTestModel + "@1080p", 77},
		{"1080p", true, seedanceVariantTestModel + "@1080p+video", 46},
	}
	for _, tc := range cases {
		bill := &OpenAIForwardResult{Model: seedanceVariantTestModel, BillingModel: seedanceVariantTestModel, VideoResolution: tc.resolution}
		bill.Usage.OutputTokens = tokens
		gateway.applySeedanceBillingVariant(context.Background(), zap.NewNop(), key, bill, &GrokVideoPendingBilling{SeedanceInputVideo: tc.inputVideo})
		require.Equal(t, tc.wantModel, bill.Model)
		require.Equal(t, tc.wantModel, bill.BillingModel)

		// 与 RecordUsage 同一条成本路径：候选里档位名在前、模型本身在后。
		candidates := usageBillingModelCandidates(bill.BillingModel, seedanceVariantTestModel)
		cost, err := gateway.calculateOpenAIRecordUsageCost(context.Background(), bill, key, candidates, rate, rate, rate, rate,
			UsageTokens{OutputTokens: tokens}, "", nil, time.Now())
		require.NoError(t, err)
		require.InDelta(t, tokens*tc.perMillion/1e6, cost.TotalCost, 1e-9, tc.wantModel)
		require.InDelta(t, tokens*tc.perMillion/1e6*rate, cost.ActualCost, 1e-9, tc.wantModel)
	}
}

// 分档价漏配、或上游没报分辨率时，按模型本身的价格计——并且留告警，不静默。
func TestSeedanceBillingVariantFallsBackLoudly(t *testing.T) {
	billing := newTestBillingService()
	gateway := &OpenAIGatewayService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}
	core, logs := observer.New(zapcore.WarnLevel)

	unpriced := &OpenAIForwardResult{Model: seedanceVariantTestModel, BillingModel: seedanceVariantTestModel, VideoResolution: "720p"}
	gateway.applySeedanceBillingVariant(context.Background(), zap.New(core), seedanceVariantTestKey(seedanceVariantTestGroup(false)), unpriced, nil)
	require.Equal(t, seedanceVariantTestModel, unpriced.BillingModel)
	require.Equal(t, 1, logs.FilterMessage("seedance.billing_variant_unpriced").Len())

	unknown := &OpenAIForwardResult{Model: seedanceVariantTestModel, BillingModel: seedanceVariantTestModel}
	gateway.applySeedanceBillingVariant(context.Background(), zap.New(core), seedanceVariantTestKey(seedanceVariantTestGroup(true)), unknown, nil)
	require.Equal(t, seedanceVariantTestModel, unknown.BillingModel)
	require.Equal(t, 1, logs.FilterMessage("seedance.billing_variant_unknown").Len())
}

// 基于样片生成正式视频：单价按样片那一步是否含输入视频定（方舟样片模式规则），读不到样片快照时按本次内容并告警。
func TestSeedanceInputVideoFollowsTheDraftStep(t *testing.T) {
	cache := &settlementCache{pending: map[string][]byte{}, billed: map[string]bool{}}
	gateway := &OpenAIGatewayService{cache: cache}
	draftKey := SeedanceTaskKey("cgt-draft")
	require.NoError(t, gateway.StoreGrokVideoPendingBilling(context.Background(), draftKey, 3, 8,
		GrokVideoPendingBilling{Model: seedanceVariantTestModel, SeedanceInputVideo: true}))
	core, logs := observer.New(zapcore.WarnLevel)
	log := zap.New(core)

	require.True(t, gateway.SeedanceInputVideo(context.Background(), log, GrokMediaRequestInfo{InputVideoURLs: []string{"https://v/1.mp4"}}, 3, 8))
	require.False(t, gateway.SeedanceInputVideo(context.Background(), log, GrokMediaRequestInfo{}, 3, 8))
	require.True(t, gateway.SeedanceInputVideo(context.Background(), log, GrokMediaRequestInfo{ReferencedTaskKeys: []string{draftKey}}, 3, 8),
		"Step 2 沿用 Step 1 的「含输入视频」")
	require.Zero(t, logs.Len())

	require.False(t, gateway.SeedanceInputVideo(context.Background(), log, GrokMediaRequestInfo{ReferencedTaskKeys: []string{SeedanceTaskKey("cgt-gone")}}, 3, 8))
	require.Equal(t, 1, logs.FilterMessage("seedance.draft_snapshot_missing").Len())
}

// 为模型配的逐模型倍率覆盖它的所有计费变体；变体自己的规则优先；非变体名不受影响。
func TestGroupModelRateMultiplierCoversBillingVariants(t *testing.T) {
	variant := seedanceVariantTestModel + "@720p+video"
	group := &Group{ID: 1, ModelRateMultipliers: []GroupModelRateMultiplier{{ModelPattern: seedanceVariantTestModel, Multiplier: 2}}}
	require.Equal(t, 2.0, resolveGroupModelRateMultiplier(group, variant))
	require.Equal(t, 2.0, resolveGroupModelRateMultiplier(group, seedanceVariantTestModel))

	group.ModelRateMultipliers = append([]GroupModelRateMultiplier{{ModelPattern: variant, Multiplier: 3}}, group.ModelRateMultipliers...)
	require.Equal(t, 3.0, resolveGroupModelRateMultiplier(group, variant))
	require.Equal(t, 1.0, resolveGroupModelRateMultiplier(group, "gpt-5.5"))
}
