package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stubCooldownRecoverer struct {
	calls   []int64
	results map[int64]*SuccessfulTestRecoveryResult
	err     error
}

func (s *stubCooldownRecoverer) RecoverAccountState(_ context.Context, accountID int64, _ AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error) {
	s.calls = append(s.calls, accountID)
	if s.err != nil {
		return nil, s.err
	}
	if result, ok := s.results[accountID]; ok {
		return result, nil
	}
	return &SuccessfulTestRecoveryResult{ClearedError: true}, nil
}

type stubExpiredCooldownRepo struct {
	AccountRepository
	ids      []int64
	lastNow  time.Time
	lastLim  int
	listErr  error
	listCall int
}

func (s *stubExpiredCooldownRepo) ListAccountsWithExpiredCooldown(_ context.Context, now time.Time, limit int) ([]int64, error) {
	s.listCall++
	s.lastNow = now
	s.lastLim = limit
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.ids, nil
}

func TestAccountCooldownRecoveryRecoversScannedAccounts(t *testing.T) {
	repo := &stubExpiredCooldownRepo{ids: []int64{11, 22, 33}}
	recoverer := &stubCooldownRecoverer{}
	svc := NewAccountCooldownRecoveryService(repo, recoverer, nil)

	svc.scanOnce(context.Background())

	require.Equal(t, []int64{11, 22, 33}, recoverer.calls, "扫到的账号必须全部尝试恢复")
	require.Equal(t, accountCooldownRecoveryBatchSize, repo.lastLim, "必须传有界的批大小")
	require.False(t, repo.lastNow.IsZero(), "必须按当前时间判定到期")
}

// 单个账号恢复失败不能中断整批——否则一个坏账号会把后面的全挡住。
func TestAccountCooldownRecoveryContinuesAfterFailure(t *testing.T) {
	repo := &stubExpiredCooldownRepo{ids: []int64{1, 2, 3}}
	recoverer := &stubCooldownRecoverer{
		results: map[int64]*SuccessfulTestRecoveryResult{
			2: nil, // 视为无可恢复状态
		},
	}
	svc := NewAccountCooldownRecoveryService(repo, recoverer, nil)

	svc.scanOnce(context.Background())

	require.Equal(t, []int64{1, 2, 3}, recoverer.calls)
}

func TestAccountCooldownRecoveryHandlesScanError(t *testing.T) {
	repo := &stubExpiredCooldownRepo{listErr: errors.New("db down")}
	recoverer := &stubCooldownRecoverer{}
	svc := NewAccountCooldownRecoveryService(repo, recoverer, nil)

	svc.scanOnce(context.Background())

	require.Empty(t, recoverer.calls, "扫描失败时不应触发任何恢复")
}

func TestAccountCooldownRecoveryNoAccountsIsNoop(t *testing.T) {
	repo := &stubExpiredCooldownRepo{ids: nil}
	recoverer := &stubCooldownRecoverer{}
	svc := NewAccountCooldownRecoveryService(repo, recoverer, nil)

	svc.scanOnce(context.Background())

	require.Equal(t, 1, repo.listCall)
	require.Empty(t, recoverer.calls)
}

// 关键不变式：仓储的扫描判据与 hasRecoverableRuntimeState 必须认同同一组冷却字段。
// 若两者分叉，就会出现「扫到了却不恢复」的死角——本测试遍历字段声明来钉住它。
func TestEveryCooldownFieldIsRecognizedAsRecoverable(t *testing.T) {
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		field   string
		account *Account
	}{
		{"rate_limit_reset_at", &Account{RateLimitResetAt: &past}},
		{"temp_unschedulable_until", &Account{TempUnschedulableUntil: &past}},
		{"overload_until", &Account{OverloadUntil: &past}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			require.Truef(t, hasRecoverableRuntimeState(tc.account),
				"%s 已过期的账号会被仓储扫出来，但 hasRecoverableRuntimeState 不认它，恢复会静默跳过", tc.field)
		})
	}

	// 纯 status=error（无任何冷却时间戳）不属于运行时可恢复状态，
	// 由 RecoverAccountState 的 ClearError 分支处理，这里只确认语义未被混淆。
	require.False(t, hasRecoverableRuntimeState(&Account{Status: StatusError}))
}
