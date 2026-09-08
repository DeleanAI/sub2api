//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQwenTokenPlanQuotaExhaustedMatchesProductionPayloads 用 may 生产库里真实出现过的
// 报文钉死分类器。
//
// 样本来自 ops_error_logs（2026-09-08 取）：121 个 Token Plan 账号命中额度耗尽 14206 次，
// 报文只有一种形状；同期出现的其它 429 全是临时限流。误判方向是不对称的——把临时限流
// 判成永久会把健康账号**不可逆**地踢出调度（该状态明确永不自动清除），
// 而漏判只是这次走普通 429 退避。所以宁可漏判。
func TestQwenTokenPlanQuotaExhaustedMatchesProductionPayloads(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		exhausted bool
		hits      string // 生产中的出现次数，说明这条样本不是编的
	}{
		{
			name:      "生产主形态：Throttling.AllocationQuota + token-plan 耗尽",
			body:      `{"request_id":"1bd488ef","code":"Throttling.AllocationQuota","message":"Your token-plan quota has been exhausted."}`,
			exhausted: true, hits: "14199",
		},
		{
			name:      "生产变体：裸 Throttling code",
			body:      "event:error\ndata:{\"request_id\":\"534126b0\",\"code\":\"Throttling\",\"message\":\"Your token-plan quota has been exhausted.\"}",
			exhausted: true, hits: "7",
		},
		{
			name:      "生产：Throttling.ResourceExhausted 模型服务过载",
			body:      `{"error":{"code":"Throttling.ResourceExhausted","message":"An error occurred in model serving, error message is: [Too many requests.]","type":"Throttling.ResourceExhausted"}}`,
			exhausted: false, hits: "37",
		},
		{
			name:      "生产：1302 账户速率限制",
			body:      `{"code":"1302","message":"[1302][您的账户已达到速率限制，请您控制请求频率][20260907143951a09bb947b1184519]"}`,
			exhausted: false, hits: "1",
		},
		{
			name:      "生产：1308 五小时窗口上限，带重置时间",
			body:      `{"code":"1308","message":"[1308][已达到 5 小时的使用上限。您的限额将在 2026-09-07 18:01:51 重置。][202609071540112a6345e552044]"}`,
			exhausted: false, hits: "8",
		},
		{
			name:      "生产：网关侧并发限制",
			body:      `{"error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded for account, please retry later"}}`,
			exhausted: false, hits: "7",
		},
		{
			name:      "生产：上游限流，稍后重试",
			body:      `{"error":{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later"}}`,
			exhausted: false, hits: "352",
		},
		// 以下按阿里云错误码文档，我们的流量里还没出现过。
		{
			name:      "文档：Coding Plan 当月额度用完",
			body:      `{"code":"Throttling.AllocationQuota","message":"month allocated quota exceeded"}`,
			exhausted: true, hits: "文档",
		},
		{
			name:      "文档：免费额度用完",
			body:      `{"code":"AllocationQuota.FreeTierOnly","message":"The free tier has been exhausted"}`,
			exhausted: true, hits: "文档",
		},
		{
			name:      "文档：TPM 限流（与当月耗尽只差一个词）",
			body:      `{"code":"Throttling.AllocationQuota","message":"usage allocated quota exceeded"}`,
			exhausted: false, hits: "文档",
		},
		{
			name:      "文档：insufficient_quota 属 TPM 限流，提额即恢复",
			body:      `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}`,
			exhausted: false, hits: "文档",
		},
		{
			name:      "文档：突发速率",
			body:      `{"code":"Throttling.BurstRate","message":"Request rate increased too quickly"}`,
			exhausted: false, hits: "文档",
		},
		// 结构性：不能在整个响应体上找关键词。
		{
			name:      "账号自己的 base_url 里带 token-plan，报文是普通限流",
			body:      `{"error":{"message":"upstream https://token-plan.example.com/v1 returned: rate limit exceeded, please retry later"}}`,
			exhausted: false, hits: "结构性",
		},
		{
			name:      "中间件 HTML 错误页",
			body:      `<html><body>Service quota exhausted. Please contact support.</body></html>`,
			exhausted: false, hits: "结构性",
		},
		{
			name:      "空响应体",
			body:      ``,
			exhausted: false, hits: "结构性",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qwenTokenPlanQuotaExhausted([]byte(tc.body))
			if tc.exhausted {
				require.Truef(t, got, "应判为额度耗尽（生产命中 %s 次）：%s", tc.hits, tc.body)
				return
			}
			require.Falsef(t, got,
				"不能判为额度耗尽（生产命中 %s 次）：判错会把健康账号不可逆地踢出调度。报文：%s", tc.hits, tc.body)
		})
	}
}

// TestQwenTokenPlanRecoveryWordingAlwaysWins 钉死"报文自称会恢复 = 临时"这条优先级。
//
// 原实现把「会在某时重置」当成**永久**的证据（hasQuota && hasExhaustion && hasReset），
// 方向是反的。生产里的 1308 正是「已达到 5 小时的使用上限。您的限额将在 … 重置。」——
// 一个五小时后就恢复的窗口限额。
func TestQwenTokenPlanRecoveryWordingAlwaysWins(t *testing.T) {
	for _, body := range []string{
		`{"message":"Your quota has been exhausted. The quota will reset at 2026-09-14T00:00:00Z."}`,
		`{"message":"quota exhausted, reset at 2026-09-14T00:00:00Z"}`,
		`{"message":"额度已用尽，将在 2026-09-14 00:00:00 重置"}`,
		`{"message":"配额耗尽，请稍后重试"}`,
	} {
		require.Falsef(t, qwenTokenPlanQuotaExhausted([]byte(body)),
			"报文说了会恢复就不能永久停调：%s", body)
	}
}

// TestQwenTokenPlanExhaustionRequiresPersistence 钉死"额度耗尽要熬过观察窗才算数"。
//
// 依据是 may 生产库实测：121 个 Token Plan 账号都发过同一句
// "Your token-plan quota has been exhausted."，但 25 个在平均 48 秒（最长 6.1 分钟）
// 内就恢复了，96 个再也没恢复过（平均持续报错 28.2 小时）。报文和错误码都区分不了
// 这两拨（Throttling.AllocationQuota 在两边都出现），唯一分得开的是这个状态
// 扛不扛得过十分钟。看一次报文就永久停调，会把 25/121 的健康账号不可逆地踢出调度。
func TestQwenTokenPlanExhaustionRequiresPersistence(t *testing.T) {
	require.Greater(t, qwenTokenPlanExhaustionConfirmWindow, 367*time.Second,
		"观察窗必须大于实测最长恢复时间（367 秒），否则临时窗口会被误判为永久耗尽")

	t.Run("首次命中只记时刻，不读起点时为空", func(t *testing.T) {
		account := &Account{ID: 1}
		_, ok := qwenTokenPlanExhaustedSince(account)
		require.False(t, ok, "没有标记时不能凭空得到起点")
	})

	t.Run("观察窗内的起点被认出来", func(t *testing.T) {
		since := time.Now().UTC().Add(-2 * time.Minute)
		account := &Account{ID: 2, Extra: map[string]any{
			qwenTokenPlanExhaustedSinceExtraKey: since.Format(time.RFC3339),
		}}
		got, ok := qwenTokenPlanExhaustedSince(account)
		require.True(t, ok)
		require.WithinDuration(t, since, got, time.Second)
		require.Less(t, time.Since(got), qwenTokenPlanExhaustionConfirmWindow,
			"2 分钟前的起点仍在观察窗内，此时不得永久停调")
	})

	t.Run("熬过观察窗的起点被认出来", func(t *testing.T) {
		since := time.Now().UTC().Add(-qwenTokenPlanExhaustionConfirmWindow - time.Minute)
		account := &Account{ID: 3, Extra: map[string]any{
			qwenTokenPlanExhaustedSinceExtraKey: since.Format(time.RFC3339),
		}}
		got, ok := qwenTokenPlanExhaustedSince(account)
		require.True(t, ok)
		require.GreaterOrEqual(t, time.Since(got), qwenTokenPlanExhaustionConfirmWindow,
			"起点已超过观察窗，此时才允许永久停调")
	})

	t.Run("脏值不能被当成很久以前的起点", func(t *testing.T) {
		account := &Account{ID: 4, Extra: map[string]any{
			qwenTokenPlanExhaustedSinceExtraKey: "not-a-timestamp",
		}}
		_, ok := qwenTokenPlanExhaustedSince(account)
		require.False(t, ok,
			"解析不了的起点必须当作没有：当成零值会让 now-since 远大于观察窗，一次 429 就永久停调")
	})
}
