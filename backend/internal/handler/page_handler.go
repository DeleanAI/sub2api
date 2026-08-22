package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PageHandler 对外提供自定义页面的读取接口。
// 页面与附件来自 PostgreSQL（issue #7）；这里不再碰磁盘，所以任何副本返回的内容都一致。
// 可见性仍由 custom_menu_items 设置驱动：只有挂在菜单上的 slug 才对外可见。
type PageHandler struct {
	pages          *service.CustomPageService
	settingService *service.SettingService
}

// NewPageHandler 创建页面读取处理器。
func NewPageHandler(pages *service.CustomPageService, settingService *service.SettingService) *PageHandler {
	return &PageHandler{pages: pages, settingService: settingService}
}

// GetPageContent serves raw markdown content for a given slug.
// GET /api/v1/pages/:slug
func (h *PageHandler) GetPageContent(c *gin.Context) {
	slug := c.Param("slug")
	if err := service.ValidateCustomPageSlug(slug); err != nil {
		response.BadRequest(c, "Invalid page slug")
		return
	}

	// Visibility check: slug must be configured in custom_menu_items
	// and the user must have permission based on visibility setting
	if !h.checkSlugVisibility(c, slug) {
		c.JSON(http.StatusNotFound, gin.H{"error": "page not found"})
		return
	}

	page, err := h.pages.GetPage(c.Request.Context(), slug)
	if err != nil {
		if errors.Is(err, service.ErrCustomPageNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "page not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read page"})
		return
	}

	c.Data(http.StatusOK, "text/markdown; charset=utf-8", []byte(page.Content))
}

// ListPages returns available page slugs.
// GET /api/v1/pages
func (h *PageHandler) ListPages(c *gin.Context) {
	pages, err := h.pages.ListPages(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	slugs := make([]string, 0, len(pages))
	for i := range pages {
		slugs = append(slugs, pages[i].Slug)
	}
	response.Success(c, slugs)
}

// ServePageImage serves a page asset.
// GET /api/v1/pages/:slug/images/*filename
// No JWT required (browser img tags can't carry tokens), but visibility is checked.
func (h *PageHandler) ServePageImage(c *gin.Context) {
	slug := c.Param("slug")
	filename := strings.TrimPrefix(c.Param("filename"), "/")

	if err := service.ValidateCustomPageSlug(slug); err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	if !h.checkImageSlugVisibility(c, slug) {
		c.Status(http.StatusNotFound)
		return
	}

	asset, err := h.pages.GetAsset(c.Request.Context(), slug, filename)
	if err != nil {
		if errors.Is(err, service.ErrCustomPageAssetNotFound) || errors.Is(err, service.ErrCustomPageInvalidAssetPath) {
			c.Status(http.StatusNotFound)
			return
		}
		c.Status(http.StatusInternalServerError)
		return
	}

	// ServeContent 负责 If-Modified-Since / Range；Content-Type 以存储的类型为准，不做嗅探。
	c.Header("Content-Type", asset.ContentType)
	http.ServeContent(c.Writer, c.Request, asset.Path, asset.UpdatedAt, bytes.NewReader(asset.Data))
}

// findSlugVisibility looks up the slug in custom_menu_items and returns (visibility, found).
func (h *PageHandler) findSlugVisibility(c *gin.Context, slug string) (string, bool) {
	if h.settingService == nil {
		return "", false
	}

	raw := h.settingService.GetCustomMenuItemsRaw(c.Request.Context())
	if raw == "" || raw == "[]" {
		return "", false
	}

	var items []struct {
		URL        string `json:"url"`
		PageSlug   string `json:"page_slug"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return "", false
	}

	for _, item := range items {
		itemSlug := item.PageSlug
		if itemSlug == "" && strings.HasPrefix(item.URL, "md:") {
			itemSlug = strings.TrimPrefix(item.URL, "md:")
		}
		if itemSlug == slug {
			return item.Visibility, true
		}
	}
	return "", false
}

// checkSlugVisibility verifies the slug is configured in custom_menu_items
// and the authenticated user has permission to view it.
func (h *PageHandler) checkSlugVisibility(c *gin.Context, slug string) bool {
	visibility, found := h.findSlugVisibility(c, slug)
	if !found {
		return false
	}
	if visibility == "admin" {
		role, _ := middleware2.GetUserRoleFromContext(c)
		return role == "admin"
	}
	return true
}

// checkImageSlugVisibility checks visibility for image requests (no JWT available).
// Only allows user-visible pages; admin-only pages are blocked.
func (h *PageHandler) checkImageSlugVisibility(c *gin.Context, slug string) bool {
	visibility, found := h.findSlugVisibility(c, slug)
	if !found {
		return false
	}
	return visibility != "admin"
}

// RegisterPageRoutes registers page routes on a router group.
func RegisterPageRoutes(v1 *gin.RouterGroup, h *PageHandler, jwtAuth gin.HandlerFunc, adminAuth gin.HandlerFunc, settingService *service.SettingService) {
	// Authenticated page content (JWT required + visibility check)
	pages := v1.Group("/pages")
	pages.Use(jwtAuth)
	{
		pages.GET("/:slug", h.GetPageContent)
	}

	// Images: no JWT (browser img tags can't carry tokens), visibility check in handler
	pageImages := v1.Group("/pages")
	{
		pageImages.GET("/:slug/images/*filename", h.ServePageImage)
	}

	// Admin-only: list all available pages
	adminPages := v1.Group("/pages")
	adminPages.Use(adminAuth)
	adminPages.Use(middleware2.AdminComplianceGuard(settingService))
	{
		adminPages.GET("", h.ListPages)
	}
}
