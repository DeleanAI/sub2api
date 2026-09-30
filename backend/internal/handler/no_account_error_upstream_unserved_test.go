//go:build unit

package handler

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 候选账号按配置能服务该模型、但上游都在冷却期内答过服务不了：照上游的回答给 404，说明何时再试；
// 选号错误里的 model_rate_limited 计数（那些正是「服务不了」冷却）不许把它降成 429「限流」。
func TestClassifyNoAccountError_UpstreamUnservedReturns404NotRateLimited(t *testing.T) {
	until := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fd := &fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: false, UpstreamUnservedUntil: &until}}
	apiKey := &service.APIKey{GroupID: ptrInt64(2)}
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic} {
		cls := classifyNoAccountErrorFromGin(newTestGinContextWithRequest(), fd, apiKey, "gpt-6-luna", "gpt-6-luna", platform)
		require.Equal(t, http.StatusNotFound, cls.Status, platform)
		require.Equal(t, "model_not_found", cls.ErrType)
		require.True(t, cls.ModelNotFound)
		require.Contains(t, cls.Message, `"gpt-6-luna"`)
		require.Contains(t, cls.Message, "upstream reported it as unsupported")
		require.Contains(t, cls.Message, "2026-09-30T12:00:00Z")

		final := classifySelectionFailureError(errors.New("no available accounts: model_rate_limited=1"), cls)
		require.Equal(t, http.StatusNotFound, final.Status, "上游答过服务不了的冷却不是限流")
	}
}

// 没配任何账号支持时仍是原来的措辞；有支持者（哪怕在真实限流中）仍走 503 / 429。
func TestClassifyNoAccountError_UpstreamUnservedDoesNotChangeOtherVerdicts(t *testing.T) {
	apiKey := &service.APIKey{GroupID: ptrInt64(2)}
	cls := classifyNoAccountErrorFromGin(newTestGinContextWithRequest(),
		&fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true}}, apiKey, "m", "m", service.PlatformOpenAI)
	require.Equal(t, `Model "m" is not supported by any configured account in this group`, cls.Message)

	cls = classifyNoAccountErrorFromGin(newTestGinContextWithRequest(),
		&fakeDiagnoser{resp: service.ModelAvailabilityDiagnosis{HasAccountsInPool: true, HasModelSupport: true}}, apiKey, "m", "m", service.PlatformOpenAI)
	require.Equal(t, http.StatusServiceUnavailable, cls.Status)
	require.Equal(t, http.StatusTooManyRequests, classifySelectionFailureError(errors.New("model_rate_limited=2"), cls).Status)
}
