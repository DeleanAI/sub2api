-- Keep the logical payload budget in one small row. Background writers lock
-- this row per batch instead of acquiring a global advisory lock per request.
CREATE TABLE IF NOT EXISTS request_payload_audit_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    total_bytes BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Initialize from existing retained payloads exactly once when upgrading from
-- migration 231. New installations simply receive a zero-byte counter.
INSERT INTO request_payload_audit_state (singleton, total_bytes)
SELECT TRUE, COALESCE(SUM(stored_bytes), 0)
FROM request_payload_logs
ON CONFLICT (singleton) DO NOTHING;

-- Retention deletes the oldest content in bounded batches. This partial index
-- excludes zero-byte usage placeholders and avoids a full-table sort.
CREATE INDEX IF NOT EXISTS idx_request_payload_logs_retention_created
    ON request_payload_logs (created_at ASC, client_request_id ASC, api_key_id ASC)
    WHERE stored_bytes > 0;

-- Placeholder expiry has a separate access pattern from payload retention.
CREATE INDEX IF NOT EXISTS idx_request_payload_logs_stale_placeholders
    ON request_payload_logs (updated_at ASC)
    WHERE stored_bytes = 0;
