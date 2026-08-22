-- 自定义页面（Markdown）及其附件改存 PostgreSQL（issue #7）。
-- 此前页面放在每个实例的 <DATA_DIR>/pages/<slug>.md、图片放在 <DATA_DIR>/pages/<slug>/，
-- 多副本部署时写入只落在处理请求的那个副本上，其它副本 404，且 emptyDir 重启即丢。
-- 共享状态必须进数据库；磁盘目录自本版本起只作为一次性导入源。
CREATE TABLE IF NOT EXISTS custom_pages (
    slug         VARCHAR(64) PRIMARY KEY,
    content      TEXT        NOT NULL,
    content_size INTEGER     NOT NULL DEFAULT 0,
    updated_by   BIGINT      NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS custom_page_assets (
    slug         VARCHAR(64)  NOT NULL REFERENCES custom_pages(slug) ON DELETE CASCADE,
    path         TEXT         NOT NULL,
    content_type VARCHAR(128) NOT NULL DEFAULT 'application/octet-stream',
    size         INTEGER      NOT NULL DEFAULT 0,
    data         BYTEA        NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (slug, path)
);

COMMENT ON TABLE custom_pages IS
    'Admin-authored Markdown pages served at /api/v1/pages/:slug; shared by all replicas (issue #7)';
COMMENT ON COLUMN custom_pages.content_size IS
    'Byte length of content, kept alongside so listings do not have to load the body';
COMMENT ON COLUMN custom_pages.updated_by IS
    'users.id of the last admin who saved the page; NULL for pages imported from disk';
COMMENT ON TABLE custom_page_assets IS
    'Binary attachments (images) of custom pages served at /api/v1/pages/:slug/images/*path';
COMMENT ON COLUMN custom_page_assets.path IS
    'Slash-separated relative path as referenced from the Markdown, e.g. images/logo.png';
