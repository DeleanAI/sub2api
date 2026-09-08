//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func cleanupCustomPages(t *testing.T, slugs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, slug := range slugs {
			_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM custom_pages WHERE slug = $1", slug)
		}
	})
}

func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

func TestCustomPageRepositoryPageRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := NewCustomPageRepository(integrationDB)
	slug := uniqueSlug("it-page")
	cleanupCustomPages(t, slug)

	_, err := repo.GetPage(ctx, slug)
	require.ErrorIs(t, err, service.ErrCustomPageNotFound)

	actor := int64(7)
	page := &service.CustomPage{Slug: slug, Content: "# v1", ContentSize: 4, UpdatedBy: &actor}
	require.NoError(t, repo.UpsertPage(ctx, page))
	require.False(t, page.CreatedAt.IsZero())
	firstCreated := page.CreatedAt

	got, err := repo.GetPage(ctx, slug)
	require.NoError(t, err)
	require.Equal(t, "# v1", got.Content)
	require.Equal(t, 4, got.ContentSize)
	require.NotNil(t, got.UpdatedBy)
	require.Equal(t, actor, *got.UpdatedBy)

	time.Sleep(5 * time.Millisecond)
	page2 := &service.CustomPage{Slug: slug, Content: "# v2 longer", ContentSize: 11}
	require.NoError(t, repo.UpsertPage(ctx, page2))
	require.Equal(t, firstCreated.UTC().Truncate(time.Millisecond), page2.CreatedAt.UTC().Truncate(time.Millisecond), "created_at survives overwrites")
	require.True(t, page2.UpdatedAt.After(firstCreated) || page2.UpdatedAt.Equal(firstCreated))

	got, err = repo.GetPage(ctx, slug)
	require.NoError(t, err)
	require.Equal(t, "# v2 longer", got.Content)
	require.Nil(t, got.UpdatedBy, "nil author overwrites the previous one")

	count, err := repo.CountPages(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, count, 1)

	require.NoError(t, repo.DeletePage(ctx, slug))
	require.ErrorIs(t, repo.DeletePage(ctx, slug), service.ErrCustomPageNotFound)
}

func TestCustomPageRepositoryAssetsCascadeAndSummaries(t *testing.T) {
	ctx := context.Background()
	repo := NewCustomPageRepository(integrationDB)
	slug := uniqueSlug("it-assets")
	cleanupCustomPages(t, slug)

	orphan := &service.CustomPageAsset{Slug: slug, Path: "images/a.png", ContentType: "image/png", Size: 1, Data: []byte("a")}
	require.ErrorIs(t, repo.UpsertAsset(ctx, orphan), service.ErrCustomPageNotFound, "FK violation surfaces as page-not-found")

	require.NoError(t, repo.UpsertPage(ctx, &service.CustomPage{Slug: slug, Content: "# A", ContentSize: 3}))
	require.NoError(t, repo.UpsertAsset(ctx, orphan))
	require.NoError(t, repo.UpsertAsset(ctx, &service.CustomPageAsset{Slug: slug, Path: "images/b.png", ContentType: "image/png", Size: 2, Data: []byte("bb")}))

	// 覆盖同一路径：内容与类型更新，计数不变
	require.NoError(t, repo.UpsertAsset(ctx, &service.CustomPageAsset{Slug: slug, Path: "images/a.png", ContentType: "image/webp", Size: 3, Data: []byte("aaa")}))
	count, err := repo.CountAssets(ctx, slug)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	got, err := repo.GetAsset(ctx, slug, "images/a.png")
	require.NoError(t, err)
	require.Equal(t, "image/webp", got.ContentType)
	require.Equal(t, []byte("aaa"), got.Data)
	require.Equal(t, 3, got.Size)

	list, err := repo.ListAssets(ctx, slug)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "images/a.png", list[0].Path)
	require.Nil(t, list[0].Data, "listing never loads blobs")

	summaries, err := repo.ListPages(ctx)
	require.NoError(t, err)
	var found *service.CustomPageSummary
	for i := range summaries {
		if summaries[i].Slug == slug {
			found = &summaries[i]
		}
	}
	require.NotNil(t, found)
	require.Equal(t, 2, found.AssetCount)
	require.EqualValues(t, 5, found.AssetBytes)
	require.Equal(t, 3, found.ContentSize)

	require.NoError(t, repo.DeleteAsset(ctx, slug, "images/b.png"))
	require.ErrorIs(t, repo.DeleteAsset(ctx, slug, "images/b.png"), service.ErrCustomPageAssetNotFound)

	require.NoError(t, repo.DeletePage(ctx, slug))
	_, err = repo.GetAsset(ctx, slug, "images/a.png")
	require.ErrorIs(t, err, service.ErrCustomPageAssetNotFound, "ON DELETE CASCADE removes the assets")
}

func TestCustomPageRepositoryImportIfAbsentIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewCustomPageRepository(integrationDB)
	slugA, slugB := uniqueSlug("it-import-a"), uniqueSlug("it-import-b")
	cleanupCustomPages(t, slugA, slugB)

	pages := []service.CustomPage{
		{Slug: slugA, Content: "# A", ContentSize: 3},
		{Slug: slugB, Content: "# B", ContentSize: 3},
	}
	assets := []service.CustomPageAsset{
		{Slug: slugA, Path: "logo.png", ContentType: "image/png", Size: 1, Data: []byte("a")},
		{Slug: slugB, Path: "logo.png", ContentType: "image/png", Size: 1, Data: []byte("b")},
	}

	insertedPages, insertedAssets, err := repo.ImportIfAbsent(ctx, pages, assets)
	require.NoError(t, err)
	require.Equal(t, 2, insertedPages)
	require.Equal(t, 2, insertedAssets)

	// 第二次（模拟另一副本同时导入）：全部已存在，什么都不写，也不报错。
	pages[0].Content = "# A changed on disk"
	insertedPages, insertedAssets, err = repo.ImportIfAbsent(ctx, pages, assets)
	require.NoError(t, err)
	require.Zero(t, insertedPages)
	require.Zero(t, insertedAssets)

	got, err := repo.GetPage(ctx, slugA)
	require.NoError(t, err)
	require.Equal(t, "# A", got.Content, "ON CONFLICT DO NOTHING keeps the first import")

	// 事务性：引用不存在页面的附件让整批回滚。
	slugC := uniqueSlug("it-import-c")
	cleanupCustomPages(t, slugC)
	_, _, err = repo.ImportIfAbsent(ctx,
		[]service.CustomPage{{Slug: slugC, Content: "# C", ContentSize: 3}},
		[]service.CustomPageAsset{{Slug: slugC + "-missing", Path: "x.png", ContentType: "image/png", Size: 1, Data: []byte("x")}},
	)
	require.Error(t, err)
	_, err = repo.GetPage(ctx, slugC)
	require.ErrorIs(t, err, service.ErrCustomPageNotFound, "the page insert of the failed batch was rolled back")
}

// 端到端：服务层导入器 + 真实仓储 + 真实磁盘布局。
func TestCustomPageImportFromDiskEndToEnd(t *testing.T) {
	ctx := context.Background()
	repo := NewCustomPageRepository(integrationDB)
	slug := uniqueSlug("it-e2e")
	cleanupCustomPages(t, slug)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, slug+".md"), []byte("# E2E\n![logo](images/logo.png)"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, slug, "images"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, slug, "images", "logo.png"), []byte("png"), 0o644))

	// 表里已有其它测试留下的页面时导入器会当作"非空"跳过；先把本测试的环境清空。
	_, err := integrationDB.ExecContext(ctx, "DELETE FROM custom_pages")
	require.NoError(t, err)

	svc := service.NewCustomPageService(repo)
	svc.SetLeaderLock(NewLeaderLockCache(testRedis(t)), integrationDB)

	report, err := svc.ImportFromDisk(ctx, dir)
	require.NoError(t, err)
	require.Equal(t, 1, report.InsertedPages)
	require.Equal(t, 1, report.InsertedAssets)

	page, err := svc.GetPage(ctx, slug)
	require.NoError(t, err)
	require.Equal(t, "# E2E\n![logo](images/logo.png)", page.Content)
	asset, err := svc.GetAsset(ctx, slug, "images/logo.png")
	require.NoError(t, err)
	require.Equal(t, "image/png", asset.ContentType)
	require.Equal(t, []byte("png"), asset.Data)

	// 再启动一次：持久标记已落，连目录都不扫（扫描会把所有附件字节读进内存，
	// 且发生在监听端口之前）。
	report, err = svc.ImportFromDisk(ctx, dir)
	require.NoError(t, err)
	require.True(t, report.AlreadyImported)
	require.Zero(t, report.InsertedPages)
	require.Empty(t, report.IgnoredFiles, "标记已存在时不扫描目录")

	// 删光页面之后再启动：撤下的内容不得从磁盘复活。
	require.NoError(t, svc.DeletePage(ctx, slug))
	count, err := repo.CountPages(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
	report, err = svc.ImportFromDisk(ctx, dir)
	require.NoError(t, err)
	require.True(t, report.AlreadyImported)
	require.Zero(t, report.InsertedPages)
	_, err = svc.GetPage(ctx, slug)
	require.ErrorIs(t, err, service.ErrCustomPageNotFound, "删掉的页面不能被磁盘导入复活")
}
