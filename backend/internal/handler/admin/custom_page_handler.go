package admin

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// CustomPageHandler 管理端自定义页面接口：页面正文与附件都写进 PostgreSQL，
// 这是多副本部署下自定义页面唯一的写入口（issue #7）。审计由 /admin 组的中间件统一记录。
type CustomPageHandler struct {
	pages *service.CustomPageService
}

// NewCustomPageHandler 创建管理端自定义页面处理器。
func NewCustomPageHandler(pages *service.CustomPageService) *CustomPageHandler {
	return &CustomPageHandler{pages: pages}
}

// SaveCustomPageRequest PUT /admin/pages/:slug 请求体。
type SaveCustomPageRequest struct {
	Content *string `json:"content" binding:"required"`
}

// List 列出全部页面及限额。
// GET /api/v1/admin/pages
func (h *CustomPageHandler) List(c *gin.Context) {
	pages, err := h.pages.ListPages(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]dto.CustomPageSummary, 0, len(pages))
	for i := range pages {
		out = append(out, dto.CustomPageSummaryFromService(&pages[i]))
	}
	response.Success(c, dto.CustomPageListResponse{Pages: out, Limits: dto.CustomPageLimitsFromService()})
}

// Get 读取页面正文。
// GET /api/v1/admin/pages/:slug
func (h *CustomPageHandler) Get(c *gin.Context) {
	page, err := h.pages.GetPage(c.Request.Context(), c.Param("slug"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.CustomPageFromService(page))
}

// Save 新建或覆盖页面正文。
// PUT /api/v1/admin/pages/:slug
func (h *CustomPageHandler) Save(c *gin.Context) {
	var req SaveCustomPageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	page, err := h.pages.SavePage(c.Request.Context(), c.Param("slug"), *req.Content, actorIDFromContext(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.CustomPageFromService(page))
}

// Delete 删除页面及其附件。
// DELETE /api/v1/admin/pages/:slug
func (h *CustomPageHandler) Delete(c *gin.Context) {
	if err := h.pages.DeletePage(c.Request.Context(), c.Param("slug")); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "page deleted"})
}

// ListAssets 列出页面附件元数据。
// GET /api/v1/admin/pages/:slug/assets
func (h *CustomPageHandler) ListAssets(c *gin.Context) {
	assets, err := h.pages.ListAssets(c.Request.Context(), c.Param("slug"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]dto.CustomPageAsset, 0, len(assets))
	for i := range assets {
		out = append(out, dto.CustomPageAssetFromService(&assets[i]))
	}
	response.Success(c, out)
}

// SaveAsset 上传或覆盖附件。支持两种请求体：
//   - multipart/form-data，取第一个文件字段（字段名任意），Content-Type 取自该 part；
//   - 其它任意 Content-Type 的原始字节流。
//
// PUT /api/v1/admin/pages/:slug/assets/*path
func (h *CustomPageHandler) SaveAsset(c *gin.Context) {
	data, contentType, err := readAssetBody(c)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	asset, err := h.pages.SaveAsset(c.Request.Context(), c.Param("slug"), assetPathParam(c), contentType, data)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.CustomPageAssetFromService(asset))
}

// DeleteAsset 删除附件。
// DELETE /api/v1/admin/pages/:slug/assets/*path
func (h *CustomPageHandler) DeleteAsset(c *gin.Context) {
	if err := h.pages.DeleteAsset(c.Request.Context(), c.Param("slug"), assetPathParam(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "asset deleted"})
}

func assetPathParam(c *gin.Context) string {
	return strings.TrimPrefix(c.Param("path"), "/")
}

func actorIDFromContext(c *gin.Context) *int64 {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		return nil
	}
	id := subject.UserID
	return &id
}

// readAssetBody 读取附件内容。读取上限取服务层声明的 MaxCustomPageAssetSize（多读一个字节用于判定超限），
// 超限判定本身仍由服务层完成，这里只是不把超大请求整个读进内存。
func readAssetBody(c *gin.Context) ([]byte, string, error) {
	limit := int64(service.MaxCustomPageAssetSize) + 1
	mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if mediaType == "multipart/form-data" {
		reader, err := c.Request.MultipartReader()
		if err != nil {
			return nil, "", service.ErrCustomPageAssetEmpty
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return nil, "", service.ErrCustomPageAssetEmpty
			}
			if err != nil {
				return nil, "", service.ErrCustomPageAssetEmpty
			}
			if part.FileName() == "" {
				_ = part.Close()
				continue
			}
			data, err := io.ReadAll(io.LimitReader(part, limit))
			_ = part.Close()
			if err != nil {
				return nil, "", err
			}
			return data, part.Header.Get("Content-Type"), nil
		}
	}

	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			// 请求体已超过附件上限，用服务层的错误定义统一表述。
			return nil, "", service.ErrCustomPageAssetTooLarge
		}
		return nil, "", infraerrors.BadRequest("CUSTOM_PAGE_ASSET_BODY_UNREADABLE", "failed to read request body: "+err.Error())
	}
	return data, c.GetHeader("Content-Type"), nil
}
