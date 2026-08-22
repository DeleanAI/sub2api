package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// customPageRepoMem 内存仓储，覆盖管理端接口需要的全部方法。
type customPageRepoMem struct {
	pages  map[string]*service.CustomPage
	assets map[string]map[string]*service.CustomPageAsset
}

func newCustomPageRepoMem() *customPageRepoMem {
	return &customPageRepoMem{pages: map[string]*service.CustomPage{}, assets: map[string]map[string]*service.CustomPageAsset{}}
}

func (m *customPageRepoMem) GetPage(_ context.Context, slug string) (*service.CustomPage, error) {
	page, ok := m.pages[slug]
	if !ok {
		return nil, service.ErrCustomPageNotFound
	}
	copied := *page
	return &copied, nil
}

func (m *customPageRepoMem) ListPages(context.Context) ([]service.CustomPageSummary, error) {
	out := make([]service.CustomPageSummary, 0, len(m.pages))
	for slug, page := range m.pages {
		out = append(out, service.CustomPageSummary{Slug: slug, ContentSize: page.ContentSize, AssetCount: len(m.assets[slug]), UpdatedBy: page.UpdatedBy})
	}
	return out, nil
}

func (m *customPageRepoMem) CountPages(context.Context) (int, error) { return len(m.pages), nil }

func (m *customPageRepoMem) UpsertPage(_ context.Context, page *service.CustomPage) error {
	copied := *page
	m.pages[page.Slug] = &copied
	return nil
}

func (m *customPageRepoMem) DeletePage(_ context.Context, slug string) error {
	if _, ok := m.pages[slug]; !ok {
		return service.ErrCustomPageNotFound
	}
	delete(m.pages, slug)
	delete(m.assets, slug)
	return nil
}

func (m *customPageRepoMem) GetAsset(_ context.Context, slug, assetPath string) (*service.CustomPageAsset, error) {
	asset, ok := m.assets[slug][assetPath]
	if !ok {
		return nil, service.ErrCustomPageAssetNotFound
	}
	copied := *asset
	return &copied, nil
}

func (m *customPageRepoMem) ListAssets(_ context.Context, slug string) ([]service.CustomPageAsset, error) {
	out := make([]service.CustomPageAsset, 0, len(m.assets[slug]))
	for _, asset := range m.assets[slug] {
		meta := *asset
		meta.Data = nil
		out = append(out, meta)
	}
	return out, nil
}

func (m *customPageRepoMem) CountAssets(_ context.Context, slug string) (int, error) {
	return len(m.assets[slug]), nil
}

func (m *customPageRepoMem) UpsertAsset(_ context.Context, asset *service.CustomPageAsset) error {
	if _, ok := m.pages[asset.Slug]; !ok {
		return service.ErrCustomPageNotFound
	}
	if m.assets[asset.Slug] == nil {
		m.assets[asset.Slug] = map[string]*service.CustomPageAsset{}
	}
	copied := *asset
	m.assets[asset.Slug][asset.Path] = &copied
	return nil
}

func (m *customPageRepoMem) DeleteAsset(_ context.Context, slug, assetPath string) error {
	if _, ok := m.assets[slug][assetPath]; !ok {
		return service.ErrCustomPageAssetNotFound
	}
	delete(m.assets[slug], assetPath)
	return nil
}

func (m *customPageRepoMem) ImportIfAbsent(context.Context, []service.CustomPage, []service.CustomPageAsset) (int, int, error) {
	return 0, 0, nil
}

func setupCustomPageRouter(repo *customPageRepoMem) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		c.Set(string(middleware2.ContextKeyUserRole), "admin")
		c.Next()
	})
	h := NewCustomPageHandler(service.NewCustomPageService(repo))
	pages := r.Group("/api/v1/admin/pages")
	pages.GET("", h.List)
	pages.GET("/:slug", h.Get)
	pages.PUT("/:slug", h.Save)
	pages.DELETE("/:slug", h.Delete)
	pages.GET("/:slug/assets", h.ListAssets)
	pages.PUT("/:slug/assets/*path", h.SaveAsset)
	pages.DELETE("/:slug/assets/*path", h.DeleteAsset)
	return r
}

func doCustomPageJSON(r *gin.Engine, method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(w, req)
	return w
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), w.Body.String())
	return envelope
}

func TestCustomPageSaveGetListDelete(t *testing.T) {
	repo := newCustomPageRepoMem()
	r := setupCustomPageRouter(repo)

	w := doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/guide", `{"content":"# Guide"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	data := decodeEnvelope(t, w)["data"].(map[string]any)
	require.Equal(t, "guide", data["slug"])
	require.EqualValues(t, 7, data["content_size"])
	require.EqualValues(t, 42, data["updated_by"], "author comes from the auth context, not from the body")

	w = doCustomPageJSON(r, http.MethodGet, "/api/v1/admin/pages/guide", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "# Guide", decodeEnvelope(t, w)["data"].(map[string]any)["content"])

	w = doCustomPageJSON(r, http.MethodGet, "/api/v1/admin/pages", "")
	require.Equal(t, http.StatusOK, w.Code)
	list := decodeEnvelope(t, w)["data"].(map[string]any)
	require.Len(t, list["pages"], 1)
	limits := list["limits"].(map[string]any)
	require.EqualValues(t, service.MaxCustomPageContentSize, limits["max_content_size"])
	require.EqualValues(t, service.MaxCustomPageAssetSize, limits["max_asset_size"])
	require.EqualValues(t, service.MaxCustomPageAssets, limits["max_assets"])

	w = doCustomPageJSON(r, http.MethodDelete, "/api/v1/admin/pages/guide", "")
	require.Equal(t, http.StatusOK, w.Code)
	w = doCustomPageJSON(r, http.MethodDelete, "/api/v1/admin/pages/guide", "")
	require.Equal(t, http.StatusNotFound, w.Code)
	w = doCustomPageJSON(r, http.MethodGet, "/api/v1/admin/pages/guide", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestCustomPageSaveValidation(t *testing.T) {
	r := setupCustomPageRouter(newCustomPageRepoMem())

	w := doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/guide", `{}`)
	require.Equal(t, http.StatusBadRequest, w.Code, "content is required (empty string is fine, missing is not)")

	w = doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/guide", `{"content":""}`)
	require.Equal(t, http.StatusOK, w.Code)

	w = doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/bad%20slug", `{"content":"x"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)

	huge, _ := json.Marshal(map[string]string{"content": strings.Repeat("a", service.MaxCustomPageContentSize+1)})
	w = doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/guide", string(huge))
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestCustomPageAssetRawBodyUpload(t *testing.T) {
	repo := newCustomPageRepoMem()
	r := setupCustomPageRouter(repo)
	require.Equal(t, http.StatusOK, doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/guide", `{"content":"# Guide"}`).Code)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/images/logo.png", bytes.NewReader([]byte("png-bytes")))
	req.Header.Set("Content-Type", "image/png")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	data := decodeEnvelope(t, w)["data"].(map[string]any)
	require.Equal(t, "images/logo.png", data["path"])
	require.Equal(t, "image/png", data["content_type"])
	require.EqualValues(t, 9, data["size"])

	stored, err := repo.GetAsset(context.Background(), "guide", "images/logo.png")
	require.NoError(t, err)
	require.Equal(t, []byte("png-bytes"), stored.Data)

	w = doCustomPageJSON(r, http.MethodGet, "/api/v1/admin/pages/guide/assets", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, decodeEnvelope(t, w)["data"], 1)

	// 无请求体
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/empty.png", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 超限：服务层规则决定 413
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/big.png", bytes.NewReader(bytes.Repeat([]byte{1}, service.MaxCustomPageAssetSize+1)))
	req.Header.Set("Content-Type", "image/png")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	// 活动内容被拒
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/evil.html", strings.NewReader("<script>"))
	req.Header.Set("Content-Type", "text/html")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 路径穿越被拒
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/..%2Fescape.png", strings.NewReader("x"))
	req.Header.Set("Content-Type", "image/png")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 页面不存在 → 404
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/missing/assets/a.png", strings.NewReader("x"))
	req.Header.Set("Content-Type", "image/png")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)

	w = doCustomPageJSON(r, http.MethodDelete, "/api/v1/admin/pages/guide/assets/images/logo.png", "")
	require.Equal(t, http.StatusOK, w.Code)
	w = doCustomPageJSON(r, http.MethodDelete, "/api/v1/admin/pages/guide/assets/images/logo.png", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestCustomPageAssetMultipartUpload(t *testing.T) {
	repo := newCustomPageRepoMem()
	r := setupCustomPageRouter(repo)
	require.Equal(t, http.StatusOK, doCustomPageJSON(r, http.MethodPut, "/api/v1/admin/pages/guide", `{"content":"# Guide"}`).Code)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("note", "ignored non-file field"))
	part, err := mw.CreateFormFile("file", "logo.webp")
	require.NoError(t, err)
	_, err = part.Write([]byte("webp-bytes"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/logo.webp", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	data := decodeEnvelope(t, w)["data"].(map[string]any)
	require.Equal(t, "image/webp", data["content_type"], "multipart parts carry no content type; the extension decides")

	stored, err := repo.GetAsset(context.Background(), "guide", "logo.webp")
	require.NoError(t, err)
	require.Equal(t, []byte("webp-bytes"), stored.Data)

	// multipart 里没有文件字段 → 400
	var empty bytes.Buffer
	mw = multipart.NewWriter(&empty)
	require.NoError(t, mw.WriteField("note", "only text"))
	require.NoError(t, mw.Close())
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/admin/pages/guide/assets/none.png", &empty)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}
