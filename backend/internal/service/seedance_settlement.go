package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Seedance 的结算保证：每个经网关创建、最终成功的任务恰好计费一次，发生在第一次观察到 succeeded 时——
// 不论是调用方查询看到的，还是网关后台补查看到的。
//
// 为什么要后台补查：请求原样透传（含 callback_url），方舟会把与查询接口一致的结果（含 video_url 与
// usage）直接推给回调地址，调用方不必再经网关查询；而方舟对成功任务按 usage.completion_tokens 收费，
// 与调用方是否来取无关。只靠调用方查询计费，回调用户与「建了不取」的任务就成了网关替人付的成本。
// 调用方正常轮询时，第一次查到成功就计费并移出索引，后台补查根本不会发生。
const (
	seedanceSettlementFirstCheck   = 10 * time.Minute
	seedanceSettlementMaxInterval  = time.Hour
	seedanceSettlementScanInterval = time.Minute
	seedanceSettlementBatchSize    = 50
	seedanceSettlementLeaderLock   = "jobs:seedance-settlement"
	seedanceSettlementLockTTL      = 55 * time.Second

	// SeedanceTasksPath 是方舟任务接口的路径（入站路由与上游端点同名）。
	SeedanceTasksPath = "/api/v3/contents/generations/tasks"
	// SeedanceSettlementInboundEndpoint 标记由网关后台结算落下的用量行：调用方没有与之对应的那次请求。
	SeedanceSettlementInboundEndpoint = "gateway:seedance-settlement"
)

// SeedanceSettlementEntry 是「已创建、未计费」索引里的一项。只含任务身份，创建与查询两侧拼出的值相同；
// 创建时的计费上下文（账号、额度归属平台、模型名、创建时间）放在计费快照 GrokVideoPendingBilling 里。
type SeedanceSettlementEntry struct {
	TaskKey  string `json:"t"`
	UserID   int64  `json:"u"`
	APIKeyID int64  `json:"k"`
	GroupID  int64  `json:"g,omitempty"`
}

func (e SeedanceSettlementEntry) member() (string, error) {
	if !IsSeedanceTaskKey(e.TaskKey) || e.UserID <= 0 || e.APIKeyID <= 0 {
		return "", fmt.Errorf("invalid seedance settlement entry")
	}
	raw, err := json.Marshal(e)
	return string(raw), err
}

func parseSeedanceSettlementMember(member string) (SeedanceSettlementEntry, error) {
	var entry SeedanceSettlementEntry
	if err := json.Unmarshal([]byte(member), &entry); err != nil {
		return entry, err
	}
	if _, err := entry.member(); err != nil {
		return entry, err
	}
	return entry, nil
}

// SeedanceSettlementStore 保存结算索引：成员是条目，分数是下一次后台补查的时刻。
type SeedanceSettlementStore interface {
	ScheduleSeedanceSettlement(ctx context.Context, member string, dueAt time.Time) error
	DueSeedanceSettlements(ctx context.Context, now time.Time, limit int) ([]string, error)
	SeedanceSettlementPending(ctx context.Context, member string) (bool, error)
	RemoveSeedanceSettlement(ctx context.Context, member string) error
}

// SeedanceBillingOutcome 是一次状态观察对计费的结论。
type SeedanceBillingOutcome int

const (
	// SeedanceBillingPending：任务还在排队/运行，继续等。
	SeedanceBillingPending SeedanceBillingOutcome = iota
	// SeedanceBillingReady：抢到了一次性计费标记，调用方负责落账（失败要释放标记）。
	SeedanceBillingReady
	// SeedanceBillingSettled：已由别处计过费，或任务以失败/取消/过期结束，无需再管。
	SeedanceBillingSettled
	// SeedanceBillingRetry：读写计费状态或查询上游出错，保留在索引里稍后再试。
	SeedanceBillingRetry
	// SeedanceBillingUnbillable：无从计价（成功却没有 completion_tokens、任务记录已不在等），已记错误日志。
	SeedanceBillingUnbillable
)

// Done 表示这个任务不再需要后台补查。
func (o SeedanceBillingOutcome) Done() bool {
	return o == SeedanceBillingReady || o == SeedanceBillingSettled || o == SeedanceBillingUnbillable
}

// LoadAsyncVideoPendingBilling 读创建时的计费快照（Grok 与 Seedance 共用）。读失败留日志并按缺失处理，
// 缺失时能否仍然计价由各提供方自己判断。
func (s *OpenAIGatewayService) LoadAsyncVideoPendingBilling(ctx context.Context, log *zap.Logger, taskKey string, userID, apiKeyID int64) *GrokVideoPendingBilling {
	pending, err := s.LoadGrokVideoPendingBilling(ctx, taskKey, userID, apiKeyID)
	if err != nil {
		log.Warn("grok_media.video_pending_billing_load_failed", zap.String("request_id", taskKey), zap.Error(err))
	}
	return pending
}

// ClaimAsyncVideoBilling 抢异步视频任务的一次性计费标记（Grok 与 Seedance 共用）。出错时不计费
// （宁可漏计不重计），但留错误日志——每一次都是上游已收费、这边还没计的任务。
func (s *OpenAIGatewayService) ClaimAsyncVideoBilling(ctx context.Context, log *zap.Logger, taskKey string, userID, apiKeyID int64) (bool, error) {
	claimed, err := s.ClaimGrokVideoBilling(ctx, taskKey, userID, apiKeyID)
	if err != nil {
		log.Error("grok_media.video_billing_claim_failed", zap.String("request_id", taskKey), zap.Error(err))
		return false, err
	}
	if !claimed {
		log.Debug("grok_media.video_billing_already_claimed", zap.String("request_id", taskKey))
	}
	return claimed, nil
}

// PrepareSeedanceCompletionBilling 根据一次状态观察准备计费；调用方查询与后台结算共用这一个实现。
//
// Ark reports actual completion tokens: never infer tokens from duration or use Grok's per-second
// tariff. 创建时快照缺失时仍然计费：用量来自这次上游返回本身（方舟文档：completion_tokens「可作为
// 计费对账依据」），缺的只是调用方用的模型别名，按上游返回的模型名计价并记错误日志。
func (s *OpenAIGatewayService) PrepareSeedanceCompletionBilling(ctx context.Context, log *zap.Logger, userID, apiKeyID int64, taskKey string, observed *OpenAIForwardResult) (*OpenAIForwardResult, *GrokVideoPendingBilling, SeedanceBillingOutcome) {
	if observed == nil {
		return nil, nil, SeedanceBillingPending
	}
	switch observed.UpstreamTaskStatus {
	case "succeeded":
	case "failed", "cancelled", "expired":
		return nil, nil, SeedanceBillingSettled
	default:
		return nil, nil, SeedanceBillingPending
	}
	if observed.Usage.OutputTokens <= 0 {
		// 方舟对成功任务总会返回 completion_tokens（2.0 系列还有最低用量）；没有就无从计价。
		log.Error("seedance.succeeded_without_completion_tokens", zap.String("request_id", taskKey), zap.String("upstream_model", observed.UpstreamModel))
		return nil, nil, SeedanceBillingUnbillable
	}
	pending := s.LoadAsyncVideoPendingBilling(ctx, log, taskKey, userID, apiKeyID)
	if pending == nil {
		log.Error("grok_media.video_billing_without_pending",
			zap.String("request_id", taskKey),
			zap.String("upstream_model", observed.UpstreamModel),
			zap.String("note", "no create-time snapshot; billing the status usage under the upstream-reported model; investigate pending store failures"),
		)
	}
	claimed, err := s.ClaimAsyncVideoBilling(ctx, log, taskKey, userID, apiKeyID)
	if err != nil {
		return nil, pending, SeedanceBillingRetry
	}
	if !claimed {
		return nil, pending, SeedanceBillingSettled
	}
	merged := *observed
	if pending != nil {
		merged.Model = pending.Model
		merged.BillingModel = firstNonBlank(pending.BillingModel, pending.Model)
		merged.UpstreamModel = firstNonBlank(pending.UpstreamModel, observed.UpstreamModel)
		if e2e := GrokVideoE2EDuration(pending.CreatedAt, time.Now()); e2e > 0 {
			merged.Duration = e2e
		}
	} else {
		merged.Model = observed.UpstreamModel
		merged.BillingModel = observed.UpstreamModel
	}
	merged.RequestID = StableGrokVideoBillingRequestID(taskKey)
	merged.ResponseID = taskKey
	return &merged, pending, SeedanceBillingReady
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// AttachSeedanceSettlement 把后台结算挂到网关服务上（装配时调用；为 nil 表示不做后台结算）。
func (s *OpenAIGatewayService) AttachSeedanceSettlement(settlement *SeedanceSettlementService) {
	s.seedanceSettlement = settlement
}

// TrackSeedanceTask 把刚创建的任务放进结算索引，安排第一次后台补查。
func (s *OpenAIGatewayService) TrackSeedanceTask(ctx context.Context, entry SeedanceSettlementEntry) {
	if s == nil || s.seedanceSettlement == nil {
		return
	}
	s.seedanceSettlement.schedule(ctx, entry, time.Now().Add(seedanceSettlementFirstCheck))
}

// ForgetSeedanceTask 把已结清（计过费或以非成功终态结束）的任务移出结算索引。
func (s *OpenAIGatewayService) ForgetSeedanceTask(ctx context.Context, entry SeedanceSettlementEntry) {
	if s == nil || s.seedanceSettlement == nil {
		return
	}
	s.seedanceSettlement.forget(ctx, entry)
}

// SettleSeedanceTaskBeforeDelete 在转发 DELETE 之前把仍未计费的任务结清：删除后任务记录就查不到了，
// 否则「回调拿到视频后立刻删任务」就能不付费。只在任务还在索引里时多查一次上游；account 是本次
// 请求已经选定的、持有该任务的账号。
func (s *OpenAIGatewayService) SettleSeedanceTaskBeforeDelete(ctx context.Context, entry SeedanceSettlementEntry, account *Account) {
	if s == nil || s.seedanceSettlement == nil {
		return
	}
	s.seedanceSettlement.settleIfPending(ctx, entry, account)
}

// SeedanceSettlementService 负责后台补查与结算。多实例下由 leader 锁收敛到一个实例扫描。
type SeedanceSettlementService struct {
	store         SeedanceSettlementStore
	gateway       *OpenAIGatewayService
	accountRepo   AccountRepository
	apiKeys       *APIKeyService
	subscriptions *SubscriptionService
	leaderLock    LeaderLockCache
	log           *zap.Logger
	// recordUsage 落一笔用量，默认是 record（与请求时同一个 RecordUsage 入口）；单测替换它以免搭整套计费依赖。
	recordUsage func(ctx context.Context, entry SeedanceSettlementEntry, account *Account, bill *OpenAIForwardResult, pending *GrokVideoPendingBilling) error

	ctx    context.Context
	cancel context.CancelFunc
	owner  string
	start  sync.Once
	stop   sync.Once
	wg     sync.WaitGroup
}

func NewSeedanceSettlementService(
	store SeedanceSettlementStore,
	gateway *OpenAIGatewayService,
	accountRepo AccountRepository,
	apiKeys *APIKeyService,
	subscriptions *SubscriptionService,
	leaderLock LeaderLockCache,
) *SeedanceSettlementService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &SeedanceSettlementService{
		store:         store,
		gateway:       gateway,
		accountRepo:   accountRepo,
		apiKeys:       apiKeys,
		subscriptions: subscriptions,
		leaderLock:    leaderLock,
		log:           logger.L().With(zap.String("component", "service.seedance_settlement")),
		ctx:           ctx,
		cancel:        cancel,
		owner:         uuid.NewString(),
	}
	s.recordUsage = s.record
	return s
}

func (s *SeedanceSettlementService) Start() {
	if s == nil || s.store == nil || s.gateway == nil || s.accountRepo == nil {
		return
	}
	s.start.Do(func() {
		s.wg.Add(1)
		go s.run()
	})
}

func (s *SeedanceSettlementService) Stop() {
	if s == nil {
		return
	}
	s.stop.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

func (s *SeedanceSettlementService) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(seedanceSettlementScanInterval)
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

func (s *SeedanceSettlementService) scanOnce(ctx context.Context) {
	release, ok := s.tryAcquireScanLock(ctx)
	if !ok {
		return
	}
	defer release()

	scanCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	due, err := s.store.DueSeedanceSettlements(scanCtx, time.Now(), seedanceSettlementBatchSize)
	if err != nil {
		s.log.Warn("seedance.settlement_scan_failed", zap.Error(err))
		return
	}
	for _, member := range due {
		if scanCtx.Err() != nil {
			return
		}
		entry, err := parseSeedanceSettlementMember(member)
		if err != nil {
			s.log.Error("seedance.settlement_entry_invalid", zap.String("member", member), zap.Error(err))
			_ = s.store.RemoveSeedanceSettlement(scanCtx, member)
			continue
		}
		s.settle(scanCtx, entry)
	}
}

func (s *SeedanceSettlementService) tryAcquireScanLock(ctx context.Context) (func(), bool) {
	if s.leaderLock == nil {
		return func() {}, true
	}
	ok, err := s.leaderLock.TryAcquireLeaderLock(ctx, seedanceSettlementLeaderLock, s.owner, seedanceSettlementLockTTL)
	if err != nil {
		s.log.Warn("seedance.settlement_lock_failed", zap.Error(err))
		return nil, false
	}
	if !ok {
		return nil, false
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.leaderLock.ReleaseLeaderLock(releaseCtx, seedanceSettlementLeaderLock, s.owner)
	}, true
}

func (s *SeedanceSettlementService) schedule(ctx context.Context, entry SeedanceSettlementEntry, dueAt time.Time) {
	member, err := entry.member()
	if err == nil {
		err = s.store.ScheduleSeedanceSettlement(ctx, member, dueAt)
	}
	if err != nil {
		// 进不了索引的任务只能靠调用方查询计费——留错误日志，便于对账时找到它。
		s.log.Error("seedance.settlement_schedule_failed", zap.String("request_id", entry.TaskKey), zap.Error(err))
	}
}

func (s *SeedanceSettlementService) forget(ctx context.Context, entry SeedanceSettlementEntry) {
	member, err := entry.member()
	if err == nil {
		err = s.store.RemoveSeedanceSettlement(ctx, member)
	}
	if err != nil {
		// 移不掉只会多一次后台补查：届时抢不到计费标记，按已结清移出。
		s.log.Warn("seedance.settlement_forget_failed", zap.String("request_id", entry.TaskKey), zap.Error(err))
	}
}

func (s *SeedanceSettlementService) settleIfPending(ctx context.Context, entry SeedanceSettlementEntry, account *Account) {
	member, err := entry.member()
	if err != nil {
		return
	}
	pending, err := s.store.SeedanceSettlementPending(ctx, member)
	if err != nil {
		s.log.Warn("seedance.settlement_lookup_failed", zap.String("request_id", entry.TaskKey), zap.Error(err))
		return
	}
	if pending {
		s.settleWith(ctx, entry, account)
	}
}

// nextCheck：刚创建时 10 分钟后查；之后间隔约为任务年龄的一半，最短 10 分钟、最长 1 小时。
// 调用方正常轮询的任务早已结清；剩下的多是回调用户，而方舟任务最长可以排队/运行 72 小时。
func nextSeedanceSettlementCheck(now time.Time, pending *GrokVideoPendingBilling) time.Time {
	interval := seedanceSettlementFirstCheck
	if pending != nil {
		if age := GrokVideoE2EDuration(pending.CreatedAt, now); age/2 > interval {
			interval = age / 2
		}
	}
	return now.Add(min(interval, seedanceSettlementMaxInterval))
}

// settle 查一次上游状态，并据此计费、移出或改期。
func (s *SeedanceSettlementService) settle(ctx context.Context, entry SeedanceSettlementEntry) SeedanceBillingOutcome {
	return s.settleWith(ctx, entry, nil)
}

// settleWith：account 为 nil 时按创建时快照（或归属绑定）找持有任务的账号。
func (s *SeedanceSettlementService) settleWith(ctx context.Context, entry SeedanceSettlementEntry, account *Account) SeedanceBillingOutcome {
	log := s.log.With(zap.String("request_id", entry.TaskKey), zap.Int64("user_id", entry.UserID), zap.Int64("api_key_id", entry.APIKeyID))
	pending, _ := s.gateway.LoadGrokVideoPendingBilling(ctx, entry.TaskKey, entry.UserID, entry.APIKeyID)
	now := time.Now()
	if pending != nil && GrokVideoE2EDuration(pending.CreatedAt, now) > seedanceTaskRetention {
		log.Error("seedance.settlement_retention_exceeded", zap.String("created_at", pending.CreatedAt))
		s.forget(ctx, entry)
		return SeedanceBillingUnbillable
	}
	if account == nil {
		accountID := int64(0)
		if pending != nil {
			accountID = pending.AccountID
		}
		if accountID <= 0 {
			groupID := entry.GroupID
			accountID, _ = s.gateway.ResolveGrokMediaVideoRequestAccount(ctx, &groupID, entry.TaskKey, entry.UserID, entry.APIKeyID)
		}
		if accountID <= 0 {
			log.Error("seedance.settlement_owner_unknown")
			s.forget(ctx, entry)
			return SeedanceBillingUnbillable
		}
		loaded, err := s.accountRepo.GetByID(ctx, accountID)
		if err != nil || loaded == nil {
			log.Warn("seedance.settlement_account_unavailable", zap.Int64("account_id", accountID), zap.Error(err))
			s.schedule(ctx, entry, nextSeedanceSettlementCheck(now, pending))
			return SeedanceBillingRetry
		}
		account = loaded
	}
	httpStatus, status, err := s.gateway.querySeedanceTask(ctx, account, entry.TaskKey)
	switch {
	case err != nil || httpStatus >= 500 || httpStatus == 429:
		log.Warn("seedance.settlement_query_failed", zap.Int("http_status", httpStatus), zap.Error(err))
		s.schedule(ctx, entry, nextSeedanceSettlementCheck(now, pending))
		return SeedanceBillingRetry
	case httpStatus == 404:
		// 方舟任务记录保留 7 天；更早就查不到，说明记录已被删除，这笔再也无法计价。
		log.Error("seedance.settlement_task_gone")
		s.forget(ctx, entry)
		return SeedanceBillingUnbillable
	case httpStatus >= 300:
		log.Warn("seedance.settlement_query_rejected", zap.Int("http_status", httpStatus))
		s.schedule(ctx, entry, nextSeedanceSettlementCheck(now, pending))
		return SeedanceBillingRetry
	}
	observed := &OpenAIForwardResult{ResponseID: entry.TaskKey, UpstreamModel: status.Model, UpstreamTaskStatus: status.Status}
	if status.Status == "succeeded" {
		observed.Usage.OutputTokens = status.CompletionTokens
	}
	bill, snapshot, outcome := s.gateway.PrepareSeedanceCompletionBilling(ctx, log, entry.UserID, entry.APIKeyID, entry.TaskKey, observed)
	switch outcome {
	case SeedanceBillingReady:
		if err := s.recordUsage(ctx, entry, account, bill, snapshot); err != nil {
			log.Error("seedance.settlement_record_failed", zap.Error(err))
			if releaseErr := s.gateway.ReleaseGrokVideoBilling(ctx, entry.TaskKey, entry.UserID, entry.APIKeyID); releaseErr != nil {
				log.Error("seedance.settlement_claim_release_failed", zap.Error(releaseErr))
			}
			s.schedule(ctx, entry, nextSeedanceSettlementCheck(now, pending))
			return SeedanceBillingRetry
		}
		log.Info("seedance.settled_by_gateway", zap.Int("completion_tokens", bill.Usage.OutputTokens), zap.String("billing_model", bill.BillingModel))
		s.forget(ctx, entry)
	case SeedanceBillingSettled, SeedanceBillingUnbillable:
		s.forget(ctx, entry)
	default:
		s.schedule(ctx, entry, nextSeedanceSettlementCheck(now, pending))
	}
	return outcome
}

// record 用与请求时相同的入口（RecordUsage）落账。API Key 走鉴权中间件同一个加载路径（GetByKey），
// 分组、用户、逐模型倍率与请求时一致；订阅组的订阅也与中间件同样取当前有效的那份。
func (s *SeedanceSettlementService) record(ctx context.Context, entry SeedanceSettlementEntry, account *Account, bill *OpenAIForwardResult, pending *GrokVideoPendingBilling) error {
	if s.apiKeys == nil {
		return errors.New("api key service unavailable")
	}
	stored, err := s.apiKeys.GetByID(ctx, entry.APIKeyID)
	if err != nil {
		return err
	}
	apiKey, err := s.apiKeys.GetByKey(ctx, stored.Key)
	if err != nil {
		return err
	}
	var subscription *UserSubscription
	if apiKey.Group != nil && apiKey.Group.IsSubscriptionType() && s.subscriptions != nil && apiKey.User != nil {
		subscription, err = s.subscriptions.GetActiveSubscription(ctx, apiKey.User.ID, apiKey.Group.ID)
		if err != nil && !errors.Is(err, ErrSubscriptionNotFound) {
			return err
		}
	}
	quotaPlatform, originalModel := "", bill.Model
	if pending != nil {
		quotaPlatform = pending.QuotaPlatform
		originalModel = firstNonBlank(pending.OriginalModel, bill.Model)
	}
	if quotaPlatform == "" {
		quotaPlatform = QuotaPlatform(ctx, apiKey)
	}
	return s.gateway.RecordUsage(ctx, &OpenAIRecordUsageInput{
		Result:             bill,
		APIKey:             apiKey,
		User:               apiKey.User,
		Account:            account,
		Subscription:       subscription,
		InboundEndpoint:    SeedanceSettlementInboundEndpoint,
		UpstreamEndpoint:   SeedanceTasksPath,
		RequestPayloadHash: HashUsageRequestPayload([]byte(entry.TaskKey)),
		APIKeyService:      s.apiKeys,
		QuotaPlatform:      quotaPlatform,
		ChannelUsageFields: ChannelUsageFields{OriginalModel: originalModel, ChannelMappedModel: bill.Model},
	})
}
