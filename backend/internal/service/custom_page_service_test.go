package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// customPageRepoFake 是内存版仓储，足够覆盖服务层的校验、限额与导入编排。
type customPageRepoFake struct {
	pages  map[string]*CustomPage
	assets map[string]map[string]*CustomPageAsset
	// importCalls 记录 ImportIfAbsent 的调用，用于断言导入只发生一次、内容正确。
	importCalls int
}

func newCustomPageRepoFake() *customPageRepoFake {
	return &customPageRepoFake{pages: map[string]*CustomPage{}, assets: map[string]map[string]*CustomPageAsset{}}
}

func (f *customPageRepoFake) GetPage(_ context.Context, slug string) (*CustomPage, error) {
	page, ok := f.pages[slug]
	if !ok {
		return nil, ErrCustomPageNotFound
	}
	copied := *page
	return &copied, nil
}

func (f *customPageRepoFake) ListPages(context.Context) ([]CustomPageSummary, error) {
	out := make([]CustomPageSummary, 0, len(f.pages))
	for slug, page := range f.pages {
		out = append(out, CustomPageSummary{Slug: slug, ContentSize: page.ContentSize, AssetCount: len(f.assets[slug])})
	}
	return out, nil
}

func (f *customPageRepoFake) CountPages(context.Context) (int, error) { return len(f.pages), nil }

func (f *customPageRepoFake) UpsertPage(_ context.Context, page *CustomPage) error {
	copied := *page
	f.pages[page.Slug] = &copied
	return nil
}

func (f *customPageRepoFake) DeletePage(_ context.Context, slug string) error {
	if _, ok := f.pages[slug]; !ok {
		return ErrCustomPageNotFound
	}
	delete(f.pages, slug)
	delete(f.assets, slug)
	return nil
}

func (f *customPageRepoFake) GetAsset(_ context.Context, slug, assetPath string) (*CustomPageAsset, error) {
	asset, ok := f.assets[slug][assetPath]
	if !ok {
		return nil, ErrCustomPageAssetNotFound
	}
	copied := *asset
	return &copied, nil
}

func (f *customPageRepoFake) ListAssets(_ context.Context, slug string) ([]CustomPageAsset, error) {
	out := make([]CustomPageAsset, 0, len(f.assets[slug]))
	for _, asset := range f.assets[slug] {
		meta := *asset
		meta.Data = nil
		out = append(out, meta)
	}
	return out, nil
}

func (f *customPageRepoFake) CountAssets(_ context.Context, slug string) (int, error) {
	return len(f.assets[slug]), nil
}

func (f *customPageRepoFake) UpsertAsset(_ context.Context, asset *CustomPageAsset) error {
	if _, ok := f.pages[asset.Slug]; !ok {
		return ErrCustomPageNotFound
	}
	if f.assets[asset.Slug] == nil {
		f.assets[asset.Slug] = map[string]*CustomPageAsset{}
	}
	copied := *asset
	f.assets[asset.Slug][asset.Path] = &copied
	return nil
}

func (f *customPageRepoFake) DeleteAsset(_ context.Context, slug, assetPath string) error {
	if _, ok := f.assets[slug][assetPath]; !ok {
		return ErrCustomPageAssetNotFound
	}
	delete(f.assets[slug], assetPath)
	return nil
}

func (f *customPageRepoFake) ImportIfAbsent(_ context.Context, pages []CustomPage, assets []CustomPageAsset) (int, int, error) {
	f.importCalls++
	insertedPages, insertedAssets := 0, 0
	for i := range pages {
		if _, exists := f.pages[pages[i].Slug]; exists {
			continue
		}
		copied := pages[i]
		f.pages[copied.Slug] = &copied
		insertedPages++
	}
	for i := range assets {
		asset := assets[i]
		if f.assets[asset.Slug] == nil {
			f.assets[asset.Slug] = map[string]*CustomPageAsset{}
		}
		if _, exists := f.assets[asset.Slug][asset.Path]; exists {
			continue
		}
		f.assets[asset.Slug][asset.Path] = &asset
		insertedAssets++
	}
	return insertedPages, insertedAssets, nil
}

func TestValidateCustomPageSlug(t *testing.T) {
	valid := []string{"a", "guide", "Guide-1", "a_b-c", "0start", strings.Repeat("x", MaxCustomPageSlugLength)}
	for _, slug := range valid {
		require.NoError(t, ValidateCustomPageSlug(slug), slug)
	}
	invalid := []string{"", "-lead", "_lead", "a b", "a/b", "a.md", "../x", "中文", strings.Repeat("x", MaxCustomPageSlugLength+1)}
	for _, slug := range invalid {
		require.ErrorIs(t, ValidateCustomPageSlug(slug), ErrCustomPageInvalidSlug, slug)
	}
}

func TestNormalizeCustomPageAssetPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "single filename", in: "logo.png", want: "logo.png", ok: true},
		{name: "nested path", in: "images/logo.png", want: "images/logo.png", ok: true},
		{name: "dot prefix", in: "./logo.png", want: "logo.png", ok: true},
		{name: "double slash collapses", in: "images//logo.png", want: "images/logo.png", ok: true},
		{name: "url escaped slash", in: "images%2Flogo.png", want: "images/logo.png", ok: true},
		{name: "unicode filename", in: "图片/标志.png", want: "图片/标志.png", ok: true},
		{name: "parent traversal", in: "../secret.png", ok: false},
		{name: "nested parent traversal", in: "images/../../secret.png", ok: false},
		{name: "encoded parent traversal", in: "%2e%2e/secret.png", ok: false},
		{name: "backslash traversal", in: `images\secret.png`, ok: false},
		{name: "absolute path", in: "/etc/passwd", ok: false},
		{name: "encoded absolute path", in: "%2fetc/passwd", ok: false},
		{name: "encoded nul byte", in: "logo.png%00", ok: false},
		{name: "control character", in: "logo\n.png", ok: false},
		{name: "invalid escape", in: "logo.png%zz", ok: false},
		{name: "empty path", in: "", ok: false},
		{name: "only dots", in: "./.", ok: false},
		{name: "too long", in: strings.Repeat("a", MaxCustomPageAssetPathLength+1), ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeCustomPageAssetPath(tt.in)
			if !tt.ok {
				require.ErrorIs(t, err, ErrCustomPageInvalidAssetPath)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSavePageEnforcesContentRules(t *testing.T) {
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()
	actor := int64(7)

	page, err := svc.SavePage(ctx, "guide", "# Hello", &actor)
	require.NoError(t, err)
	require.Equal(t, 7, page.ContentSize)
	require.Equal(t, &actor, page.UpdatedBy)

	_, err = svc.SavePage(ctx, "bad slug", "x", nil)
	require.ErrorIs(t, err, ErrCustomPageInvalidSlug)

	_, err = svc.SavePage(ctx, "guide", strings.Repeat("a", MaxCustomPageContentSize+1), nil)
	require.ErrorIs(t, err, ErrCustomPageContentTooLarge)

	_, err = svc.SavePage(ctx, "guide", strings.Repeat("a", MaxCustomPageContentSize), nil)
	require.NoError(t, err, "exactly the limit is allowed")

	_, err = svc.SavePage(ctx, "guide", "bad\x00nul", nil)
	require.ErrorIs(t, err, ErrCustomPageContentInvalid)

	_, err = svc.SavePage(ctx, "guide", string([]byte{0xff, 0xfe}), nil)
	require.ErrorIs(t, err, ErrCustomPageContentInvalid)

	// 空正文是合法的：管理员可以先建页再填内容。
	_, err = svc.SavePage(ctx, "empty", "", nil)
	require.NoError(t, err)
}

func TestSaveAssetEnforcesSizeTypeAndCount(t *testing.T) {
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()
	_, err := svc.SavePage(ctx, "guide", "# Guide", nil)
	require.NoError(t, err)

	asset, err := svc.SaveAsset(ctx, "guide", "images/logo.png", "", []byte("png"))
	require.NoError(t, err)
	require.Equal(t, "image/png", asset.ContentType, "content type inferred from extension when not declared")
	require.Equal(t, "images/logo.png", asset.Path)

	asset, err = svc.SaveAsset(ctx, "guide", "raw.bin", "image/webp; charset=binary", []byte("x"))
	require.NoError(t, err)
	require.Equal(t, "image/webp", asset.ContentType, "declared media type wins, parameters dropped")

	asset, err = svc.SaveAsset(ctx, "guide", "unknown.zzz", "", []byte("x"))
	require.NoError(t, err)
	require.Equal(t, "application/octet-stream", asset.ContentType)

	_, err = svc.SaveAsset(ctx, "guide", "empty.png", "", nil)
	require.ErrorIs(t, err, ErrCustomPageAssetEmpty)

	_, err = svc.SaveAsset(ctx, "guide", "big.png", "", bytes.Repeat([]byte{1}, MaxCustomPageAssetSize+1))
	require.ErrorIs(t, err, ErrCustomPageAssetTooLarge)

	_, err = svc.SaveAsset(ctx, "guide", "../escape.png", "", []byte("x"))
	require.ErrorIs(t, err, ErrCustomPageInvalidAssetPath)

	_, err = svc.SaveAsset(ctx, "missing", "a.png", "", []byte("x"))
	require.ErrorIs(t, err, ErrCustomPageNotFound)
}

// 遍历声明的拒绝表而不是硬编码某个类型：新增条目当天就被覆盖。
func TestSaveAssetRejectsEveryBlockedMediaType(t *testing.T) {
	require.NotEmpty(t, customPageAssetBlockedMediaTypes)
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()
	_, err := svc.SavePage(ctx, "guide", "# Guide", nil)
	require.NoError(t, err)

	for mediaType := range customPageAssetBlockedMediaTypes {
		t.Run(mediaType, func(t *testing.T) {
			_, err := svc.SaveAsset(ctx, "guide", "payload.bin", mediaType, []byte("<script>"))
			require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected)
			_, err = svc.SaveAsset(ctx, "guide", "payload.bin", strings.ToUpper(mediaType)+"; charset=utf-8", []byte("<script>"))
			require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected, "case and parameters must not bypass the list")
		})
	}

	// 扩展名推断出的活动类型同样被拒：.html 文件不能借"未声明类型"混进来。
	_, err = svc.SaveAsset(ctx, "guide", "page.html", "", []byte("<html>"))
	require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected)
}

func TestSaveAssetCountLimitAllowsReplacingExisting(t *testing.T) {
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()
	_, err := svc.SavePage(ctx, "guide", "# Guide", nil)
	require.NoError(t, err)

	for i := 0; i < MaxCustomPageAssets; i++ {
		_, err := svc.SaveAsset(ctx, "guide", fmt.Sprintf("img%03d.png", i), "", []byte("x"))
		require.NoError(t, err)
	}

	_, err = svc.SaveAsset(ctx, "guide", "one-more.png", "", []byte("x"))
	require.ErrorIs(t, err, ErrCustomPageTooManyAssets)

	_, err = svc.SaveAsset(ctx, "guide", "img000.png", "", []byte("replaced"))
	require.NoError(t, err, "overwriting an existing path does not add an asset")

	require.NoError(t, svc.DeleteAsset(ctx, "guide", "img001.png"))
	_, err = svc.SaveAsset(ctx, "guide", "one-more.png", "", []byte("x"))
	require.NoError(t, err, "room is available again after a delete")
}

func TestGetAndDeleteRoundTrip(t *testing.T) {
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()

	_, err := svc.GetPage(ctx, "guide")
	require.ErrorIs(t, err, ErrCustomPageNotFound)
	_, err = svc.GetPage(ctx, "bad/slug")
	require.ErrorIs(t, err, ErrCustomPageInvalidSlug)

	_, err = svc.SavePage(ctx, "guide", "# Guide", nil)
	require.NoError(t, err)
	_, err = svc.SaveAsset(ctx, "guide", "images/a.png", "image/png", []byte("a"))
	require.NoError(t, err)

	asset, err := svc.GetAsset(ctx, "guide", "images%2Fa.png")
	require.NoError(t, err)
	require.Equal(t, []byte("a"), asset.Data)

	_, err = svc.GetAsset(ctx, "guide", "../a.png")
	require.ErrorIs(t, err, ErrCustomPageInvalidAssetPath)

	assets, err := svc.ListAssets(ctx, "guide")
	require.NoError(t, err)
	require.Len(t, assets, 1)
	require.Nil(t, assets[0].Data, "listing never carries blobs")

	require.NoError(t, svc.DeletePage(ctx, "guide"))
	require.ErrorIs(t, svc.DeletePage(ctx, "guide"), ErrCustomPageNotFound)
	_, err = svc.GetAsset(ctx, "guide", "images/a.png")
	require.ErrorIs(t, err, ErrCustomPageAssetNotFound, "assets go away with the page")
}

func writeImportFixture(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, data, 0o644))
	}
}

func TestImportFromDiskImportsPagesAndAssetsOnce(t *testing.T) {
	dir := t.TempDir()
	writeImportFixture(t, dir, map[string][]byte{
		"guide.md":              []byte("# Guide\n![logo](images/logo.png)"),
		"guide/images/logo.png": []byte("png-bytes"),
		"guide/notes.txt":       []byte("plain"),
		"faq.md":                []byte("# FAQ"),
	})
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)

	report, err := svc.ImportFromDisk(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, 2, report.InsertedPages)
	require.Equal(t, 2, report.InsertedAssets)
	require.Empty(t, report.SkippedFiles)
	require.Empty(t, report.IgnoredFiles)
	require.False(t, report.LockHeldByPeer)
	require.Equal(t, 1, repo.importCalls)

	page, err := repo.GetPage(context.Background(), "guide")
	require.NoError(t, err)
	require.Equal(t, "# Guide\n![logo](images/logo.png)", page.Content)
	require.Nil(t, page.UpdatedBy, "imported pages have no admin author")

	asset, err := repo.GetAsset(context.Background(), "guide", "images/logo.png")
	require.NoError(t, err)
	require.Equal(t, "image/png", asset.ContentType)
	require.Equal(t, []byte("png-bytes"), asset.Data)

	// 第二次启动：表已非空，磁盘文件被忽略且列在报告里，不再写库。
	report, err = svc.ImportFromDisk(context.Background(), dir)
	require.NoError(t, err)
	require.Zero(t, report.InsertedPages)
	require.ElementsMatch(t, []string{"faq.md", "guide.md", "guide/images/logo.png", "guide/notes.txt"}, report.IgnoredFiles)
	require.Equal(t, 1, repo.importCalls, "no second import")
}

func TestImportFromDiskSkipsInvalidFilesButImportsTheRest(t *testing.T) {
	dir := t.TempDir()
	writeImportFixture(t, dir, map[string][]byte{
		"ok.md":               []byte("fine"),
		"bad slug.md":         []byte("invalid slug"),
		"README.txt":          []byte("not a page"),
		"ok/page.html":        []byte("<html>"),
		"ok/images/logo.png":  []byte("png"),
		"orphan/images/x.png": []byte("asset dir without page"),
		"toolarge.md":         bytes.Repeat([]byte("a"), MaxCustomPageContentSize+1),
		"ok/images/huge.bin":  bytes.Repeat([]byte("b"), MaxCustomPageAssetSize+1),
		"binary.md":           {0xff, 0xfe, 0x00},
	})
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)

	report, err := svc.ImportFromDisk(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, 1, report.InsertedPages)
	require.Equal(t, 1, report.InsertedAssets, "only images/logo.png passes the asset rules")

	joined := strings.Join(report.SkippedFiles, "\n")
	for _, want := range []string{"bad slug.md", "README.txt", "ok/page.html", "orphan/", "toolarge.md", "ok/images/huge.bin", "binary.md"} {
		require.Contains(t, joined, want)
	}
	_, err = repo.GetPage(context.Background(), "ok")
	require.NoError(t, err)
	_, err = repo.GetAsset(context.Background(), "ok", "page.html")
	require.ErrorIs(t, err, ErrCustomPageAssetNotFound, "active content never gets imported")
}

func TestImportFromDiskHandlesMissingAndEmptyDir(t *testing.T) {
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)

	report, err := svc.ImportFromDisk(context.Background(), filepath.Join(t.TempDir(), "nope"))
	require.NoError(t, err)
	require.True(t, report.DirMissing)
	require.Zero(t, repo.importCalls)

	report, err = svc.ImportFromDisk(context.Background(), t.TempDir())
	require.NoError(t, err)
	require.False(t, report.DirMissing)
	require.Zero(t, repo.importCalls)
}

func TestImportFromDiskGivesUpWhenPeerHoldsLock(t *testing.T) {
	dir := t.TempDir()
	writeImportFixture(t, dir, map[string][]byte{"guide.md": []byte("# Guide")})
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	svc.SetLeaderLock(&heldLeaderLockCache{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 不等满 10s：ctx 取消让重试循环立即退出
	report, err := svc.ImportFromDisk(ctx, dir)
	require.NoError(t, err)
	require.True(t, report.LockHeldByPeer)
	require.Zero(t, repo.importCalls)
}

// heldLeaderLockCache 模拟锁一直被其它副本持有。
type heldLeaderLockCache struct{}

func (heldLeaderLockCache) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

func (heldLeaderLockCache) ReleaseLeaderLock(context.Context, string, string) error { return nil }

var _ LeaderLockCache = heldLeaderLockCache{}

func TestImportFromDiskSurfacesRepositoryErrors(t *testing.T) {
	dir := t.TempDir()
	writeImportFixture(t, dir, map[string][]byte{"guide.md": []byte("# Guide")})
	svc := NewCustomPageService(&failingCountRepo{customPageRepoFake: newCustomPageRepoFake()})

	_, err := svc.ImportFromDisk(context.Background(), dir)
	require.Error(t, err)
	require.True(t, errors.Is(err, errBoom))
}

var errBoom = errors.New("boom")

type failingCountRepo struct {
	*customPageRepoFake
}

func (failingCountRepo) CountPages(context.Context) (int, error) { return 0, errBoom }
