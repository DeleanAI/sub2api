package handler

import (
	"net/http"
	"strconv"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// SeedanceAssets 是方舟素材库接口（POST /?Action=…&Version=2024-01-01）。鉴权与账号调度同 Seedance 任务接口（Key 所在
// 分组里具备 seedance 能力的账号），限流走 RPM / Key 额度窗口 / 用户与账号并发。请求体与上游的回答原样透传；网关自己的
// 回答按方舟 OpenAPI 的格式（ResponseMetadata.Error）。素材接口不计费，每次调用记一条审计日志 seedance.asset_action
// （开启请求负载审计时，请求与响应体另由负载审计记录）。
func (h *OpenAIGatewayHandler) SeedanceAssets(c *gin.Context) {
	started := time.Now()
	action, version := c.Query("Action"), c.Query("Version")
	fail := func(status int, code, message string) {
		if code == "" {
			code = seedanceAssetStatusCode(status)
		}
		service.WriteSeedanceAssetError(c, action, version, &service.SeedanceAssetError{Status: status, Code: code, Message: message})
	}
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	if err := service.LookupSeedanceAssetAction(action, version); err != nil {
		service.WriteSeedanceAssetError(c, action, version, err)
		return
	}
	if !seedanceJSONContentType(c) {
		fail(http.StatusUnsupportedMediaType, "InvalidParameter", "Content-Type must be application/json")
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		fail(http.StatusUnauthorized, "", "Invalid API key")
		return
	}
	if !seedanceGroupAllowed(apiKey.Group) || !service.GroupAllowsImageGeneration(apiKey.Group) {
		fail(http.StatusForbidden, "", "Seedance assets require an OpenAI or composite group that is allowed to generate media")
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		fail(http.StatusInternalServerError, "", "User context not found")
		return
	}
	reqLog := requestLogger(c, "handler.openai_gateway.seedance_asset",
		zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", apiKey.ID), zap.Any("group_id", apiKey.GroupID), zap.String("action", action))
	if missing := h.missingResponsesDependencies(); len(missing) > 0 {
		reqLog.Error("seedance.asset_dependencies_missing", zap.Strings("missing_dependencies", missing))
		fail(http.StatusServiceUnavailable, "", "Service temporarily unavailable")
		return
	}
	setActualUpstreamEndpoint(c, service.SeedanceAssetUpstreamEndpoint(action))

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			fail(http.StatusRequestEntityTooLarge, "InvalidParameter", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		fail(http.StatusBadRequest, "InvalidParameter", "Failed to read request body")
		return
	}

	ctx := c.Request.Context()
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(ctx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(ctx, apiKey)); err != nil {
		reqLog.Info("seedance.asset_billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		fail(status, code, message)
		return
	}
	userRelease, err := h.concurrencyHelper.AcquireUserSlotWithWait(c, subject.UserID, subject.Concurrency, false, &streamStarted)
	if err != nil {
		reqLog.Warn("seedance.asset_user_slot_acquire_failed", zap.Error(err))
		status, _, code, message := concurrencyErrorResponse(err, "user")
		fail(status, code, message)
		return
	}
	if release := wrapReleaseOnDone(ctx, userRelease); release != nil {
		defer release()
	}

	account, accountRelease, ok := h.selectSeedanceAssetAccount(c, reqLog, apiKey, &streamStarted, fail)
	if !ok {
		return
	}
	defer accountRelease()
	writerSize := c.Writer.Size()
	upstreamStatus, err := h.gatewayService.ForwardSeedanceAsset(ctx, c, account, action, version, body)
	if err != nil {
		reqLog.Warn("seedance.asset_forward_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		if !service.IsResponseCommitted(c) && c.Writer.Size() == writerSize {
			fail(http.StatusBadGateway, "UpstreamError", "Upstream request failed")
		}
		return
	}
	reqLog.Info("seedance.asset_action", zap.Int64("account_id", account.ID), zap.Int("upstream_status", upstreamStatus),
		zap.Int64("latency_ms", time.Since(started).Milliseconds()))
}

// selectSeedanceAssetAccount 由调度器在 Key 的分组里选一个具备 seedance 能力的账号，并占住它的并发槽位。
func (h *OpenAIGatewayHandler) selectSeedanceAssetAccount(c *gin.Context, reqLog *zap.Logger, apiKey *service.APIKey, streamStarted *bool,
	fail func(status int, code, message string)) (*service.Account, func(), bool) {
	ctx := service.WithOpenAIProfitControlSuppressed(c.Request.Context())
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(ctx, apiKey.GroupID, "", "", "", nil,
		service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilitySeedance, false, false, false, service.PlatformOpenAI)
	if selection != nil && selection.Acquired {
		selection.ReleaseFunc = wrapReleaseOnDone(ctx, selection.ReleaseFunc)
	}
	if err != nil || selection == nil || selection.Account == nil {
		if selection != nil && selection.Acquired && selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		reqLog.Warn("seedance.asset_account_unavailable", zap.Error(err))
		markOpsRoutingCapacityLimited(c)
		fail(http.StatusServiceUnavailable, "", "No eligible Seedance accounts")
		return nil, nil, false
	}
	setOpsSelectedAccount(c, selection.Account.ID, selection.Account.Platform)
	release, result := h.acquireOpenAIAccountSlot(c, apiKey.GroupID, "", selection, false, streamStarted, reqLog,
		func(status int, _, code, message string) { fail(status, code, message) })
	switch result {
	case openAISlotAcquireOK:
		if release == nil {
			release = func() {}
		}
		return selection.Account, release, true
	case openAISlotAcquireProfitVetoed:
		// 媒体调用豁免利润门（上面的 ctx 已标记），这里只防御性兜底。
		fail(http.StatusServiceUnavailable, "", "No eligible Seedance accounts")
	}
	return nil, nil, false
}

// seedanceAssetStatusCode 是共用组件（鉴权、限流、并发）没给错误码时，按 HTTP 状态补的方舟风格错误码。
func seedanceAssetStatusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "InvalidParameter"
	case http.StatusUnauthorized:
		return "InvalidAuthorization"
	case http.StatusForbidden:
		return "AccessDenied"
	case http.StatusTooManyRequests:
		return "TooManyRequests"
	case http.StatusServiceUnavailable:
		return "ServiceUnavailable"
	case statusClientClosedRequest:
		return "RequestCanceled"
	default:
		return "InternalError"
	}
}
