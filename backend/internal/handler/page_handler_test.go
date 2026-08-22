package handler

import (
	"context"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// pageSettingRepoStub 只需要 custom_menu_items 一个键。
type pageSettingRepoStub struct {
	service.SettingRepository
	menuItems string
}

func (s *pageSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyCustomMenuItems {
		return s.menuItems, nil
	}
	return "", nil
}

// pageRepoStub 是最小内存仓储：页面 + 附件，足够驱动读取接口。
type pageRepoStub struct {
	service.CustomPageRepository
	pages  map[string]string
	assets map[string]*service.CustomPageAsset
}

func (s *pageRepoStub) GetPage(_ context.Context, slug string) (*service.CustomPage, error) {
	content, ok := s.pages[slug]
	if !ok {
		return nil, service.ErrCustomPageNotFound
	}
	return &service.CustomPage{Slug: slug, Content: content, ContentSize: len(content)}, nil
}

func (s *pageRepoStub) ListPages(context.Context) ([]service.CustomPageSummary, error) {
	out := make([]service.CustomPageSummary, 0, len(s.pages))
	for slug := range s.pages {
		out = append(out, service.CustomPageSummary{Slug: slug})
	}
	return out, nil
}

func (s *pageRepoStub) GetAsset(_ context.Context, slug, assetPath string) (*service.CustomPageAsset, error) {
	asset, ok := s.assets[slug+"/"+assetPath]
	if !ok {
		return nil, service.ErrCustomPageAssetNotFound
	}
	return asset, nil
}

func newPageTestRouter(t *testing.T, menuItems string, repo *pageRepoStub, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	settingSvc := service.NewSettingService(&pageSettingRepoStub{menuItems: menuItems}, &config.Config{})
	h := NewPageHandler(service.NewCustomPageService(repo), settingSvc)

	authStub := func(c *gin.Context) {
		if role != "" {
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1})
			c.Set(string(middleware2.ContextKeyUserRole), role)
		}
		c.Next()
	}
	r := gin.New()
	// 最后一个参数只喂给 AdminComplianceGuard；传 nil 让守卫直通，合规确认不是这里要测的东西。
	RegisterPageRoutes(r.Group("/api/v1"), h, authStub, authStub, nil)
	return r
}

func doPageRequest(r *gin.Engine, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestGetPageContentServesDatabaseContentWithVisibility(t *testing.T) {
	repo := &pageRepoStub{pages: map[string]string{"guide": "# Guide", "secret": "# Secret"}}
	menu := `[{"id":"1","url":"md:guide","visibility":"user"},{"id":"2","page_slug":"secret","visibility":"admin"}]`

	user := newPageTestRouter(t, menu, repo, "user")
	w := doPageRequest(user, http.MethodGet, "/api/v1/pages/guide")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "text/markdown; charset=utf-8", w.Header().Get("Content-Type"))
	require.Equal(t, "# Guide", w.Body.String())

	w = doPageRequest(user, http.MethodGet, "/api/v1/pages/secret")
	require.Equal(t, http.StatusNotFound, w.Code, "admin-only page is hidden from users")

	admin := newPageTestRouter(t, menu, repo, "admin")
	w = doPageRequest(admin, http.MethodGet, "/api/v1/pages/secret")
	require.Equal(t, http.StatusOK, w.Code)

	w = doPageRequest(admin, http.MethodGet, "/api/v1/pages/unlisted")
	require.Equal(t, http.StatusNotFound, w.Code, "slug not on the menu is invisible even if it existed")

	w = doPageRequest(admin, http.MethodGet, "/api/v1/pages/bad..slug")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetPageContentReturns404WhenRowMissing(t *testing.T) {
	repo := &pageRepoStub{pages: map[string]string{}}
	menu := `[{"id":"1","url":"md:guide","visibility":"user"}]`
	r := newPageTestRouter(t, menu, repo, "user")

	w := doPageRequest(r, http.MethodGet, "/api/v1/pages/guide")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestServePageImageServesStoredAssetWithStoredContentType(t *testing.T) {
	repo := &pageRepoStub{
		pages: map[string]string{"guide": "# Guide", "secret": "# Secret"},
		assets: map[string]*service.CustomPageAsset{
			"guide/images/logo.png":  {Slug: "guide", Path: "images/logo.png", ContentType: "image/png", Size: 3, Data: []byte("png")},
			"secret/images/logo.png": {Slug: "secret", Path: "images/logo.png", ContentType: "image/png", Size: 3, Data: []byte("png")},
		},
	}
	menu := `[{"id":"1","url":"md:guide","visibility":"user"},{"id":"2","page_slug":"secret","visibility":"admin"}]`
	r := newPageTestRouter(t, menu, repo, "")

	w := doPageRequest(r, http.MethodGet, "/api/v1/pages/guide/images/images/logo.png")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "image/png", w.Header().Get("Content-Type"))
	require.Equal(t, "png", w.Body.String())

	w = doPageRequest(r, http.MethodGet, "/api/v1/pages/guide/images/images%2Flogo.png")
	require.Equal(t, http.StatusOK, w.Code, "encoded separators resolve to the same stored path")

	w = doPageRequest(r, http.MethodGet, "/api/v1/pages/guide/images/missing.png")
	require.Equal(t, http.StatusNotFound, w.Code)

	w = doPageRequest(r, http.MethodGet, "/api/v1/pages/guide/images/..%2Fimages%2Flogo.png")
	require.Equal(t, http.StatusNotFound, w.Code, "traversal is rejected before the repository is consulted")

	w = doPageRequest(r, http.MethodGet, "/api/v1/pages/secret/images/images/logo.png")
	require.Equal(t, http.StatusNotFound, w.Code, "assets of admin-only pages are never served without a JWT")
}

func TestListPagesReturnsSlugsFromDatabase(t *testing.T) {
	repo := &pageRepoStub{pages: map[string]string{"b": "", "a": ""}}
	r := newPageTestRouter(t, "[]", repo, "admin")

	w := doPageRequest(r, http.MethodGet, "/api/v1/pages")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"a"`)
	require.Contains(t, w.Body.String(), `"b"`)
}

// 回归护栏：页面读取接口不得再触碰本地文件系统（issue #7 的根因）。
// 走源码 import 声明而不是点名函数：任何新加的磁盘读取都会在这里被拦下。
func TestPageHandlersDoNotImportFilesystemPackages(t *testing.T) {
	forbidden := map[string]struct{}{"os": {}, "io/fs": {}, "path/filepath": {}}
	files := []string{
		"page_handler.go",
		filepath.Join("admin", "custom_page_handler.go"),
	}
	for _, file := range files {
		src, err := os.ReadFile(file)
		require.NoError(t, err, file)
		parsed, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly)
		require.NoError(t, err, file)
		for _, imp := range parsed.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			_, bad := forbidden[path]
			require.Falsef(t, bad, "%s imports %q: custom pages are served from the database, never from the local disk", file, path)
		}
	}
}
