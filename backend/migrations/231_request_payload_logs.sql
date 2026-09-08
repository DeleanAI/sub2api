-- Bounded, redacted gateway request/response payload history. This table is
-- deliberately not linked by a foreign key to usage_logs: usage persistence is
-- asynchronous and either row may be written first.
CREATE TABLE IF NOT EXISTS request_payload_logs (
    client_request_id TEXT NOT NULL,
    api_key_id BIGINT NOT NULL,
    usage_request_id TEXT,
    request_body TEXT NOT NULL DEFAULT '',
    response_body TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    stored_bytes BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (client_request_id, api_key_id),
    CONSTRAINT request_payload_logs_stored_bytes_nonnegative CHECK (stored_bytes >= 0)
);

-- A usage row can only resolve to one payload record for its API key. The
-- partial index keeps unbound, in-flight capture rows inexpensive.
CREATE UNIQUE INDEX IF NOT EXISTS idx_request_payload_logs_usage_request_api_key
    ON request_payload_logs (usage_request_id, api_key_id)
    WHERE usage_request_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_request_payload_logs_retention
    ON request_payload_logs (updated_at DESC, client_request_id DESC);
