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
	// legacyImportMarker 模拟 settings 里的"已导入"标记（空串表示未落标记）。
	legacyImportMarker string
}

func (f *customPageRepoFake) LegacyImportDone(context.Context, string) (bool, error) {
	return f.legacyImportMarker != "", nil
}

func (f *customPageRepoFake) MarkLegacyImportDone(_ context.Context, dir, note string) error {
	if f.legacyImportMarker == "" {
		f.legacyImportMarker = dir + "|" + note
	}
	return nil
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
// TestCustomPageAssetActiveContentTypesAreRejected 遍历一张"已知会执行脚本"的类型表，
// 而不是遍历放行/拦截规则本身。
//
// 遍历规则表的测试对本类缺陷是结构性失明的：之前的黑名单里没有 image/svg+xml，
// 而测试正是遍历那张黑名单逐条断言被拦，于是它永远不可能发现漏了谁。这张表独立
// 于实现，列的是攻击面而不是当前实现认得的东西。
func TestCustomPageAssetActiveContentTypesAreRejected(t *testing.T) {
	activeContentTypes := []string{
		"text/html",
		"application/xhtml+xml",
		"text/javascript",
		"application/javascript",
		"application/x-javascript",
		"application/ecmascript",
		"text/ecmascript",
		// SVG 可以内嵌 <script>，附件是同源内联提供的公开路由。
		"image/svg+xml",
		"text/xml",
		"application/xml",
		"application/xslt+xml",
		"text/vtt",
		"application/x-shockwave-flash",
		"application/wasm",
		"text/css",
	}
	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()
	_, err := svc.SavePage(ctx, "guide", "# Guide", nil)
	require.NoError(t, err)

	for _, mediaType := range activeContentTypes {
		t.Run(mediaType, func(t *testing.T) {
			_, err := svc.SaveAsset(ctx, "guide", "payload.bin", mediaType, []byte("<script>"))
			require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected)
			_, err = svc.SaveAsset(ctx, "guide", "payload.bin", strings.ToUpper(mediaType)+"; charset=utf-8", []byte("<script>"))
			require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected, "case and parameters must not bypass the rule")
			// 扩展名也不是控制点：声明类型即便被拒，也不能靠改扩展名换一条路进来。
			_, err = svc.SaveAsset(ctx, "guide", "payload.svg", mediaType, []byte("<script>"))
			require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected)
		})
	}

	// .svg 扩展名在不声明类型时也会被 mime.TypeByExtension 推成 image/svg+xml。
	_, err = svc.SaveAsset(ctx, "guide", "evil.svg", "", []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"><script/></svg>"))
	require.ErrorIs(t, err, ErrCustomPageAssetContentTypeRejected, "扩展名推断出的活动类型同样必须被拒")

	// 正常图片仍然放行，白名单不能把功能一起关掉。
	asset, err := svc.SaveAsset(ctx, "guide", "images/logo.png", "image/png", []byte("png-bytes"))
	require.NoError(t, err)
	require.Equal(t, "image/png", asset.ContentType)
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

	// 第二次启动：持久标记已落，连目录都不再扫描（IgnoredFiles 因此为空），更不写库。
	// 扫描发生在监听端口之前且会把所有附件字节读进内存，"已经导入过"必须在碰磁盘之前判掉。
	report, err = svc.ImportFromDisk(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, report.AlreadyImported)
	require.Zero(t, report.InsertedPages)
	require.Empty(t, report.IgnoredFiles, "标记已存在时不扫描目录")
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

// TestImportFromDisk_NeverResurrectsDeletedPages 钉死"导入是一次性的"。
//
// 判据曾经是"表里有没有行"，而这个判据每次启动都重算：管理员依法删掉最后一个页面
// （法务要求撤下旧版 ToS 是真实场景），下一次重启就把磁盘上那份撤下的文本重新导回来
// 并对外提供，updated_by 还是 NULL。Compose 默认把 /app/data 挂在持久卷上，
// 磁盘文件一直都在，所以这不是理论问题。
func TestImportFromDisk_NeverResurrectsDeletedPages(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tos.md"), []byte("# OLD TERMS FROM DISK"), 0o600))

	repo := newCustomPageRepoFake()
	svc := NewCustomPageService(repo)
	ctx := context.Background()

	report, err := svc.ImportFromDisk(ctx, dir)
	require.NoError(t, err)
	require.Equal(t, 1, report.InsertedPages)
	require.NotEmpty(t, repo.legacyImportMarker, "导入成功后必须落下持久标记")

	// 管理员撤下这个页面，表变空。
	require.NoError(t, svc.DeletePage(ctx, "tos"))
	count, err := repo.CountPages(ctx)
	require.NoError(t, err)
	require.Zero(t, count)

	// 重启：磁盘文件还在，但撤下的内容不得复活。
	report, err = svc.ImportFromDisk(ctx, dir)
	require.NoError(t, err)
	require.True(t, report.AlreadyImported)
	require.Zero(t, report.InsertedPages)
	require.Equal(t, 1, repo.importCalls, "第二次启动不得再次写入")

	page, err := svc.GetPage(ctx, "tos")
	require.ErrorIs(t, err, ErrCustomPageNotFound)
	require.Nil(t, page)
}

// TestImportFromDisk_DoesNotTouchDiskOnceImported 钉死"先看标记，再碰磁盘"的顺序。
//
// 扫描会把整个目录（含每个附件的全部字节）读进内存，而这发生在监听端口之前：
// 按上限 200 附件 × 5 MiB 算是 ~1 GiB 常驻，512Mi 的 Pod 会在数据库明明已有内容的
// 情况下反复 OOM。用一个不可读的目录代表"碰了磁盘就会出事"。
func TestImportFromDisk_DoesNotTouchDiskOnceImported(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tos.md"), []byte("# terms"), 0o600))

	repo := newCustomPageRepoFake()
	repo.legacyImportMarker = "已导入"
	svc := NewCustomPageService(repo)

	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	report, err := svc.ImportFromDisk(context.Background(), dir)
	require.NoError(t, err, "标记已存在时不应该去读目录，因而也读不出错")
	require.True(t, report.AlreadyImported)
	require.Zero(t, repo.importCalls)
}

// TestImportFromDisk_MarksDoneWhenTableAlreadyHasRows 覆盖"这一版之前就有页面"的升级路径：
// 表非空说明导入这件事已经了结，同样要落标记，否则删空页面后磁盘内容仍会复活。
func TestImportFromDisk_MarksDoneWhenTableAlreadyHasRows(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tos.md"), []byte("# terms from disk"), 0o600))

	repo := newCustomPageRepoFake()
	repo.pages["about"] = &CustomPage{Slug: "about", Content: "# about"}
	svc := NewCustomPageService(repo)

	report, err := svc.ImportFromDisk(context.Background(), dir)
	require.NoError(t, err)
	require.Zero(t, report.InsertedPages)
	require.NotEmpty(t, report.IgnoredFiles)
	require.NotEmpty(t, repo.legacyImportMarker, "表非空同样要落标记")
}
