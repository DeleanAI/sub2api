package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// 国产供应商（kimi/zhipu/deepseek）的响应式冷却辅助。
//
// 与 openai/anthropic 不同：
//   - 余额不足是「可恢复」状态（充值/检测恢复后自动重新调度），不能走 handleAuthError
//     永久置 status=error。这里改为 SetTempUnschedulable，由 CN 余额检测周期任务
//     （cn_provider_balance_check_service.go）在余额恢复后 ClearTempUnschedulable。
//   - Coding Plan 滚动窗口耗尽（429）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），而非默认的秒级兜底。

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

const (
	// Qwen Token Plan is a one-time allowance. These markers are written only
	// after the inference endpoint explicitly reports the allowance as exhausted.
	qwenTokenPlanExhaustedExtraKey       = "qwen_token_plan_exhausted"
	qwenTokenPlanExhaustedAtExtraKey     = "qwen_token_plan_exhausted_at"
	qwenTokenPlanExhaustedReasonExtraKey = "qwen_token_plan_exhausted_reason"
	// qwenTokenPlanExhaustedSinceExtraKey 记录"第一次看到额度耗尽"的时刻。
	// 永久停调的判据是这个状态**熬过了** qwenTokenPlanExhaustionConfirmWindow，
	// 而不是看到一次报文就下结论（见 handleQwenTokenPlanExhausted 的说明）。
	qwenTokenPlanExhaustedSinceExtraKey = "qwen_token_plan_exhausted_since"
)

// cnBalanceLowReasonPrefix 是余额不足临时停调 reason 的稳定前缀。
// 周期余额检测任务据此识别「是我们停调的」并在余额恢复后安全清除——不会误清
// 其他子系统（阈值/限流/401）写入的临时停调。
const cnBalanceLowReasonPrefix = "cn_balance_low"

const kimiConcurrentRequestLimitMessage = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."

const cnConcurrencyLimitReasonPrefix = "cn_concurrency_limit"

func isCNProviderConcurrencyLimit403(account *Account, upstreamMsg string) bool {
	return account != nil && account.Platform == PlatformKimi &&
		strings.TrimSpace(upstreamMsg) == kimiConcurrentRequestLimitMessage
}

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	reason := cnConcurrencyLimitReasonPrefix + ": " + kimiConcurrentRequestLimitMessage
	s.notifyAccountSchedulingBlocked(account, until, cnConcurrencyLimitReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("cn_concurrency_limit_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_concurrency_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceLowReason 构造余额不足临时停调的 reason（带稳定前缀）。
func cnBalanceLowReason(upstreamMsg string) string {
	if upstreamMsg = strings.TrimSpace(upstreamMsg); upstreamMsg != "" {
		return cnBalanceLowReasonPrefix + ": " + upstreamMsg
	}
	return cnBalanceLowReasonPrefix + ": 余额不足，账号临时停调"
}

// cnProviderResponseIndicatesInsufficientBalance 通过响应体文案识别余额不足
// （智谱 payg 无独立余额端点，仅能靠响应文案识别）。
func cnProviderResponseIndicatesInsufficientBalance(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "余额不足") ||
		strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "insufficient_credit") ||
		strings.Contains(s, "balance is not enough") ||
		strings.Contains(s, "no enough balance")
}

// handleCNProviderInsufficientBalance 把余额不足标记为可恢复的临时停调：
// 写入 balance_low 快照 + SetTempUnschedulable 一个余额检测周期，
// 由周期任务在余额恢复后清除。返回前已通知调度阻塞。
func (s *RateLimitService) handleCNProviderInsufficientBalance(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	msg := cnBalanceLowReason(upstreamMsg)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}); err != nil {
		slog.Warn("cn_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}

	until := time.Now().Add(s.cnBalanceCooldownDuration())
	s.notifyAccountSchedulingBlocked(account, until, "cn_insufficient_balance")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("cn_balance_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_insufficient_balance",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceCooldownDuration 返回余额不足临时停调的持续时长（= 2× 余额检测周期，
// 默认 20 分钟）。周期任务会在余额恢复后提前清除，故此处只需保证冷却覆盖到下一次
// 周期检测即可。
func (s *RateLimitService) cnBalanceCooldownDuration() time.Duration {
	minutes := 10
	if s != nil && s.cfg != nil {
		if cfgMin := s.cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}

// cnProviderQuotaSnapshotReset 读取 Coding Plan 账号快照中最早一个仍在未来的窗口
// 重置时间（5h / weekly）。429 多数由 5h 滚动窗口触发，取较早的重置点可避免
// 把账号冷却到 weekly 重置（可达数天）的过度停调；如果确是 weekly 窗口耗尽，
// 周期额度探测刷新快照后阈值评估会再次停调到正确的时间点。
// 无快照或均已过期返回 nil。
func cnProviderQuotaSnapshotReset(account *Account, now time.Time) *time.Time {
	if account == nil || !account.IsCNProvider() || !account.IsCodingPlan() || len(account.Extra) == 0 {
		return nil
	}
	provider := account.Platform
	var earliest *time.Time
	for _, suffix := range []string{cnExtraSuffix5hReset, cnExtraSuffixWeeklyReset} {
		t := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, suffix)])
		if t == nil || !t.After(now) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// applyCNProviderReactive429 处理国产供应商的 429 响应。
// 返回 true 表示已处理（调用方应 return），false 表示未命中、继续走默认 429 逻辑。
func (s *RateLimitService) applyCNProviderReactive429(
	ctx context.Context,
	account *Account,
	headers http.Header,
	responseBody []byte,
) bool {
	if account == nil {
		return false
	}
	// Token Plan 的判定放在 IsCNProvider 之前。
	//
	// IsCNProvider 是按 platform 字段判的，而生产上 121 个 Token Plan 账号全部登记在
	// platform=anthropic 下（base_url 指向阿里的 Anthropic 兼容端点），一个都过不了这道门。
	// 先按 platform 分流、再判 Token Plan，等于让这个功能对它唯一的目标群体永远不生效。
	// Token Plan 是"账号连的是哪个上游"的属性，不是"它被归到哪个平台"的属性。
	if account.IsTokenPlan() {
		// 一次性额度：文档化的耗尽响应是带明确额度措辞的 429。普通 429、凭据错误、
		// 传输失败都不能变成永久停调（判据见 qwenTokenPlanQuotaExhausted）。
		if qwenTokenPlanQuotaExhausted(responseBody) {
			s.handleQwenTokenPlanExhausted(ctx, account, responseBody)
			return true
		}
		return false
	}
	if !account.IsCNProvider() {
		return false
	}
	// 1) 余额不足文案：可恢复临时停调（含智谱 payg 这类无余额端点的场景）。
	if cnProviderResponseIndicatesInsufficientBalance(responseBody) {
		s.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(responseBody))
		return true
	}
	// 2) Coding Plan 窗口耗尽：冷却到快照中最早的窗口重置点（见
	// cnProviderQuotaSnapshotReset：429 多由 5h 窗口触发，取较早点避免过度停调）。
	if account.IsCodingPlan() {
		if until := cnProviderQuotaSnapshotReset(account, time.Now()); until != nil {
			s.notifyAccountSchedulingBlocked(account, *until, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, *until); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("cn_coding_plan_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", *until,
			)
			return true
		}
	}
	return false
}

// qwenTokenPlanQuotaExhausted 判断这次 429 是不是"Token Plan 一次性额度真的用完了"。
//
// 判据来自生产实测，而不是对文案的猜测。在 may 生产库的 ops_error_logs 里，
// 121 个 Token Plan 账号一共命中过 14206 次额度耗尽，报文只有一种形状：
//
//	{"request_id":"…","code":"Throttling.AllocationQuota",
//	 "message":"Your token-plan quota has been exhausted."}
//
// （SSE 里是 `event:error` + `data:{…}`；少数几条 code 为裸 "Throttling"。）
// 同一批数据里出现的其它 429 全都是临时限流，必须放行：
//
//	Throttling.ResourceExhausted  "An error occurred in model serving, error message is: [Too many requests.]"
//	1302                          "您的账户已达到速率限制，请您控制请求频率"
//	1308                          "已达到 5 小时的使用上限。您的限额将在 2026-09-07 18:01:51 重置。"
//
// 注意 1308：**带重置时间的窗口限额**是真实存在且高频的临时形态。所以"报文里说了会在
// 某时重置"是**临时**的证据，绝不能反过来当成永久的证据。
//
// 另外三条按阿里云错误码文档（help.aliyun.com/zh/model-studio/error-code）保留为永久，
// 它们语义无歧义，只是我们的流量里还没出现过：
//
//	"month allocated quota exceeded"        Coding Plan 当月额度用完，等下个订阅周期
//	"free tier has been exhausted"          免费额度用完，需充值
//	AllocationQuota.FreeTierOnly            同上（错误码形式）
//
// 反过来，下面这些**不能**判为永久——文档明确它们是 TPS/TPM 限流，降频或提额即可恢复：
//
//	"usage allocated quota exceeded" / 裸 "allocated quota exceeded"
//	"insufficient_quota" / "You exceeded your current quota"
//	Throttling.RateQuota / Throttling.BurstRate / Throttling.Concurrency / Throttling.ResourceExhausted
//
// 误判的代价是不对称的：把临时限流判成永久，会把一个健康账号**不可逆**地踢出调度
// （handleQwenTokenPlanExhausted 明确说这个状态永不自动清除），而漏判只是这一次请求
// 走了普通 429 退避、下次再试。所以这里只认已经证实过的措辞，宁可漏判。
func qwenTokenPlanQuotaExhausted(responseBody []byte) bool {
	if len(responseBody) == 0 {
		return false
	}

	// 只看上游 error.message，不在整个响应体上做子串匹配：响应体里可能有账号自己的
	// base_url（含 token-plan 字样）、中间件的 HTML 错误页、被回显的请求内容。
	// 在整块字节上找关键词，等于让这些无关内容也能把账号永久停调。
	message := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(responseBody)))
	if message == "" {
		message = strings.ToLower(string(responseBody))
	}

	// 说了什么时候恢复 = 临时。放在最前面，任何后续判据都不能推翻它。
	if qwenTokenPlanMessageStatesRecovery(message) {
		return false
	}
	if qwenTokenPlanTransientRateLimit(message) {
		return false
	}

	// 生产实测的唯一形态。
	if strings.Contains(message, "token-plan") && strings.Contains(message, "exhausted") {
		return true
	}
	// 文档里语义无歧义的两种真耗尽。
	if strings.Contains(message, "month allocated quota exceeded") {
		return true
	}
	if strings.Contains(message, "free tier has been exhausted") {
		return true
	}
	return false
}

// qwenTokenPlanMessageStatesRecovery 报告报文是否自称会在某个时间点恢复。
//
// 生产里 1308 就是这种：「已达到 5 小时的使用上限。您的限额将在 2026-09-07 18:01:51 重置。」
// 会恢复的东西不该被永久停调；这条判据优先于所有"额度用完"的措辞判据。
func qwenTokenPlanMessageStatesRecovery(message string) bool {
	for _, marker := range []string{"will reset", "reset at", "将在", "重置", "稍后重试", "please retry", "retry later", "try again"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// qwenTokenPlanTransientRateLimit 列出确定属于临时限流的措辞。
//
// 覆盖阿里云文档里的 Throttling.* 家族与我们生产里实际出现过的中文限流文案。
func qwenTokenPlanTransientRateLimit(message string) bool {
	markers := []string{
		// 阿里云错误码文档：均为可恢复的限流
		"requests rate limit",
		"api-key requests rate limit",
		"request rate increased too quickly",
		"too many requests",
		"too many concurrent requests",
		"you have exceeded your request limit",
		"throttling.ratequota",
		"throttling.burstrate",
		"throttling.concurrency",
		"throttling.resourceexhausted",
		// TPS/TPM 限流：文档明确"提额或降频即可恢复"，不是一次性额度耗尽
		"usage allocated quota exceeded",
		"insufficient_quota",
		"exceeded your current quota",
		// 生产实测的中文限流（1302 / 1308）
		"速率限制",
		"请求频率",
		"使用上限",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// qwenTokenPlanExhaustionConfirmWindow 是"确认额度真的用完了"要观察的时长。
//
// 这个数字是量出来的，不是拍的。may 生产库里 121 个 Token Plan 账号都命中过
// "Your token-plan quota has been exhausted."，但它们分成截然不同的两拨：
//
//	25 个恢复了：平均 48 秒后就有成功请求，**最长 367 秒（6.1 分钟）**
//	96 个没恢复：此后再没有一次成功请求，持续报错平均 28.2 小时
//
// 报文本身区分不了这两拨（两边都是同一句话），错误码也区分不了
// （Throttling.AllocationQuota 在 84 个没恢复的和 20 个恢复了的账号上都出现）。
// 唯一分得开的信号是**这个状态扛不扛得过十分钟**。
//
// 取 15 分钟：比实测最长恢复时间（6.1 分钟）留一倍以上余量；对真的用完的账号
// 也只是晚 15 分钟停调，相对它们平均 28 小时的枯竭期可以忽略。
const qwenTokenPlanExhaustionConfirmWindow = 15 * time.Minute

// qwenTokenPlanExhaustionRetryInterval 是观察期内每次冷却多久，与"多久算确认耗尽"是
// 两件事，必须分开。
//
// 早先这两者是同一个值：第一次 429 就冷却到 since+15min。后果是那 25 个平均 48 秒
// 就自愈的账号被白晾满 15 分钟——比改动之前（不冷却、直接 failover）还差。
// 判定要慢（避免误杀），重试要快（避免白白损失容量），这是两个相反的诉求。
//
// 90 秒：高于实测平均恢复 48 秒，于是在观察窗内会重试约 90s/180s/…/840s 共 9 次，
// 实测最长恢复 367 秒也能在它发生后 90 秒内被抓到；同时不会把已枯竭的账号
// 每几秒打一次。
const qwenTokenPlanExhaustionRetryInterval = 90 * time.Second

// handleQwenTokenPlanExhausted 处理 Token Plan 账号的额度耗尽 429。
//
// 关键：**第一次看到不下永久结论**。先记下时刻并临时冷却到观察窗结束；只有当这个
// 状态熬过了 qwenTokenPlanExhaustionConfirmWindow 仍在报，才永久停调
// （这个状态不会自动清除，只能人工在后台恢复）。
//
// 这样做是因为同一句报文既来自"这一周的额度真的花光了"，也来自"这一刻的窗口打满了"，
// 而后者在生产里六分钟内就自己好了。看一次报文就永久停调，会把 25/121 的健康账号
// 不可逆地踢出调度池。
func (s *RateLimitService) handleQwenTokenPlanExhausted(ctx context.Context, account *Account, responseBody []byte) {
	now := time.Now().UTC()
	reason := qwenTokenPlanExhaustionReason(responseBody)
	since, hasSince := qwenTokenPlanExhaustedSince(account)
	if !hasSince {
		since = now
	}

	if now.Sub(since) < qwenTokenPlanExhaustionConfirmWindow {
		// 观察期内：只记时刻 + 临时冷却，让调度器在窗口结束后自己再试一次。
		// 成功一次就会走 ClearRateLimit，连带把这个标记清掉。
		if !hasSince {
			if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
				qwenTokenPlanExhaustedSinceExtraKey:  since.Format(time.RFC3339),
				qwenTokenPlanExhaustedReasonExtraKey: reason,
			}); err != nil {
				slog.Warn("qwen_token_plan_exhaustion_mark_failed", "account_id", account.ID, "error", err)
			}
		}
		// 只冷却一个重试间隔，不是冷却到观察窗结束：临时限流的账号要能尽快回到调度池。
		// 不越过确认时刻，避免最后一次重试落在窗口之外白等。
		deadline := since.Add(qwenTokenPlanExhaustionConfirmWindow)
		until := now.Add(qwenTokenPlanExhaustionRetryInterval)
		if until.After(deadline) {
			until = deadline
		}
		s.notifyAccountSchedulingBlocked(account, until, "qwen_token_plan_exhausted_observing")
		if err := s.accountRepo.SetRateLimited(ctx, account.ID, until); err != nil {
			slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
		}
		slog.Info("qwen_token_plan_exhaustion_observing",
			"account_id", account.ID,
			"since", since,
			"retry_at", until,
			"confirm_at", deadline,
			"note", "多数临时窗口在 6 分钟内自行恢复；熬过观察窗仍在报才判定为真耗尽")
		return
	}

	// 熬过观察窗仍在报：判定为一次性额度真的用完，永久停调。
	updates := map[string]any{
		qwenTokenPlanExhaustedExtraKey:       true,
		qwenTokenPlanExhaustedAtExtraKey:     now.Format(time.RFC3339),
		qwenTokenPlanExhaustedSinceExtraKey:  since.Format(time.RFC3339),
		qwenTokenPlanExhaustedReasonExtraKey: reason,
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, updates); err != nil {
		slog.Warn("qwen_token_plan_exhaustion_mark_failed", "account_id", account.ID, "error", err)
	}

	s.notifyAccountSchedulingBlocked(account, time.Time{}, "qwen_token_plan_exhausted")
	if err := s.accountRepo.SetSchedulable(ctx, account.ID, false); err != nil {
		slog.Warn("qwen_token_plan_pause_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Warn("qwen_token_plan_exhausted",
		"account_id", account.ID,
		"observed_since", since,
		"observed_for", now.Sub(since).String())
}

// qwenTokenPlanExhaustedSince 读回"第一次看到额度耗尽"的时刻。
func qwenTokenPlanExhaustedSince(account *Account) (time.Time, bool) {
	if account == nil {
		return time.Time{}, false
	}
	raw, ok := account.Extra[qwenTokenPlanExhaustedSinceExtraKey].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		slog.Warn("qwen_token_plan_exhausted_since_unparsable",
			"account_id", account.ID, "value", raw, "error", err)
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func qwenTokenPlanExhaustionReason(responseBody []byte) string {
	message := strings.TrimSpace(extractUpstreamErrorMessage(responseBody))
	if message == "" {
		message = strings.TrimSpace(string(responseBody))
	}
	message = strings.Join(strings.Fields(message), " ")
	message = sanitizeUpstreamErrorMessage(message)
	message = truncateForLog([]byte(message), 512)
	if message == "" {
		return "Qwen Token Plan quota exhausted"
	}
	return "Qwen Token Plan quota exhausted: " + message
}
