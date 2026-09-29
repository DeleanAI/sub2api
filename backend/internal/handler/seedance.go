package handler

import (
	"mime"
	"net/http"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// seedanceGroupAllowed：Seedance（任务与素材库）只对 OpenAI 或组合分组开放——Seedance 账号是 OpenAI 平台的 API Key 账号。
func seedanceGroupAllowed(group *service.Group) bool {
	return group != nil && (group.Platform == service.PlatformOpenAI || group.Platform == service.PlatformComposite)
}

// seedanceJSONContentType：带请求体的 Seedance 请求只收 JSON；没写 Content-Type 的按 JSON 处理。
func seedanceJSONContentType(c *gin.Context) bool {
	if c.Request.Method != http.MethodPost || c.GetHeader("Content-Type") == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// SeedanceTasks exposes Ark's native asynchronous video task protocol.
func (h *OpenAIGatewayHandler) SeedanceTasks(c *gin.Context) {
	if !seedanceJSONContentType(c) {
		h.errorResponse(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Seedance requires application/json")
		return
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || !seedanceGroupAllowed(key.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Seedance requires an OpenAI or composite group")
		return
	}
	endpoint := service.SeedanceEndpointCreate
	setActualUpstreamEndpoint(c, EndpointSeedanceTasks)
	taskID := ""
	if c.Request.Method != http.MethodPost {
		taskID = service.SeedanceTaskKey(c.Param("task_id"))
		endpoint = service.SeedanceEndpointStatus
		if c.Request.Method == http.MethodDelete {
			endpoint = service.SeedanceEndpointDelete
		}
	}
	h.handleGrokMedia(c, endpoint, taskID)
}

// seedanceSettlementEntry 是本次请求对应的结算条目（任务身份）：创建、查询、删除三处拼出的值相同。
func seedanceSettlementEntry(key *service.APIKey, subject middleware.AuthSubject, taskKey string) service.SeedanceSettlementEntry {
	entry := service.SeedanceSettlementEntry{TaskKey: taskKey, UserID: subject.UserID, APIKeyID: key.ID}
	if key.GroupID != nil {
		entry.GroupID = *key.GroupID
	}
	return entry
}
