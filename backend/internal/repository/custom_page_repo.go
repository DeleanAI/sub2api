package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// customPageRepository 自定义页面仓储（raw SQL）。
// 页面与附件都在 PostgreSQL 里，任何副本写入后所有副本立即可见（issue #7）。
type customPageRepository struct {
	db *sql.DB
}

// NewCustomPageRepository 创建自定义页面仓储。
func NewCustomPageRepository(db *sql.DB) service.CustomPageRepository {
	return &customPageRepository{db: db}
}

const (
	pgForeignKeyViolation = "23503"

	customPageSelectColumns  = `slug, content, content_size, updated_by, created_at, updated_at`
	customPageAssetMetaCols  = `slug, path, content_type, size, created_at, updated_at`
	customPageAssetFullCols  = customPageAssetMetaCols + `, data`
	customPageInsertIgnore   = `INSERT INTO custom_pages (slug, content, content_size, updated_by, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $5) ON CONFLICT (slug) DO NOTHING`
	customPageAssetInsIgnore = `INSERT INTO custom_page_assets (slug, path, content_type, size, data, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6) ON CONFLICT (slug, path) DO NOTHING`
)

func (r *customPageRepository) GetPage(ctx context.Context, slug string) (*service.CustomPage, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil custom page repository")
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+customPageSelectColumns+` FROM custom_pages WHERE slug = $1`, slug)
	page, err := scanCustomPage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrCustomPageNotFound
		}
		return nil, err
	}
	return page, nil
}

func (r *customPageRepository) ListPages(ctx context.Context) ([]service.CustomPageSummary, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil custom page repository")
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT p.slug, p.content_size, p.updated_by, p.created_at, p.updated_at,
       COALESCE(a.asset_count, 0), COALESCE(a.asset_bytes, 0)
FROM custom_pages p
LEFT JOIN (
    SELECT slug, COUNT(*) AS asset_count, COALESCE(SUM(size), 0) AS asset_bytes
    FROM custom_page_assets GROUP BY slug
) a ON a.slug = p.slug
ORDER BY p.slug`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.CustomPageSummary, 0)
	for rows.Next() {
		var item service.CustomPageSummary
		var updatedBy sql.NullInt64
		if err := rows.Scan(&item.Slug, &item.ContentSize, &updatedBy, &item.CreatedAt, &item.UpdatedAt, &item.AssetCount, &item.AssetBytes); err != nil {
			return nil, err
		}
		if updatedBy.Valid {
			v := updatedBy.Int64
			item.UpdatedBy = &v
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *customPageRepository) CountPages(ctx context.Context) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("nil custom page repository")
	}
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM custom_pages`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *customPageRepository) UpsertPage(ctx context.Context, page *service.CustomPage) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil custom page repository")
	}
	if page == nil {
		return fmt.Errorf("nil custom page")
	}
	now := time.Now().UTC()
	// created_at 只在首次插入时写入；覆盖保存只推进 updated_at 与修改者。
	return r.db.QueryRowContext(ctx, `
INSERT INTO custom_pages (slug, content, content_size, updated_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (slug) DO UPDATE
SET content = EXCLUDED.content,
    content_size = EXCLUDED.content_size,
    updated_by = EXCLUDED.updated_by,
    updated_at = EXCLUDED.updated_at
RETURNING created_at, updated_at`,
		page.Slug, page.Content, page.ContentSize, nullInt64Ptr(page.UpdatedBy), now,
	).Scan(&page.CreatedAt, &page.UpdatedAt)
}

func (r *customPageRepository) DeletePage(ctx context.Context, slug string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil custom page repository")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM custom_pages WHERE slug = $1`, slug)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrCustomPageNotFound
	}
	return nil
}

func (r *customPageRepository) GetAsset(ctx context.Context, slug, assetPath string) (*service.CustomPageAsset, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil custom page repository")
	}
	row := r.db.QueryRowContext(ctx, `SELECT `+customPageAssetFullCols+` FROM custom_page_assets WHERE slug = $1 AND path = $2`, slug, assetPath)
	var asset service.CustomPageAsset
	if err := row.Scan(&asset.Slug, &asset.Path, &asset.ContentType, &asset.Size, &asset.CreatedAt, &asset.UpdatedAt, &asset.Data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrCustomPageAssetNotFound
		}
		return nil, err
	}
	return &asset, nil
}

func (r *customPageRepository) ListAssets(ctx context.Context, slug string) ([]service.CustomPageAsset, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil custom page repository")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+customPageAssetMetaCols+` FROM custom_page_assets WHERE slug = $1 ORDER BY path`, slug)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.CustomPageAsset, 0)
	for rows.Next() {
		var asset service.CustomPageAsset
		if err := rows.Scan(&asset.Slug, &asset.Path, &asset.ContentType, &asset.Size, &asset.CreatedAt, &asset.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, asset)
	}
	return out, rows.Err()
}

func (r *customPageRepository) CountAssets(ctx context.Context, slug string) (int, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("nil custom page repository")
	}
	var count int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM custom_page_assets WHERE slug = $1`, slug).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *customPageRepository) UpsertAsset(ctx context.Context, asset *service.CustomPageAsset) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil custom page repository")
	}
	if asset == nil {
		return fmt.Errorf("nil custom page asset")
	}
	now := time.Now().UTC()
	err := r.db.QueryRowContext(ctx, `
INSERT INTO custom_page_assets (slug, path, content_type, size, data, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT (slug, path) DO UPDATE
SET content_type = EXCLUDED.content_type,
    size = EXCLUDED.size,
    data = EXCLUDED.data,
    updated_at = EXCLUDED.updated_at
RETURNING created_at, updated_at`,
		asset.Slug, asset.Path, asset.ContentType, asset.Size, asset.Data, now,
	).Scan(&asset.CreatedAt, &asset.UpdatedAt)
	if err != nil && isForeignKeyViolation(err) {
		// 外键指向 custom_pages(slug)：页面不存在时由数据库兜底，不需要先查一遍页面。
		return service.ErrCustomPageNotFound
	}
	return err
}

func (r *customPageRepository) DeleteAsset(ctx context.Context, slug, assetPath string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil custom page repository")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM custom_page_assets WHERE slug = $1 AND path = $2`, slug, assetPath)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrCustomPageAssetNotFound
	}
	return nil
}

func (r *customPageRepository) ImportIfAbsent(ctx context.Context, pages []service.CustomPage, assets []service.CustomPageAsset) (int, int, error) {
	if r == nil || r.db == nil {
		return 0, 0, fmt.Errorf("nil custom page repository")
	}
	if len(pages) == 0 && len(assets) == 0 {
		return 0, 0, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	insertedPages := 0
	for i := range pages {
		page := &pages[i]
		res, err := tx.ExecContext(ctx, customPageInsertIgnore, page.Slug, page.Content, page.ContentSize, nullInt64Ptr(page.UpdatedBy), now)
		if err != nil {
			return 0, 0, fmt.Errorf("import page %s: %w", page.Slug, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		insertedPages += int(n)
	}

	insertedAssets := 0
	for i := range assets {
		asset := &assets[i]
		res, err := tx.ExecContext(ctx, customPageAssetInsIgnore, asset.Slug, asset.Path, asset.ContentType, asset.Size, asset.Data, now)
		if err != nil {
			return 0, 0, fmt.Errorf("import asset %s/%s: %w", asset.Slug, asset.Path, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		insertedAssets += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return insertedPages, insertedAssets, nil
}

func scanCustomPage(row *sql.Row) (*service.CustomPage, error) {
	var page service.CustomPage
	var updatedBy sql.NullInt64
	if err := row.Scan(&page.Slug, &page.Content, &page.ContentSize, &updatedBy, &page.CreatedAt, &page.UpdatedAt); err != nil {
		return nil, err
	}
	if updatedBy.Valid {
		v := updatedBy.Int64
		page.UpdatedBy = &v
	}
	return &page, nil
}

func isForeignKeyViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code) == pgForeignKeyViolation
	}
	return false
}

// customPageLegacyImportSettingKey 是"旧版磁盘目录已导入"标记在 settings 表里的键。
//
// 放 settings 而不是新开一张表：这是一条一次性的部署事实，settings 已经是这类
// 键值的去处，也省掉一次会与上游迁移号相撞的新迁移。
const customPageLegacyImportSettingKey = "custom_pages_legacy_import_done"

// LegacyImportDone 报告磁盘目录是否已经导入过。
func (r *customPageRepository) LegacyImportDone(ctx context.Context, dir string) (bool, error) {
	if r == nil || r.db == nil {
		return false, fmt.Errorf("nil custom page repository")
	}
	var value string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, customPageLegacyImportSettingKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value != "", nil
}

// MarkLegacyImportDone 落下"已导入"标记；已存在时不覆盖（第一次的记录更有价值）。
func (r *customPageRepository) MarkLegacyImportDone(ctx context.Context, dir string, note string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil custom page repository")
	}
	value := fmt.Sprintf("%s|%s|%s", time.Now().UTC().Format(time.RFC3339), dir, note)
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW()) ON CONFLICT (key) DO NOTHING`,
		customPageLegacyImportSettingKey, value)
	return err
}
