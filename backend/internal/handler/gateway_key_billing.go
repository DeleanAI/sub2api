package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// keyBillingInfoSchemaVersion 直接取自 service.KeyBillingSchemaVersion：版本号的
// 含义与"什么时候该 +1"的规则写在那一处，这里只是引用。
//
// fork 新增的 model_rate_multipliers、以及可选 ?model= 带出的 model /
// model_rate_multiplier / matched_model_pattern 都是纯增量可选字段，按规则不升版本。
const keyBillingInfoSchemaVersion = service.KeyBillingSchemaVersion

type keyBillingInfoResponse struct {
	Object                  string                             `json:"object"`
	SchemaVersion           int                                `json:"schema_version"`
	BillingScope            string                             `json:"billing_scope"`
	GroupRateMultiplier     float64                            `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64                           `json:"user_rate_multiplier,omitempty"`
	ResolvedRateMultiplier  float64                            `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool                               `json:"peak_rate_enabled"`
	PeakStart               *string                            `json:"peak_start,omitempty"`
	PeakEnd                 *string                            `json:"peak_end,omitempty"`
	PeakRateMultiplier      *float64                           `json:"peak_rate_multiplier,omitempty"`
	AppliedPeakMultiplier   *float64                           `json:"applied_peak_multiplier,omitempty"`
	ModelRateMultipliers    []service.GroupModelRateMultiplier `json:"model_rate_multipliers"`
	Model                   *string                            `json:"model,omitempty"`
	ModelRateMultiplier     *float64                           `json:"model_rate_multiplier,omitempty"`
	MatchedModelPattern     *string                            `json:"matched_model_pattern,omitempty"`
	EffectiveRateMultiplier float64                            `json:"effective_rate_multiplier"`
	Timezone                *string                            `json:"timezone,omitempty"`
	ObservedAt              time.Time                          `json:"observed_at"`
}

// KeyBillingInfo returns the token billing multiplier effective for the authenticated API key.
// GET /v1/sub2api/billing[?model=<model>]
//
// 不带 model 时 effective_rate_multiplier = resolved × peak（逐模型因子未知，不折入）；
// 带 model 时再乘入该模型命中的分组逐模型倍率，与扣费及利润门同一公式。
func (h *GatewayHandler) KeyBillingInfo(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if h.cfg != nil && h.cfg.RunMode == config.RunModeSimple {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Billing information is not supported in simple mode")
		return
	}
	if apiKey.GroupID == nil {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	if apiKey.Group == nil {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Billing information is unavailable")
		return
	}

	resolvedRate, ok := h.resolveKeyBillingRate(c, apiKey)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "Billing information is unavailable")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, buildKeyBillingInfo(apiKey, resolvedRate, timezone.Now(), strings.TrimSpace(c.Query("model"))))
}

func (h *GatewayHandler) resolveKeyBillingRate(c *gin.Context, apiKey *service.APIKey) (float64, bool) {
	groupRate := apiKey.Group.RateMultiplier
	switch apiKey.Group.Platform {
	case service.PlatformOpenAI, service.PlatformGrok:
		if h.openAIGatewayService == nil {
			return 0, false
		}
		return h.openAIGatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), apiKey.UserID, *apiKey.GroupID, groupRate), true
	default:
		if h.gatewayService == nil {
			return 0, false
		}
		return h.gatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), apiKey.UserID, *apiKey.GroupID, groupRate), true
	}
}

func buildKeyBillingInfo(apiKey *service.APIKey, resolvedRate float64, now time.Time, model string) keyBillingInfoResponse {
	group := apiKey.Group
	groupRate := group.RateMultiplier
	var userRate *float64
	if resolvedRate != groupRate {
		userRate = &resolvedRate
	}
	appliedPeak := group.PeakMultiplierAt(now)

	modelRateMultipliers := group.ModelRateMultipliers
	if modelRateMultipliers == nil {
		// 总是输出数组（而不是 null），客户端无需区分"未配置"和"字段缺失"。
		modelRateMultipliers = []service.GroupModelRateMultiplier{}
	}
	response := keyBillingInfoResponse{
		Object:                  "sub2api.key_billing",
		SchemaVersion:           keyBillingInfoSchemaVersion,
		BillingScope:            "token",
		GroupRateMultiplier:     groupRate,
		UserRateMultiplier:      userRate,
		ResolvedRateMultiplier:  resolvedRate,
		PeakRateEnabled:         group.PeakRateEnabled,
		ModelRateMultipliers:    modelRateMultipliers,
		EffectiveRateMultiplier: service.EffectiveDownstreamMultiplier(group, resolvedRate, now, model),
		ObservedAt:              now.UTC(),
	}
	if group.PeakRateEnabled {
		response.PeakStart = &group.PeakStart
		response.PeakEnd = &group.PeakEnd
		response.PeakRateMultiplier = &group.PeakRateMultiplier
		response.AppliedPeakMultiplier = &appliedPeak
		tz := timezone.Location().String()
		response.Timezone = &tz
	}
	if model != "" {
		response.Model = &model
		modelRate := service.ResolveGroupModelRateMultiplier(group, model)
		response.ModelRateMultiplier = &modelRate
		if entry, matched := group.ModelRateMultiplierFor(model); matched {
			pattern := entry.ModelPattern
			response.MatchedModelPattern = &pattern
		}
	}
	return response
}
