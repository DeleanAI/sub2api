//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type settlementMemStore struct{ due map[string]time.Time }

func (m *settlementMemStore) ScheduleSeedanceSettlement(_ context.Context, member string, dueAt time.Time) error {
	m.due[member] = dueAt
	return nil
}

func (m *settlementMemStore) DueSeedanceSettlements(_ context.Context, now time.Time, limit int) ([]string, error) {
	var members []string
	for member, dueAt := range m.due {
		if !dueAt.After(now) {
			members = append(members, member)
		}
	}
	sort.Strings(members)
	if len(members) > limit {
		members = members[:limit]
	}
	return members, nil
}

func (m *settlementMemStore) SeedanceSettlementPending(_ context.Context, member string) (bool, error) {
	_, ok := m.due[member]
	return ok, nil
}

func (m *settlementMemStore) RemoveSeedanceSettlement(_ context.Context, member string) error {
	delete(m.due, member)
	return nil
}

type settlementCache struct {
	GatewayCache
	pending  map[string][]byte
	billed   map[string]bool
	claimErr error
}

func (c *settlementCache) SetGrokVideoPendingBilling(_ context.Context, key string, payload []byte, _ time.Duration) error {
	c.pending[key] = payload
	return nil
}

func (c *settlementCache) GetGrokVideoPendingBilling(_ context.Context, key string) ([]byte, error) {
	return c.pending[key], nil
}

func (c *settlementCache) ClaimGrokVideoBilled(_ context.Context, key string, _ time.Duration) (bool, error) {
	if c.claimErr != nil {
		return false, c.claimErr
	}
	if c.billed[key] {
		return false, nil
	}
	c.billed[key] = true
	return true, nil
}

func (c *settlementCache) ReleaseGrokVideoBilled(_ context.Context, key string) error {
	delete(c.billed, key)
	return nil
}

type settlementAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *settlementAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account != nil && r.account.ID == id {
		return r.account, nil
	}
	return nil, errors.New("not found")
}

type settlementFixture struct {
	svc      *SeedanceSettlementService
	gateway  *OpenAIGatewayService
	store    *settlementMemStore
	cache    *settlementCache
	upstream *grokMediaContentUpstreamStub
	entry    SeedanceSettlementEntry
	member   string
	apiKey   *APIKey
	recorded []*OpenAIForwardResult
	recordFn func() error
}

func newSettlementFixture(t *testing.T, createdAt time.Time) *settlementFixture {
	t.Helper()
	f := &settlementFixture{
		store:    &settlementMemStore{due: map[string]time.Time{}},
		cache:    &settlementCache{pending: map[string][]byte{}, billed: map[string]bool{}},
		upstream: &grokMediaContentUpstreamStub{},
		entry:    SeedanceSettlementEntry{TaskKey: SeedanceTaskKey("cgt-1"), UserID: 10, APIKeyID: 20, GroupID: 24},
	}
	account := seedanceTestAccount()
	gateway := &OpenAIGatewayService{cache: f.cache, httpUpstream: f.upstream}
	f.gateway = gateway
	groupID := f.entry.GroupID
	f.apiKey = &APIKey{ID: f.entry.APIKeyID, UserID: f.entry.UserID, GroupID: &groupID,
		Group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 1}}
	f.svc = NewSeedanceSettlementService(f.store, gateway, &settlementAccountRepo{account: account}, nil, nil, nil)
	gateway.AttachSeedanceSettlement(f.svc)
	f.svc.loadAPIKey = func(context.Context, SeedanceSettlementEntry) (*APIKey, error) { return f.apiKey, nil }
	f.svc.recordUsage = func(_ context.Context, _ SeedanceSettlementEntry, _ *Account, _ *APIKey, bill *OpenAIForwardResult, _ *GrokVideoPendingBilling) error {
		if f.recordFn != nil {
			if err := f.recordFn(); err != nil {
				return err
			}
		}
		f.recorded = append(f.recorded, bill)
		return nil
	}
	require.NoError(t, gateway.StoreGrokVideoPendingBilling(context.Background(), f.entry.TaskKey, 10, 20, GrokVideoPendingBilling{
		Model: "seedance-video", BillingModel: "seedance-video", UpstreamModel: "ep-seedance",
		AccountID: account.ID, QuotaPlatform: PlatformOpenAI, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
	}))
	gateway.TrackSeedanceTask(context.Background(), f.entry)
	member, err := f.entry.member()
	require.NoError(t, err)
	f.member = member
	return f
}

func (f *settlementFixture) respond(status int, body string) {
	resp := grokMediaContentStatusResponse(body)
	resp.StatusCode = status
	f.upstream.responses = append(f.upstream.responses, resp)
}

func (f *settlementFixture) pending() bool {
	_, ok := f.store.due[f.member]
	return ok
}

// 调用方从不来查（例如用了 callback_url）：网关后台补查到成功就按上游 usage 计费，且只计一次。
func TestSeedanceSettlementBillsTasksTheClientNeverPolls(t *testing.T) {
	f := newSettlementFixture(t, time.Now().Add(-15*time.Minute))
	require.True(t, f.pending(), "创建即登记")
	require.WithinDuration(t, time.Now().Add(seedanceSettlementFirstCheck), f.store.due[f.member], 5*time.Second)

	f.store.due[f.member] = time.Now()
	f.respond(http.StatusOK, `{"id":"cgt-1","status":"succeeded","model":"doubao-seedance-2-0","usage":{"completion_tokens":40594}}`)
	f.svc.scanOnce(context.Background())

	require.Len(t, f.recorded, 1)
	require.Equal(t, 40594, f.recorded[0].Usage.OutputTokens)
	require.Equal(t, "seedance-video", f.recorded[0].BillingModel, "按创建时调用方用的模型名计价")
	require.Equal(t, StableGrokVideoBillingRequestID(f.entry.TaskKey), f.recorded[0].RequestID)
	require.False(t, f.pending(), "结清后移出索引")
	require.Equal(t, "/api/v3/contents/generations/tasks/cgt-1", f.upstream.request.URL.Path)

	// 同一任务再被观察到成功（例如调用方随后来查）：抢不到标记，不重复计费。
	f.respond(http.StatusOK, `{"id":"cgt-1","status":"succeeded","usage":{"completion_tokens":40594}}`)
	f.store.due[f.member] = time.Now()
	f.svc.scanOnce(context.Background())
	require.Len(t, f.recorded, 1)
	require.False(t, f.pending())
}

func TestSeedanceSettlementOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		recordErr   error
		wantOutcome SeedanceBillingOutcome
		wantPending bool
		wantRecords int
	}{
		{name: "still running", status: http.StatusOK, body: `{"status":"running"}`, wantOutcome: SeedanceBillingPending, wantPending: true},
		{name: "failed", status: http.StatusOK, body: `{"status":"failed","error":{"code":"x"}}`, wantOutcome: SeedanceBillingSettled},
		{name: "expired", status: http.StatusOK, body: `{"status":"expired"}`, wantOutcome: SeedanceBillingSettled},
		{name: "record gone", status: http.StatusNotFound, body: `{"error":{"code":"NotFound"}}`, wantOutcome: SeedanceBillingUnbillable},
		{name: "upstream 5xx", status: http.StatusBadGateway, body: `{}`, wantOutcome: SeedanceBillingRetry, wantPending: true},
		{name: "succeeded without usage", status: http.StatusOK, body: `{"status":"succeeded"}`, wantOutcome: SeedanceBillingUnbillable},
		{name: "record fails", status: http.StatusOK, body: `{"status":"succeeded","usage":{"completion_tokens":7}}`, recordErr: errors.New("db down"), wantOutcome: SeedanceBillingRetry, wantPending: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSettlementFixture(t, time.Now().Add(-30*time.Minute))
			f.respond(tc.status, tc.body)
			if tc.recordErr != nil {
				f.recordFn = func() error { return tc.recordErr }
			}
			require.Equal(t, tc.wantOutcome, f.svc.settle(context.Background(), f.entry))
			require.Equal(t, tc.wantPending, f.pending())
			require.Len(t, f.recorded, tc.wantRecords)
			if tc.wantPending {
				require.True(t, f.store.due[f.member].After(time.Now()), "改期到将来")
			}
			if tc.recordErr != nil {
				require.Empty(t, f.cache.billed, "落账失败必须释放计费标记，下次才能重试")
			}
		})
	}
}

func TestSeedanceSettlementGivesUpAfterRetention(t *testing.T) {
	f := newSettlementFixture(t, time.Now().Add(-seedanceTaskRetention-time.Hour))
	require.Equal(t, SeedanceBillingUnbillable, f.svc.settle(context.Background(), f.entry))
	require.False(t, f.pending())
	require.Empty(t, f.upstream.requests, "超过方舟的任务保留期不再查询上游")
}

// 删除前结清：回调拿到视频后立刻删任务，否则任务记录就查不到了。已结清的任务不多查一次上游。
func TestSeedanceSettleBeforeDelete(t *testing.T) {
	f := newSettlementFixture(t, time.Now().Add(-time.Minute))
	f.respond(http.StatusOK, `{"status":"succeeded","usage":{"completion_tokens":11}}`)
	f.svc.gateway.SettleSeedanceTaskBeforeDelete(context.Background(), f.entry, seedanceTestAccount())
	require.Len(t, f.recorded, 1)
	require.False(t, f.pending())

	f.svc.gateway.SettleSeedanceTaskBeforeDelete(context.Background(), f.entry, seedanceTestAccount())
	require.Len(t, f.upstream.requests, 1, "不在索引里的任务不再查询上游")
}

func TestSeedanceSettlementEntryMemberIsStableIdentity(t *testing.T) {
	entry := SeedanceSettlementEntry{TaskKey: SeedanceTaskKey("cgt-9"), UserID: 1, APIKeyID: 2, GroupID: 3}
	a, err := entry.member()
	require.NoError(t, err)
	b, err := entry.member()
	require.NoError(t, err)
	require.Equal(t, a, b)
	parsed, err := parseSeedanceSettlementMember(a)
	require.NoError(t, err)
	require.Equal(t, entry, parsed)
	for _, bad := range []SeedanceSettlementEntry{{TaskKey: "cgt-9", UserID: 1, APIKeyID: 2}, {TaskKey: SeedanceTaskKey("x"), APIKeyID: 2}} {
		_, err := bad.member()
		require.Error(t, err)
	}
	raw, _ := json.Marshal(map[string]any{"t": "grok-1", "u": 1, "k": 2})
	_, err = parseSeedanceSettlementMember(string(raw))
	require.Error(t, err)
}

func TestSeedanceSettlementBackoff(t *testing.T) {
	now := time.Now()
	at := func(age time.Duration) time.Duration {
		pending := &GrokVideoPendingBilling{CreatedAt: now.Add(-age).UTC().Format(time.RFC3339Nano)}
		return nextSeedanceSettlementCheck(now, pending).Sub(now)
	}
	require.Equal(t, seedanceSettlementFirstCheck, nextSeedanceSettlementCheck(now, nil).Sub(now))
	require.Equal(t, seedanceSettlementFirstCheck, at(10*time.Minute).Round(time.Second))
	require.Equal(t, 30*time.Minute, at(time.Hour).Round(time.Second))
	require.Equal(t, seedanceSettlementMaxInterval, at(10*time.Hour).Round(time.Second))
}

// 创建路径与后台结算用的是同一次上游调用：查询不写任何 HTTP 响应。
func TestSeedanceQueryTaskUsesSharedUpstreamCall(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"status":"succeeded","model":"m","usage":{"completion_tokens":3}}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	status, parsed, err := svc.querySeedanceTask(context.Background(), seedanceTestAccount(), SeedanceTaskKey("cgt-1"))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, seedanceTaskStatus{Status: "succeeded", Model: "m", CompletionTokens: 3}, parsed)
	require.Equal(t, "Bearer ark-secret", upstream.request.Header.Get("Authorization"))
}

// 后台补查同样按计价档计费：分辨率取补查时上游报告的值，「含输入视频」取创建时的快照。
func TestSeedanceSettlementBillsThePriceTier(t *testing.T) {
	f := newSettlementFixture(t, time.Now().Add(-20*time.Minute))
	billing := newTestBillingService()
	f.gateway.billingService, f.gateway.resolver = billing, NewModelPricingResolver(nil, billing)
	f.apiKey.Group = seedanceVariantTestGroup(true)
	require.NoError(t, f.gateway.StoreGrokVideoPendingBilling(context.Background(), f.entry.TaskKey, f.entry.UserID, f.entry.APIKeyID, GrokVideoPendingBilling{
		Model: seedanceVariantTestModel, BillingModel: seedanceVariantTestModel, AccountID: seedanceTestAccount().ID,
		QuotaPlatform: PlatformOpenAI, CreatedAt: time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339Nano), SeedanceInputVideo: true,
	}))
	f.store.due[f.member] = time.Now()
	f.respond(http.StatusOK, `{"id":"cgt-1","status":"succeeded","resolution":"1080p","draft":false,"usage":{"completion_tokens":1000}}`)
	f.svc.scanOnce(context.Background())

	require.Len(t, f.recorded, 1)
	require.Equal(t, seedanceVariantTestModel+"@1080p+video", f.recorded[0].BillingModel)
	require.Equal(t, seedanceVariantTestModel+"@1080p+video", f.recorded[0].Model)
	require.False(t, f.pending())
}

// 后台补查落的用量行：model 是计价档，请求模型是客户端创建任务时请求的模型（走真实的 RecordUsage）。
func TestSeedanceSettlementRecordsTheRequestedModelWithoutTheTier(t *testing.T) {
	for _, tc := range []struct {
		name          string
		originalModel string
		wantRequested string
	}{
		{"client used a public alias", "seedance-video", "seedance-video"},
		{"snapshot without the client's model", "", seedanceVariantTestModel},
	} {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		gateway := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
		svc := &SeedanceSettlementService{gateway: gateway}
		group := seedanceVariantTestGroup(true)
		key := &APIKey{ID: 8, GroupID: &group.ID, Group: group, User: &User{ID: 3}}
		bill := &OpenAIForwardResult{Model: seedanceVariantTestModel + "@480p", BillingModel: seedanceVariantTestModel + "@480p",
			RequestID: StableGrokVideoBillingRequestID(SeedanceTaskKey("cgt-1")), VideoResolution: "480p"}
		bill.Usage.OutputTokens = 1000
		pending := &GrokVideoPendingBilling{Model: seedanceVariantTestModel, OriginalModel: tc.originalModel, QuotaPlatform: PlatformOpenAI}

		require.NoError(t, svc.record(context.Background(), SeedanceSettlementEntry{TaskKey: SeedanceTaskKey("cgt-1"), UserID: 3, APIKeyID: 8},
			&Account{ID: 4, Platform: PlatformOpenAI}, key, bill, pending), tc.name)
		require.NotNil(t, usageRepo.lastLog, tc.name)
		require.Equal(t, seedanceVariantTestModel+"@480p", usageRepo.lastLog.Model, tc.name)
		require.Equal(t, tc.wantRequested, usageRepo.lastLog.RequestedModel, tc.name)
	}
}
