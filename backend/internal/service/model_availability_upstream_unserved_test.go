//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// diagnoseBoth 用负责该平台的诊断器对同一个候选池诊断同一个模型：GatewayService 覆盖全部平台，
// OpenAIGatewayService 只负责 OpenAI 兼容平台。
func diagnoseBoth(t *testing.T, accounts []Account, model string) map[string]ModelAvailabilityDiagnosis {
	t.Helper()
	repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{}}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	groupID := int64(42)
	gateway := &GatewayService{accountRepo: repo, cfg: testConfig(), schedulerSnapshot: &SchedulerSnapshotService{}}
	openai := &OpenAIGatewayService{accountRepo: repo, cfg: testConfig(), schedulerSnapshot: &SchedulerSnapshotService{}}
	platform := accounts[0].Platform
	diags := map[string]ModelAvailabilityDiagnosis{
		"gateway": gateway.DiagnoseModelAvailabilityForPlatform(context.Background(), &groupID, model, platform),
	}
	if platform == PlatformOpenAI {
		diags["openai"] = openai.DiagnoseModelAvailabilityForPlatform(context.Background(), &groupID, model, platform)
	}
	return diags
}

func unservedTestAccount(id int64, platform string) Account {
	return Account{ID: id, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		AccountGroups: []AccountGroup{{GroupID: 42}}}
}

// 声明里的每一种「上游答服务不了」冷却，在冷却期内都不算支持者；诊断给出最早重试时间。遍历声明，新加的原因当天被覆盖。
func TestModelAvailabilityExcludesEveryUpstreamUnservedReason(t *testing.T) {
	require.NotEmpty(t, upstreamModelUnservedReasons)
	for reason := range upstreamModelUnservedReasons {
		for _, platform := range []string{PlatformOpenAI, PlatformAnthropic} {
			until := time.Now().Add(20 * time.Minute).Truncate(time.Second)
			account := unservedTestAccount(1, platform)
			setAccountModelRateLimitSnapshot(&account, "some-model", until, reason, time.Now())
			for name, diag := range diagnoseBoth(t, []Account{account}, "some-model") {
				require.True(t, diag.HasAccountsInPool, "%s %s %s", reason, platform, name)
				require.False(t, diag.HasModelSupport, "%s %s %s：上游答过服务不了，冷却期内不算支持", reason, platform, name)
				require.NotNil(t, diag.UpstreamUnservedUntil, "%s %s %s", reason, platform, name)
				require.WithinDuration(t, until, *diag.UpstreamUnservedUntil, time.Second)
			}
		}
	}
}

// 反例：真正的限流（其他原因的模型级冷却）、已过期的「服务不了」冷却、以及池里还有别的账号能服务时，模型仍算有支持。
func TestModelAvailabilityKeepsGenuineCooldownsAndOtherSupporters(t *testing.T) {
	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Minute)
	limited := unservedTestAccount(1, PlatformOpenAI)
	setAccountModelRateLimitSnapshot(&limited, "some-model", future, openAICodexSparkRateLimitReason, time.Now())
	expired := unservedTestAccount(2, PlatformOpenAI)
	setAccountModelRateLimitSnapshot(&expired, "some-model", past, upstreamModelNotFoundReason, time.Now())
	unserved := unservedTestAccount(3, PlatformOpenAI)
	setAccountModelRateLimitSnapshot(&unserved, "some-model", future, upstreamModelNotFoundReason, time.Now())
	healthy := unservedTestAccount(4, PlatformOpenAI)
	for name, pool := range map[string][]Account{
		"genuine rate limit":            {limited},
		"expired unserved cooldown":     {expired},
		"another account still serves":  {unserved, healthy},
		"unserved listed after healthy": {healthy, unserved},
	} {
		for diagName, diag := range diagnoseBoth(t, pool, "some-model") {
			require.True(t, diag.HasModelSupport, "%s (%s)", name, diagName)
		}
	}
}

// 往返：HandleUpstreamError 把上游的「服务不了」写成冷却后，诊断必须按不支持算——写入方用的原因都在声明里。
func TestUpstreamModelNotFoundCooldownRoundTripsIntoDiagnosis(t *testing.T) {
	for name, tc := range map[string]struct {
		account *Account
		status  int
		body    string
		model   string
	}{
		"404 model not found":  {openAIModelNotFoundTempAccount(), http.StatusNotFound, `{"error":{"code":"model_not_found","message":"model not found"}}`, "gpt-5.4"},
		"401 unknown model":    {openAIModelNotFoundTempAccount(), http.StatusUnauthorized, `{"error":{"message":"unknown model definitely-not-real"}}`, "definitely-not-real"},
		"codex plan-gated 400": {openAICodexPlanGatedOAuthAccount(), http.StatusBadRequest, `{"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}`, "gpt-5.6-sol"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &modelNotFoundAccountRepoStub{}
			svc := &RateLimitService{accountRepo: repo}
			require.True(t, svc.HandleUpstreamError(context.Background(), tc.account, tc.status, http.Header{}, []byte(tc.body), tc.model))
			require.Len(t, repo.modelRateLimitCalls, 1)
			call := repo.modelRateLimitCalls[0]
			require.True(t, upstreamModelUnservedReasons[call.reason], "写入的原因 %q 必须在 upstreamModelUnservedReasons 里", call.reason)
			account := *tc.account
			account.AccountGroups = []AccountGroup{{GroupID: 42}}
			setAccountModelRateLimitSnapshot(&account, call.scope, call.resetAt, call.reason, time.Now())
			for diagName, diag := range diagnoseBoth(t, []Account{account}, tc.model) {
				require.False(t, diag.HasModelSupport, diagName)
				require.NotNil(t, diag.UpstreamUnservedUntil, diagName)
			}
		})
	}
}

// 每一种声明的「服务不了」原因都要有一个上游回答样本，判定认得出来，客户端拿到 404 model_not_found；
// 新加原因不补样本，这里就失败。
func TestUpstreamModelUnservedVerdictCoversEveryDeclaredReason(t *testing.T) {
	samples := map[string]struct {
		status int
		body   string
	}{
		// 线上原文（toooooken 2026-09-30，上游是另一个 sub2api）。
		upstreamModelNotFoundReason:       {http.StatusNotFound, `{"error":{"message":"Model \"gpt-5.4\" is not supported by any configured account in this group","type":"model_not_found"}}`},
		upstreamModelNotFound401Reason:    {http.StatusUnauthorized, `{"error":{"message":"unknown model definitely-not-real"}}`},
		upstreamCodexPlanGatedModelReason: {http.StatusBadRequest, `{"detail":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}`},
	}
	require.Len(t, samples, len(upstreamModelUnservedReasons), "每个声明的原因都要有样本")
	for reason := range upstreamModelUnservedReasons {
		sample, ok := samples[reason]
		require.True(t, ok, "原因 %q 没有样本", reason)
		require.Equal(t, reason, upstreamModelUnservedKind(sample.status, []byte(sample.body)), reason)
		status, errType, message, handled := UpstreamModelUnservedClientError(sample.status, []byte(sample.body))
		require.True(t, handled, reason)
		require.Equal(t, http.StatusNotFound, status, reason)
		require.Equal(t, "model_not_found", errType)
		require.Contains(t, message, "upstream reported it as unsupported")
	}
	for _, other := range []struct {
		status int
		body   string
	}{
		{http.StatusNotFound, `{"error":{"message":"route not found"}}`},
		{http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`},
		{http.StatusTooManyRequests, `{"error":{"message":"rate limit exceeded for model gpt-5.4"}}`},
		{http.StatusInternalServerError, `{"error":{"message":"model not found"}}`},
	} {
		_, _, _, handled := UpstreamModelUnservedClientError(other.status, []byte(other.body))
		require.False(t, handled, "%d %s", other.status, other.body)
	}
}
