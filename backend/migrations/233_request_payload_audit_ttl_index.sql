-- TTL cleanup also removes zero-byte usage placeholders, so it needs a
-- non-partial created_at index rather than the payload-only retention index.
CREATE INDEX IF NOT EXISTS idx_request_payload_logs_ttl
    ON request_payload_logs (created_at ASC, client_request_id ASC, api_key_id ASC);
