package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

// 自定义页面（issue #7）：页面正文与附件存 PostgreSQL，所有副本共享；
// 限制与校验只写在本文件，handler、导入器、前端契约都以这里为准。
const (
	// MaxCustomPageContentSize 页面正文上限（沿用磁盘时代的 1 MiB）。
	MaxCustomPageContentSize = 1 << 20
	// MaxCustomPageAssetSize 单个附件上限。
	MaxCustomPageAssetSize = 5 << 20
	// MaxCustomPageAssets 单页附件数量上限。
	MaxCustomPageAssets = 200
	// MaxCustomPageSlugLength slug 长度上限（与 custom_pages.slug VARCHAR(64) 一致）。
	MaxCustomPageSlugLength = 64
	// MaxCustomPageAssetPathLength 附件相对路径长度上限。
	MaxCustomPageAssetPathLength = 512
	// maxCustomPageContentTypeLength 与 custom_page_assets.content_type VARCHAR(128) 一致。
	maxCustomPageContentTypeLength = 128

	customPageDefaultContentType = "application/octet-stream"
	customPageMarkdownExt        = ".md"

	// customPageImportLeaderLockKey 把一次性磁盘导入串行化到单个副本；TTL 只做崩溃兜底。
	customPageImportLeaderLockKey = "custom_pages:import:leader"
	customPageImportLeaderLockTTL = 5 * time.Minute
	customPageImportLockWait      = 10 * time.Second
	customPageImportLockRetry     = 500 * time.Millisecond
	// customPageImportLogListLimit 警告日志里最多列出的文件数，避免刷屏。
	customPageImportLogListLimit = 50
)

var customPageSlugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// customPageAssetAllowedMediaTypes 是页面附件允许的媒体类型白名单。
//
// 这里之前是黑名单，而黑名单在这个位置注定不完整：它挡住了 html/xhtml/javascript，
// 却放过 image/svg+xml——SVG 里可以带 <script>，附件又是同源、内联提供的，于是
// "管理员上传一张图"就等于在每个访客的会话里执行脚本。text/xml、application/xml
// 同理。补上这三个只是修掉今天这一个，下一种活动类型照样会漏。
//
// 白名单反过来：没在表里的一律拒绝，新类型必须有人显式判断它是否安全才能加进来。
// 精确类型而不是 image/* 这样的前缀匹配，正是为了不把 image/svg+xml 顺手放进来。
var customPageAssetAllowedMediaTypes = map[string]struct{}{
	"image/png":                {},
	"image/jpeg":               {},
	"image/gif":                {},
	"image/webp":               {},
	"image/avif":               {},
	"image/bmp":                {},
	"image/x-icon":             {},
	"image/vnd.microsoft.icon": {},
	"image/tiff":               {},
	"video/mp4":                {},
	"video/webm":               {},
	"audio/mpeg":               {},
	"audio/ogg":                {},
	"audio/wav":                {},
	"font/woff":                {},
	"font/woff2":               {},
	"font/ttf":                 {},
	"font/otf":                 {},
	"application/pdf":          {},
	"text/plain":               {},
	"text/csv":                 {},
	"text/markdown":            {},
	// 认不出扩展名时的兜底类型：不透明字节流，浏览器不会执行它，配合响应上的
	// nosniff + sandbox CSP 无法变成活动内容。保留它是为了不把"传个 zip"一起关掉。
	customPageDefaultContentType: {},
}

var (
	ErrCustomPageNotFound         = infraerrors.NotFound("CUSTOM_PAGE_NOT_FOUND", "page not found")
	ErrCustomPageAssetNotFound    = infraerrors.NotFound("CUSTOM_PAGE_ASSET_NOT_FOUND", "page asset not found")
	ErrCustomPageInvalidSlug      = infraerrors.BadRequest("CUSTOM_PAGE_SLUG_INVALID", "invalid page slug")
	ErrCustomPageInvalidAssetPath = infraerrors.BadRequest("CUSTOM_PAGE_ASSET_PATH_INVALID", "invalid asset path")
	ErrCustomPageContentInvalid   = infraerrors.BadRequest("CUSTOM_PAGE_CONTENT_INVALID", "page content must be valid UTF-8 text")
	ErrCustomPageContentTooLarge  = infraerrors.New(http.StatusRequestEntityTooLarge, "CUSTOM_PAGE_CONTENT_TOO_LARGE",
		fmt.Sprintf("page content exceeds %d bytes", MaxCustomPageContentSize))
	ErrCustomPageAssetEmpty    = infraerrors.BadRequest("CUSTOM_PAGE_ASSET_EMPTY", "asset body is empty")
	ErrCustomPageAssetTooLarge = infraerrors.New(http.StatusRequestEntityTooLarge, "CUSTOM_PAGE_ASSET_TOO_LARGE",
		fmt.Sprintf("asset exceeds %d bytes", MaxCustomPageAssetSize))
	ErrCustomPageTooManyAssets = infraerrors.BadRequest("CUSTOM_PAGE_TOO_MANY_ASSETS",
		fmt.Sprintf("a page may hold at most %d assets", MaxCustomPageAssets))
	ErrCustomPageAssetContentTypeRejected = infraerrors.BadRequest("CUSTOM_PAGE_ASSET_CONTENT_TYPE_REJECTED",
		"active content types (html/script) are not allowed as page assets")
)

// CustomPage 是一个 Markdown 自定义页面。
type CustomPage struct {
	Slug        string
	Content     string
	ContentSize int
	UpdatedBy   *int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CustomPageSummary 是列表用的页面概要（不带正文）。
type CustomPageSummary struct {
	Slug        string
	ContentSize int
	AssetCount  int
	AssetBytes  int64
	UpdatedBy   *int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CustomPageAsset 是页面附件；列表场景下 Data 为 nil。
type CustomPageAsset struct {
	Slug        string
	Path        string
	ContentType string
	Size        int
	Data        []byte
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CustomPageRepository 自定义页面仓储。
type CustomPageRepository interface {
	GetPage(ctx context.Context, slug string) (*CustomPage, error)
	ListPages(ctx context.Context) ([]CustomPageSummary, error)
	CountPages(ctx context.Context) (int, error)
	// UpsertPage 插入或覆盖页面，并把持久化后的 CreatedAt/UpdatedAt 回填到 page。
	UpsertPage(ctx context.Context, page *CustomPage) error
	DeletePage(ctx context.Context, slug string) error

	GetAsset(ctx context.Context, slug, assetPath string) (*CustomPageAsset, error)
	ListAssets(ctx context.Context, slug string) ([]CustomPageAsset, error)
	CountAssets(ctx context.Context, slug string) (int, error)
	// UpsertAsset 插入或覆盖附件；页面不存在时返回 ErrCustomPageNotFound。
	UpsertAsset(ctx context.Context, asset *CustomPageAsset) error
	DeleteAsset(ctx context.Context, slug, assetPath string) error

	// ImportIfAbsent 在一个事务里写入页面与附件，已存在的键跳过（ON CONFLICT DO NOTHING），
	// 返回实际插入的条数。用于一次性磁盘导入，多副本同时启动也不会重复写入。
	ImportIfAbsent(ctx context.Context, pages []CustomPage, assets []CustomPageAsset) (insertedPages, insertedAssets int, err error)

	// LegacyImportDone / MarkLegacyImportDone 是"磁盘目录已经导入过"的持久标记。
	//
	// 没有这个标记时，唯一的判据只能是"表里有没有行"，而这个判据每次启动都重算：
	// 管理员依法删掉最后一个页面（例如法务要求撤下旧版 ToS），下一次重启就会把
	// 磁盘上那份撤下的文本重新导回来并对外提供。Compose 默认把 /app/data 挂在
	// 持久卷上，所以磁盘文件一直都在。标记只写一次，之后与表里有几行无关。
	LegacyImportDone(ctx context.Context, dir string) (bool, error)
	MarkLegacyImportDone(ctx context.Context, dir string, note string) error
}

// CustomPageService 自定义页面服务。
type CustomPageService struct {
	repo       CustomPageRepository
	lockCache  LeaderLockCache
	db         *sql.DB
	instanceID string
}

// NewCustomPageService 创建自定义页面服务。
func NewCustomPageService(repo CustomPageRepository) *CustomPageService {
	return &CustomPageService{
		repo:       repo,
		instanceID: uuid.NewString(),
	}
}

// SetLeaderLock 注入跨副本互斥所需的 Redis 锁与 DB（advisory lock 兜底），只影响磁盘导入。
func (s *CustomPageService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// ValidateCustomPageSlug 校验 slug：字母数字开头，只含字母数字、下划线、连字符，长度 ≤ 64。
func ValidateCustomPageSlug(slug string) error {
	if slug == "" || len(slug) > MaxCustomPageSlugLength || !customPageSlugPattern.MatchString(slug) {
		return ErrCustomPageInvalidSlug
	}
	return nil
}

// NormalizeCustomPageAssetPath 把请求/Markdown 里的相对路径规整为存储键：
// URL 解码、丢弃空段与 "."、拒绝 ".."/反斜杠/NUL/控制字符/绝对路径/盘符，分隔符统一为 "/"。
// 这是附件路径校验的唯一实现：读、写、删、导入都经过它。
func NormalizeCustomPageAssetPath(raw string) (string, error) {
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", ErrCustomPageInvalidAssetPath
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", ErrCustomPageInvalidAssetPath
	}
	if decoded == "" || strings.HasPrefix(decoded, "/") || strings.Contains(decoded, "\\") {
		return "", ErrCustomPageInvalidAssetPath
	}
	for _, r := range decoded {
		if r < 0x20 || r == 0x7f {
			return "", ErrCustomPageInvalidAssetPath
		}
	}

	parts := make([]string, 0, 4)
	for _, part := range strings.Split(decoded, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", ErrCustomPageInvalidAssetPath
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", ErrCustomPageInvalidAssetPath
	}

	normalized := path.Join(parts...)
	if filepath.IsAbs(normalized) || filepath.VolumeName(normalized) != "" ||
		len(normalized) > MaxCustomPageAssetPathLength || !utf8.ValidString(normalized) {
		return "", ErrCustomPageInvalidAssetPath
	}
	return normalized, nil
}

// GetPage 读取页面正文。
func (s *CustomPageService) GetPage(ctx context.Context, slug string) (*CustomPage, error) {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return nil, err
	}
	return s.repo.GetPage(ctx, slug)
}

// ListPages 列出全部页面概要。
func (s *CustomPageService) ListPages(ctx context.Context) ([]CustomPageSummary, error) {
	return s.repo.ListPages(ctx)
}

// SavePage 新建或覆盖页面正文；actorID 记录最后修改者。
func (s *CustomPageService) SavePage(ctx context.Context, slug, content string, actorID *int64) (*CustomPage, error) {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return nil, err
	}
	if err := validateCustomPageContent(content); err != nil {
		return nil, err
	}
	page := &CustomPage{
		Slug:        slug,
		Content:     content,
		ContentSize: len(content),
		UpdatedBy:   actorID,
	}
	if err := s.repo.UpsertPage(ctx, page); err != nil {
		return nil, err
	}
	return page, nil
}

// DeletePage 删除页面及其全部附件（外键级联）。
func (s *CustomPageService) DeletePage(ctx context.Context, slug string) error {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return err
	}
	return s.repo.DeletePage(ctx, slug)
}

// GetAsset 读取附件（含二进制内容）。
func (s *CustomPageService) GetAsset(ctx context.Context, slug, rawPath string) (*CustomPageAsset, error) {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return nil, err
	}
	assetPath, err := NormalizeCustomPageAssetPath(rawPath)
	if err != nil {
		return nil, err
	}
	return s.repo.GetAsset(ctx, slug, assetPath)
}

// ListAssets 列出页面附件（不含内容）。
func (s *CustomPageService) ListAssets(ctx context.Context, slug string) ([]CustomPageAsset, error) {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return nil, err
	}
	return s.repo.ListAssets(ctx, slug)
}

// SaveAsset 新建或覆盖附件。declaredContentType 为空或 octet-stream 时按扩展名推断。
func (s *CustomPageService) SaveAsset(ctx context.Context, slug, rawPath, declaredContentType string, data []byte) (*CustomPageAsset, error) {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return nil, err
	}
	assetPath, err := NormalizeCustomPageAssetPath(rawPath)
	if err != nil {
		return nil, err
	}
	if err := validateCustomPageAssetSize(len(data)); err != nil {
		return nil, err
	}
	contentType, err := normalizeCustomPageAssetContentType(declaredContentType, assetPath)
	if err != nil {
		return nil, err
	}

	// 数量上限：覆盖已有路径不计入新增。计数与写入不在同一事务，管理员并发上传可能
	// 短暂超出上限几条；这是后台低频操作，不值得为此加表锁。
	count, err := s.repo.CountAssets(ctx, slug)
	if err != nil {
		return nil, err
	}
	if count >= MaxCustomPageAssets {
		if _, err := s.repo.GetAsset(ctx, slug, assetPath); err != nil {
			if errors.Is(err, ErrCustomPageAssetNotFound) {
				return nil, ErrCustomPageTooManyAssets
			}
			return nil, err
		}
	}

	asset := &CustomPageAsset{
		Slug:        slug,
		Path:        assetPath,
		ContentType: contentType,
		Size:        len(data),
		Data:        data,
	}
	if err := s.repo.UpsertAsset(ctx, asset); err != nil {
		return nil, err
	}
	return asset, nil
}

// DeleteAsset 删除单个附件。
func (s *CustomPageService) DeleteAsset(ctx context.Context, slug, rawPath string) error {
	if err := ValidateCustomPageSlug(slug); err != nil {
		return err
	}
	assetPath, err := NormalizeCustomPageAssetPath(rawPath)
	if err != nil {
		return err
	}
	return s.repo.DeleteAsset(ctx, slug, assetPath)
}

func validateCustomPageContent(content string) error {
	if len(content) > MaxCustomPageContentSize {
		return ErrCustomPageContentTooLarge
	}
	// PostgreSQL TEXT 拒绝 NUL 与非法 UTF-8；在这里拦下来给出明确错误，而不是把数据库报错透出去。
	if !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return ErrCustomPageContentInvalid
	}
	return nil
}

func validateCustomPageAssetSize(size int) error {
	if size <= 0 {
		return ErrCustomPageAssetEmpty
	}
	if size > MaxCustomPageAssetSize {
		return ErrCustomPageAssetTooLarge
	}
	return nil
}

// normalizeCustomPageAssetContentType 取声明的媒体类型（丢弃参数），缺省按扩展名推断，
// 最后落到 application/octet-stream；活动内容类型一律拒绝。
func normalizeCustomPageAssetContentType(declared, assetPath string) (string, error) {
	mediaType := ""
	if strings.TrimSpace(declared) != "" {
		if parsed, _, err := mime.ParseMediaType(declared); err == nil {
			mediaType = strings.ToLower(parsed)
		}
	}
	if mediaType == "" || mediaType == customPageDefaultContentType {
		if byExt := mime.TypeByExtension(strings.ToLower(path.Ext(assetPath))); byExt != "" {
			if parsed, _, err := mime.ParseMediaType(byExt); err == nil {
				mediaType = strings.ToLower(parsed)
			}
		}
	}
	if mediaType == "" {
		mediaType = customPageDefaultContentType
	}
	if _, allowed := customPageAssetAllowedMediaTypes[mediaType]; !allowed {
		return "", ErrCustomPageAssetContentTypeRejected
	}
	if len(mediaType) > maxCustomPageContentTypeLength {
		return "", ErrCustomPageAssetContentTypeRejected
	}
	return mediaType, nil
}

// CustomPageImportReport 记录一次磁盘导入的结果，供日志与测试使用。
type CustomPageImportReport struct {
	Dir string
	// DirMissing 表示目录不存在（全新部署的常态）。
	DirMissing bool
	// AlreadyImported 表示持久标记已存在，本次连磁盘都没碰。
	AlreadyImported bool
	// LockHeldByPeer 表示等待超时后锁仍被其它副本持有，本副本放弃导入。
	LockHeldByPeer bool
	// IgnoredFiles 表示表已非空时磁盘上仍存在、被忽略的文件。
	IgnoredFiles []string
	// SkippedFiles 是未通过校验（slug/路径/大小/编码）而被跳过的文件，带原因。
	SkippedFiles   []string
	InsertedPages  int
	InsertedAssets int
}

type customPageImportCandidate struct {
	pages  []CustomPage
	assets []CustomPageAsset
	files  []string // 全部发现的文件（相对 dir），用于"表非空时被忽略"的警告
	skips  []string
}

// ImportFromDisk 把 <dir>/<slug>.md 与 <dir>/<slug>/** 一次性导入数据库。
// 只在 custom_pages 为空时执行；表非空时磁盘文件被忽略并打警告（列出文件名）。
// 多副本同时启动时通过 leader lock 串行化，写入本身也是 ON CONFLICT DO NOTHING。
func (s *CustomPageService) ImportFromDisk(ctx context.Context, dir string) (*CustomPageImportReport, error) {
	report := &CustomPageImportReport{Dir: dir}

	// 先看标记，再碰磁盘。扫描会把整个目录（含每个附件的全部字节）读进内存，
	// 而绝大多数启动都是"早就导入过"——按上限 200 个附件 × 5 MiB 算是 ~1 GiB 常驻，
	// 发生在监听端口之前，512Mi 的 Pod 会在数据库里明明已有内容的情况下反复 OOM。
	done, err := s.repo.LegacyImportDone(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("check custom pages import marker: %w", err)
	}
	if done {
		report.AlreadyImported = true
		slog.Debug("custom_pages.import_skipped: legacy dir was already imported once", "dir", dir)
		return report, nil
	}

	candidate, err := scanCustomPagesDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			report.DirMissing = true
			slog.Debug("custom_pages.import: legacy pages dir does not exist, nothing to import", "dir", dir)
			return report, nil
		}
		return nil, fmt.Errorf("scan custom pages dir %s: %w", dir, err)
	}
	report.SkippedFiles = candidate.skips
	if len(candidate.files) == 0 {
		slog.Debug("custom_pages.import: legacy pages dir is empty, nothing to import", "dir", dir)
		return report, nil
	}

	release, acquired := s.acquireImportLock(ctx)
	if !acquired {
		report.LockHeldByPeer = true
		slog.Warn("custom_pages.import_skipped: another instance holds the import lock; "+
			"the peer imports the files, this instance serves from the database",
			"dir", dir, "files", len(candidate.files))
		return report, nil
	}
	defer release()

	existing, err := s.repo.CountPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("count custom pages: %w", err)
	}
	if existing > 0 {
		report.IgnoredFiles = candidate.files
		slog.Warn("custom_pages.disk_files_ignored: custom_pages already has rows, files under the legacy pages dir are never read again; "+
			"manage pages via the admin UI/API and remove the directory",
			"dir", dir, "existing_pages", existing, "ignored", truncateImportList(candidate.files))
		// 表已有内容说明导入这件事已经了结（这一版导入过，或页面本就由管理界面创建）。
		// 落标记，否则"表非空"这个判据每次启动都重算，删空页面后磁盘内容会复活。
		s.markLegacyImportDone(ctx, dir, fmt.Sprintf("custom_pages already had %d row(s)", existing))
		return report, nil
	}

	if len(candidate.skips) > 0 {
		// ERROR 而不是 WARN：磁盘时代 c.File() 对大小没有任何限制，被跳过的文件是
		// 升级前真的在对外提供、升级后不再提供的内容，而且管理员也无法重新上传
		// （写入路径用的是同一组上限）。这不是"顺带提一句"，是需要人处理的降级。
		slog.Error("custom_pages.import_dropped_content: these files were served before the upgrade and are NOT imported; "+
			"the corresponding pages/assets will 404 until the content is reduced under the limits and uploaded via the admin UI",
			"dir", dir,
			"page_limit_bytes", MaxCustomPageContentSize,
			"asset_limit_bytes", MaxCustomPageAssetSize,
			"dropped", truncateImportList(candidate.skips))
	}
	if len(candidate.pages) == 0 {
		slog.Warn("custom_pages.import: no importable page found", "dir", dir, "files", len(candidate.files))
		return report, nil
	}

	insertedPages, insertedAssets, err := s.repo.ImportIfAbsent(ctx, candidate.pages, candidate.assets)
	if err != nil {
		return nil, fmt.Errorf("import custom pages: %w", err)
	}
	report.InsertedPages = insertedPages
	report.InsertedAssets = insertedAssets
	slog.Info("custom_pages.imported_from_disk",
		"count", insertedPages, "assets", insertedAssets, "dir", dir,
		"candidates_pages", len(candidate.pages), "candidates_assets", len(candidate.assets))
	s.markLegacyImportDone(ctx, dir, fmt.Sprintf("imported %d page(s), %d asset(s)", insertedPages, insertedAssets))
	return report, nil
}

// markLegacyImportDone 落"已导入"标记。写失败不改变本次导入的结果（内容已经进库），
// 但必须留痕：标记没落上，下一次启动会重新扫描目录，删空页面后磁盘内容会复活。
func (s *CustomPageService) markLegacyImportDone(ctx context.Context, dir, note string) {
	if err := s.repo.MarkLegacyImportDone(ctx, dir, note); err != nil {
		slog.Error("custom_pages.import_marker_write_failed: the legacy dir will be scanned again on the next start; "+
			"if every page is later deleted, the disk copies would be re-imported",
			"dir", dir, "note", note, "error", err)
	}
}

// acquireImportLock 在有限时间内反复尝试拿导入锁：同时启动的副本里，后来者等前者导入完成后
// 再看表是否已有数据，而不是一开始就放弃。
func (s *CustomPageService) acquireImportLock(ctx context.Context) (func(), bool) {
	deadline := time.Now().Add(customPageImportLockWait)
	for {
		release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, customPageImportLeaderLockKey, s.instanceID, customPageImportLeaderLockTTL)
		if ok {
			return release, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(customPageImportLockRetry):
		}
	}
}

// scanCustomPagesDir 扫描旧版磁盘布局：<dir>/<slug>.md 是页面，<dir>/<slug>/** 是该页面的附件。
// 校验失败的文件记录到 skips（带原因），不会让整个导入失败。
func scanCustomPagesDir(dir string) (*customPageImportCandidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := &customPageImportCandidate{}
	pageSlugs := make(map[string]struct{})
	assetDirs := make([]string, 0)

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			assetDirs = append(assetDirs, name)
			continue
		}
		if !strings.HasSuffix(name, customPageMarkdownExt) {
			out.files = append(out.files, name)
			out.skips = append(out.skips, name+": not a .md page")
			continue
		}
		out.files = append(out.files, name)
		slug := strings.TrimSuffix(name, customPageMarkdownExt)
		if err := ValidateCustomPageSlug(slug); err != nil {
			out.skips = append(out.skips, name+": invalid slug")
			continue
		}
		info, err := entry.Info()
		if err != nil {
			out.skips = append(out.skips, name+": "+err.Error())
			continue
		}
		if info.Size() > MaxCustomPageContentSize {
			out.skips = append(out.skips, fmt.Sprintf("%s: %d bytes exceeds page limit %d", name, info.Size(), MaxCustomPageContentSize))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			out.skips = append(out.skips, name+": "+err.Error())
			continue
		}
		content := string(raw)
		if err := validateCustomPageContent(content); err != nil {
			out.skips = append(out.skips, name+": "+err.Error())
			continue
		}
		pageSlugs[slug] = struct{}{}
		out.pages = append(out.pages, CustomPage{Slug: slug, Content: content, ContentSize: len(content)})
	}

	for _, slug := range assetDirs {
		if _, ok := pageSlugs[slug]; !ok {
			out.files = append(out.files, slug+"/")
			out.skips = append(out.skips, slug+"/: asset directory without an importable "+slug+customPageMarkdownExt)
			continue
		}
		base := filepath.Join(dir, slug)
		walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				out.skips = append(out.skips, relImportPath(dir, p)+": "+err.Error())
				return nil
			}
			if d.IsDir() {
				return nil
			}
			rel := relImportPath(dir, p)
			out.files = append(out.files, rel)
			relToPage := filepath.ToSlash(strings.TrimPrefix(rel, slug+"/"))
			assetPath, err := NormalizeCustomPageAssetPath(relToPage)
			if err != nil {
				out.skips = append(out.skips, rel+": invalid asset path")
				return nil
			}
			info, err := d.Info()
			if err != nil {
				out.skips = append(out.skips, rel+": "+err.Error())
				return nil
			}
			if err := validateCustomPageAssetSize(int(info.Size())); err != nil {
				out.skips = append(out.skips, fmt.Sprintf("%s: %d bytes: %s", rel, info.Size(), err.Error()))
				return nil
			}
			contentType, err := normalizeCustomPageAssetContentType("", assetPath)
			if err != nil {
				out.skips = append(out.skips, rel+": "+err.Error())
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				out.skips = append(out.skips, rel+": "+err.Error())
				return nil
			}
			out.assets = append(out.assets, CustomPageAsset{
				Slug:        slug,
				Path:        assetPath,
				ContentType: contentType,
				Size:        len(data),
				Data:        data,
			})
			return nil
		})
		if walkErr != nil {
			out.skips = append(out.skips, slug+"/: "+walkErr.Error())
		}
	}

	// 单页附件数上限同样适用于导入：超出的按路径排序后截断，并记录被丢弃的文件。
	perPage := make(map[string]int)
	kept := out.assets[:0]
	sort.SliceStable(out.assets, func(i, j int) bool {
		if out.assets[i].Slug != out.assets[j].Slug {
			return out.assets[i].Slug < out.assets[j].Slug
		}
		return out.assets[i].Path < out.assets[j].Path
	})
	for _, asset := range out.assets {
		if perPage[asset.Slug] >= MaxCustomPageAssets {
			out.skips = append(out.skips, fmt.Sprintf("%s/%s: page already has %d assets", asset.Slug, asset.Path, MaxCustomPageAssets))
			continue
		}
		perPage[asset.Slug]++
		kept = append(kept, asset)
	}
	out.assets = kept
	sort.Strings(out.files)
	return out, nil
}

func relImportPath(dir, p string) string {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

func truncateImportList(items []string) []string {
	if len(items) <= customPageImportLogListLimit {
		return items
	}
	out := make([]string, 0, customPageImportLogListLimit+1)
	out = append(out, items[:customPageImportLogListLimit]...)
	out = append(out, fmt.Sprintf("... and %d more", len(items)-customPageImportLogListLimit))
	return out
}
