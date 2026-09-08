//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// qwenTokenPlanRepo records the durable state changes made when an inference
// response proves that a one-time Token Plan allowance is exhausted.
type qwenTokenPlanRepo struct {
	AccountRepository
	extraWrites      []map[string]any
	updateExtraErr   error
	schedulableCalls []bool
	setScheduleErr   error
	rateLimitCalls   int
}

func (r *qwenTokenPlanRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	copyOfUpdates := make(map[string]any, len(updates))
	for key, value := range updates {
		copyOfUpdates[key] = value
	}
	r.extraWrites = append(r.extraWrites, copyOfUpdates)
	return r.updateExtraErr
}

func (r *qwenTokenPlanRepo) SetSchedulable(_ context.Context, _ int64, schedulable bool) error {
	r.schedulableCalls = append(r.schedulableCalls, schedulable)
	return r.setScheduleErr
}

func (r *qwenTokenPlanRepo) SetRateLimited(_ context.Context, _ int64, _ time.Time) error {
	r.rateLimitCalls++
	return nil
}

// newQwenTokenPlanAccountExhaustedSince 造一个"从 since 起就一直在报额度耗尽"的账号。
// 永久停调的判据是这个状态熬过了观察窗，所以想测永久停调必须先有起点。
func newQwenTokenPlanAccountExhaustedSince(since time.Time) *Account {
	account := newQwenTokenPlanAccount()
	account.Extra = map[string]any{
		qwenTokenPlanExhaustedSinceExtraKey: since.UTC().Format(time.RFC3339),
	}
	return account
}

func newQwenTokenPlanAccount() *Account {
	return &Account{
		ID:       771,
		Platform: PlatformQwen,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"account_mode": AccountModeTokenPlan,
			"api_key":      "qwen-token-plan-key",
		},
	}
}

func TestQwenTokenPlanQuotaExhausted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			// token-plan + exhausted：生产实测的唯一形态（14206 次命中）。
			name: "documented one week message",
			body: `{"error":{"message":"Your token-plan 1-week quota has been exhausted."}}`,
			want: true,
		},
		{
			// 同一句话但带了重置时间 → 说明会恢复。生产里 1308「已达到 5 小时的使用
			// 上限。您的限额将在 … 重置。」正是这种高频的临时窗口。
			name: "token-plan wording that states a reset time is transient",
			body: `{"error":{"message":"Your token-plan 1-week quota has been exhausted. The quota will reset at 2026-09-14T00:00:00Z."}}`,
			want: false,
		},
		{
			// 说了 will reset = 会恢复，不能永久停调。
			name: "json field wording variant",
			body: `{"message":"weekly quota exceeded; will reset at 2026-09-14T00:00:00Z"}`,
			want: false,
		},
		{
			// 阿里云文档：Throttling.AllocationQuota 是 TPS/TPM 消耗超限，提额或降频即恢复
			// （真正的当月耗尽是 "month allocated quota exceeded"）。
			// 报文自己就写着 please increase your quota limit——这是可恢复的。
			name: "allocated quota exceeded without reset",
			body: `{"error":{"code":"Throttling.AllocationQuota","message":"Allocated quota exceeded, please increase your quota limit."}}`,
			want: false,
		},
		{
			// 文档里语义无歧义的当月耗尽，保留为永久。
			name: "month allocated quota exceeded is real exhaustion",
			body: `{"error":{"code":"Throttling.AllocationQuota","message":"month allocated quota exceeded"}}`,
			want: true,
		},
		{
			// 文档把 insufficient_quota 归在 Throttling.AllocationQuota 同一条下（TPM 限流）。
			name: "insufficient_quota plan wording",
			body: `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}`,
			want: false,
		},
		{
			// 裸 "quota exhausted" 没有任何 Token Plan 证据，生产里也从未出现。
			// 这类兜底措辞正是把临时限流误判成永久的来源。
			name: "one-time quota exhausted without reset",
			body: `{"error":{"message":"quota exhausted"}}`,
			want: false,
		},
		{
			// 中文分支删掉了：生产里中文 429 全是限流（1302 速率限制、1308 五小时窗口
			// 上限），一条中文额度耗尽都没有过，而按关键词猜会把 1308 这种带重置时间的
			// 窗口限额判成永久。
			name: "chinese one-time exhaustion",
			body: `{"error":{"message":"套餐额度已用尽"}}`,
			want: false,
		},
		{
			name: "generic rate limit",
			body: `{"error":{"message":"rate limit exceeded"}}`,
			want: false,
		},
		{
			name: "requests rate limit exceeded",
			body: `{"error":{"message":"Requests rate limit exceeded, please try again later."}}`,
			want: false,
		},
		{
			name: "api-key requests rate limit",
			body: `{"error":{"message":"API-Key Requests rate limit exceeded"}}`,
			want: false,
		},
		{
			name: "request surge protection",
			body: `{"error":{"message":"Request rate increased too quickly"}}`,
			want: false,
		},
		{
			name: "reset message without exhaustion",
			body: `{"error":{"message":"quota will reset at 2026-09-14T00:00:00Z"}}`,
			want: false,
		},
		{
			name: "authentication failure",
			body: `{"error":{"message":"invalid api key"}}`,
			want: false,
		},
		{
			name: "transport-like text",
			body: `upstream connection reset`,
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, qwenTokenPlanQuotaExhausted([]byte(tt.body)))
		})
	}
}

// 第一次看到额度耗尽：只记起点 + 临时冷却，不停调度。
// 生产实测 25/121 的账号在平均 48 秒内自行恢复，看一次报文就永久停调会误伤它们。
func TestHandle429_QwenTokenPlanFirstHitOnlyObserves(t *testing.T) {
	repo := &qwenTokenPlanRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := newQwenTokenPlanAccount()

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"Your token-plan quota has been exhausted."}}`))

	require.Empty(t, repo.schedulableCalls, "观察期内不得关闭调度")
	require.Equal(t, 1, repo.rateLimitCalls, "观察期内应临时冷却，让调度器窗口结束后再试")
	require.Len(t, repo.extraWrites, 1)
	require.NotEmpty(t, repo.extraWrites[0][qwenTokenPlanExhaustedSinceExtraKey], "必须记下观察起点")
	require.Nil(t, repo.extraWrites[0][qwenTokenPlanExhaustedExtraKey], "观察期内还不能标记为已耗尽")
}

// 熬过观察窗仍在报：判定为真耗尽，关闭调度。
func TestHandle429_QwenTokenPlanExhaustionDisablesScheduling(t *testing.T) {
	repo := &qwenTokenPlanRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := newQwenTokenPlanAccountExhaustedSince(time.Now().Add(-qwenTokenPlanExhaustionConfirmWindow - time.Minute))

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"Your token-plan quota has been exhausted."}}`))

	require.Equal(t, []bool{false}, repo.schedulableCalls)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, true, repo.extraWrites[0][qwenTokenPlanExhaustedExtraKey])
	require.NotEmpty(t, repo.extraWrites[0][qwenTokenPlanExhaustedAtExtraKey])
	require.Contains(t, repo.extraWrites[0][qwenTokenPlanExhaustedReasonExtraKey], "quota exhausted")
}

func TestHandle429_QwenTokenPlanGeneric429DoesNotDisableScheduling(t *testing.T) {
	repo := &qwenTokenPlanRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := newQwenTokenPlanAccount()

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"rate limit exceeded"}}`))

	require.Empty(t, repo.extraWrites)
	require.Empty(t, repo.schedulableCalls)
	require.Equal(t, 1, repo.rateLimitCalls, "generic 429 may use the short fallback cooldown")
}

// 熬过观察窗后，生产实测的那句报文（Throttling.AllocationQuota + token-plan 耗尽）
// 判定为真耗尽。用例名保留是为了让 PR 的评审历史对得上。
func TestHandle429_QwenTokenPlanAllocatedQuotaDisablesScheduling(t *testing.T) {
	repo := &qwenTokenPlanRepo{}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := newQwenTokenPlanAccountExhaustedSince(time.Now().Add(-qwenTokenPlanExhaustionConfirmWindow - time.Minute))

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"request_id":"1bd488ef","code":"Throttling.AllocationQuota","message":"Your token-plan quota has been exhausted."}`))

	require.Equal(t, []bool{false}, repo.schedulableCalls)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, true, repo.extraWrites[0][qwenTokenPlanExhaustedExtraKey])
	require.Zero(t, repo.rateLimitCalls)
}

func TestHandle429_QwenTokenPlanMarkerWriteFailureStillPausesScheduling(t *testing.T) {
	repo := &qwenTokenPlanRepo{updateExtraErr: errors.New("database unavailable")}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := newQwenTokenPlanAccountExhaustedSince(time.Now().Add(-qwenTokenPlanExhaustionConfirmWindow - time.Minute))

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"Your token-plan quota has been exhausted."}}`))

	require.Equal(t, []bool{false}, repo.schedulableCalls)
	require.Len(t, repo.extraWrites, 1)
}

func TestHandle429_QwenTokenPlanPauseFailureStillPersistsMarker(t *testing.T) {
	repo := &qwenTokenPlanRepo{setScheduleErr: errors.New("database unavailable")}
	svc := NewRateLimitService(repo, nil, nil, nil, nil)
	account := newQwenTokenPlanAccountExhaustedSince(time.Now().Add(-qwenTokenPlanExhaustionConfirmWindow - time.Minute))

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"Your token-plan quota has been exhausted."}}`))

	require.Equal(t, []bool{false}, repo.schedulableCalls)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, true, repo.extraWrites[0][qwenTokenPlanExhaustedExtraKey])
}
