package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	accountCooldownRecoveryScanInterval = time.Minute
	accountCooldownRecoveryBatchSize    = 200
	accountCooldownRecoveryLeaderLock   = "jobs:account-cooldown-recovery"
	accountCooldownRecoveryLockTTL      = 55 * time.Second
)

// accountCooldownRecoverer 是本服务需要的恢复能力，由 RateLimitService 提供。
// 复用既有的 RecoverAccountState：它已经能一次清掉 error / 限流 / 过载 /
// 临时停调 / 模型级限流 / 403 计数，恢复语义只写一处。
type accountCooldownRecoverer interface {
	RecoverAccountState(ctx context.Context, accountID int64, options AccountRecoveryOptions) (*SuccessfulTestRecoveryResult, error)
}

// AccountCooldownRecoveryService 把「冷却到期即恢复」补成时间驱动的入口。
//
// 为什么需要它：暂时性上游故障（429 限流、过载、并发上限、余额不足等）会同时写
// 两处状态——带到期时间的冷却列（rate_limit_reset_at / temp_unschedulable_until /
// overload_until），以及不带到期时间的 status=error。后者盖住前者，于是冷却到期后
// 没有任何代码回头看它：恢复能力（RecoverAccountState）其实早就写好了，但只有两个
// 触发口——管理员点 recover，或管理员点账号测试。两个都要人。
//
// 实测（生产，2026-09-18）：252 个 error 账号里 120 个的 rate_limit_reset_at 已经
// 过期、8 个 temp_unschedulable_until 已经过期，全部钉死等人工点。
//
// 判据只看「冷却列已到期」这个声明，不枚举错误原因（见 ListAccountsWithExpiredCooldown）。
// 这样新增的冷却类型只要写在那三列上就自动被覆盖，不必再改这里。
//
// 关于凭据失效账号：那 120 个里 118 个其实是 token revoked / 401，恢复后第一次请求
// 会再次 401 并被重新置回 error。代价是每个账号一发失败请求，且不会反复刷——因为
// 重新置 error 时不带新的到期时间，本服务的判据下次就不再命中它。用这一发请求换掉
// 「靠错误文案猜测哪些能恢复」的一整套特例判定，是刻意的取舍。
// ExpiredCooldownAccountLister 是本服务对账号仓储的全部依赖。
//
// 刻意不加进 AccountRepository：那个接口在上游测试里有几十个手写 mock，fork 往里加一个
// 方法，这些 mock 就全部编译不过（带 unit 标签的测试因此整包失效过，而默认的 go test
// 不带标签、看不出来），上游以后每新增一个 mock 还会再撞一次。与 AdminAccountRepository
// 同一个做法：同一个仓储实例，以使用方需要的那一个能力暴露。
type ExpiredCooldownAccountLister interface {
	// ListAccountsWithExpiredCooldown 返回冷却时间已过期但仍不可调度的账号 ID。
	// 判据是冷却列已到期这个声明本身，不枚举错误原因，因此新增的冷却类型自动被覆盖。
	ListAccountsWithExpiredCooldown(ctx context.Context, now time.Time, limit int) ([]int64, error)
}

type AccountCooldownRecoveryService struct {
	accountRepo ExpiredCooldownAccountLister
	recoverer   accountCooldownRecoverer
	leaderLock  LeaderLockCache

	ctx    context.Context
	cancel context.CancelFunc
	owner  string
	start  sync.Once
	stop   sync.Once
	wg     sync.WaitGroup
}

func NewAccountCooldownRecoveryService(
	accountRepo ExpiredCooldownAccountLister,
	recoverer accountCooldownRecoverer,
	leaderLock LeaderLockCache,
) *AccountCooldownRecoveryService {
	ctx, cancel := context.WithCancel(context.Background())
	return &AccountCooldownRecoveryService{
		accountRepo: accountRepo,
		recoverer:   recoverer,
		leaderLock:  leaderLock,
		ctx:         ctx,
		cancel:      cancel,
		owner:       uuid.NewString(),
	}
}

func (s *AccountCooldownRecoveryService) Start() {
	if s == nil || s.accountRepo == nil || s.recoverer == nil {
		return
	}
	s.start.Do(func() {
		s.wg.Add(1)
		go s.run()
	})
}

func (s *AccountCooldownRecoveryService) Stop() {
	if s == nil {
		return
	}
	s.stop.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

func (s *AccountCooldownRecoveryService) run() {
	defer s.wg.Done()
	// 启动后稍等，避开实例启动时的其它初始化工作。
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-timer.C:
		s.scanOnce(s.ctx)
	}
	ticker := time.NewTicker(accountCooldownRecoveryScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.scanOnce(s.ctx)
		}
	}
}

// scanOnce 扫一批冷却已到期的账号并恢复。多实例下由 leader 锁收敛到一个实例执行。
func (s *AccountCooldownRecoveryService) scanOnce(ctx context.Context) {
	release, ok := s.tryAcquireScanLock(ctx)
	if !ok {
		return
	}
	defer release()

	scanCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()

	now := time.Now()
	accountIDs, err := s.accountRepo.ListAccountsWithExpiredCooldown(scanCtx, now, accountCooldownRecoveryBatchSize)
	if err != nil {
		slog.Warn("account_cooldown_recovery_scan_failed", "error", err)
		return
	}
	if len(accountIDs) == 0 {
		return
	}

	recovered := 0
	for _, accountID := range accountIDs {
		select {
		case <-scanCtx.Done():
			return
		default:
		}
		result, err := s.recoverer.RecoverAccountState(scanCtx, accountID, AccountRecoveryOptions{})
		if err != nil {
			slog.Warn("account_cooldown_recovery_failed", "account_id", accountID, "error", err)
			continue
		}
		if result == nil || (!result.ClearedError && !result.ClearedRateLimit) {
			continue
		}
		recovered++
		// 每次自愈都留痕：这类兜底以前完全静默，正是 120 个账号卡死却没人发现的原因。
		slog.Info(
			"account_cooldown_recovered",
			"account_id", accountID,
			"cleared_error", result.ClearedError,
			"cleared_rate_limit", result.ClearedRateLimit,
		)
	}
	if recovered > 0 {
		slog.Info("account_cooldown_recovery_batch_done",
			"scanned", len(accountIDs),
			"recovered", recovered,
		)
	}
}

func (s *AccountCooldownRecoveryService) tryAcquireScanLock(ctx context.Context) (func(), bool) {
	if s.leaderLock == nil {
		// 未配置分布式锁（单实例部署/测试）：直接扫描。
		return func() {}, true
	}
	ok, err := s.leaderLock.TryAcquireLeaderLock(ctx, accountCooldownRecoveryLeaderLock, s.owner, accountCooldownRecoveryLockTTL)
	if err != nil {
		slog.Warn("account_cooldown_recovery_lock_failed", "error", err)
		return nil, false
	}
	if !ok {
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.leaderLock.ReleaseLeaderLock(releaseCtx, accountCooldownRecoveryLeaderLock, s.owner)
	}, true
}
