package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	bytesPerMiB = int64(1024 * 1024)

	defaultRequestPayloadAuditQueueSize     = 256
	defaultRequestPayloadAuditBatchSize     = 64
	defaultRequestPayloadAuditFlushInterval = 500 * time.Millisecond
	defaultRequestPayloadAuditWriteTimeout  = 3 * time.Second
	defaultRequestPayloadAuditRetentionDays = 7
	requestPayloadAuditPruneBatchSize       = 256
	requestPayloadAuditCleanupInterval      = 10 * time.Minute
	requestPayloadAuditPlaceholderPruneGap  = 10 * time.Minute
	requestPayloadAuditDropLogInterval      = 5 * time.Second
)

type requestPayloadAuditContextKey struct{}

// RequestPayloadAuditCapture is the redacted, bounded data captured by the
// gateway middleware after a client request has completed.
type RequestPayloadAuditCapture struct {
	ClientRequestID string
	APIKeyID        int64
	RequestBody     string
	ResponseBody    string
	Metadata        map[string]any
}

// RequestPayloadAuditDetail is returned only after a caller has proven access
// to its corresponding usage record.
type RequestPayloadAuditDetail struct {
	RequestBody  string          `json:"request_body"`
	ResponseBody string          `json:"response_body"`
	Metadata     json.RawMessage `json:"metadata"`
	StoredBytes  int64           `json:"stored_bytes"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// RequestPayloadAuditStats exposes the in-process health of the asynchronous
// persistence path. Counters reset on restart; StorageBytes is the latest
// committed logical storage total observed by this process.
type RequestPayloadAuditStats struct {
	QueueCapacity int    `json:"queue_capacity"`
	QueueDepth    int    `json:"queue_depth"`
	Enqueued      uint64 `json:"enqueued"`
	Dropped       uint64 `json:"dropped"`
	Persisted     uint64 `json:"persisted"`
	FailedBatches uint64 `json:"failed_batches"`
	PrunedRows    uint64 `json:"pruned_rows"`
	PrunedBytes   uint64 `json:"pruned_bytes"`
	StorageBytes  int64  `json:"storage_bytes"`
}

type requestPayloadAuditRecord struct {
	clientRequestID string
	apiKeyID        int64
	requestBody     string
	responseBody    string
	metadata        []byte
	storedBytes     int64
}

type requestPayloadAuditPersistResult struct {
	storageBytes int64
	prunedRows   int64
	prunedBytes  int64
}

// RequestPayloadAuditService owns persistent request/response history. The
// request path only enqueues already-redacted records; a single bounded worker
// batches writes so database latency cannot extend gateway request lifetime.
type RequestPayloadAuditService struct {
	db              *sql.DB
	enabled         bool
	maxStorageBytes int64
	maxEntryBytes   int64
	retentionDays   int

	queue         chan requestPayloadAuditRecord
	batchSize     int
	flushInterval time.Duration
	writeTimeout  time.Duration
	stopCh        chan struct{}
	stopped       bool
	queueMu       sync.RWMutex
	stopOnce      sync.Once
	workerWg      sync.WaitGroup

	enqueued      atomic.Uint64
	dropped       atomic.Uint64
	persisted     atomic.Uint64
	failedBatches atomic.Uint64
	prunedRows    atomic.Uint64
	prunedBytes   atomic.Uint64
	storageBytes  atomic.Int64
	lastDropLog   atomic.Int64
	lastCleanup   atomic.Int64
}

func NewRequestPayloadAuditService(db *sql.DB, cfg *config.Config) *RequestPayloadAuditService {
	svc := &RequestPayloadAuditService{db: db}
	if cfg == nil || !cfg.Gateway.RequestPayloadAudit.Enabled || db == nil {
		return svc
	}

	auditCfg := cfg.Gateway.RequestPayloadAudit
	svc.enabled = true
	svc.maxStorageBytes = int64(auditCfg.MaxStorageMB) * bytesPerMiB
	svc.maxEntryBytes = int64(auditCfg.MaxEntryMB) * bytesPerMiB
	svc.retentionDays = auditCfg.RetentionDays
	if svc.retentionDays <= 0 {
		svc.retentionDays = defaultRequestPayloadAuditRetentionDays
	}
	svc.batchSize = auditCfg.BatchSize
	if svc.batchSize <= 0 {
		svc.batchSize = defaultRequestPayloadAuditBatchSize
	}
	queueSize := auditCfg.QueueSize
	if queueSize <= 0 {
		queueSize = defaultRequestPayloadAuditQueueSize
	}
	if svc.batchSize > queueSize {
		svc.batchSize = queueSize
	}
	svc.flushInterval = time.Duration(auditCfg.FlushIntervalMilliseconds) * time.Millisecond
	if svc.flushInterval <= 0 {
		svc.flushInterval = defaultRequestPayloadAuditFlushInterval
	}
	svc.writeTimeout = time.Duration(auditCfg.WriteTimeoutSeconds) * time.Second
	if svc.writeTimeout <= 0 {
		svc.writeTimeout = defaultRequestPayloadAuditWriteTimeout
	}
	svc.queue = make(chan requestPayloadAuditRecord, queueSize)
	svc.stopCh = make(chan struct{})
	svc.workerWg.Add(1)
	go svc.run()
	return svc
}

func (s *RequestPayloadAuditService) Enabled() bool {
	return s != nil && s.enabled && s.db != nil
}

// CaptureLimits reserves most of the entry budget for the user input. The
// middleware uses these exact limits to keep its per-request memory bounded.
func (s *RequestPayloadAuditService) CaptureLimits() (requestLimit, responseLimit int64) {
	if s == nil || s.maxEntryBytes <= 0 {
		return 0, 0
	}
	return requestPayloadCaptureLimits(s.maxEntryBytes)
}

// ContextWithRequestPayloadAudit preserves the service across detached usage
// recording contexts without making lower layers depend on Gin.
func ContextWithRequestPayloadAudit(ctx context.Context, audit *RequestPayloadAuditService) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if audit == nil {
		return ctx
	}
	return context.WithValue(ctx, requestPayloadAuditContextKey{}, audit)
}

func RequestPayloadAuditFromContext(ctx context.Context) *RequestPayloadAuditService {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(requestPayloadAuditContextKey{}).(*RequestPayloadAuditService)
	return audit
}

// EnqueueCapture accepts a record without waiting for database I/O. A full
// queue intentionally drops audit history rather than delaying a model result.
func (s *RequestPayloadAuditService) EnqueueCapture(capture RequestPayloadAuditCapture) bool {
	if !s.Enabled() {
		return true
	}
	record, err := s.prepareRecord(capture)
	if err != nil {
		s.dropped.Add(1)
		logger.L().With(zap.String("component", "service.request_payload_audit"), zap.Error(err)).Warn("request_payload_audit.capture_rejected")
		return false
	}

	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	if s.stopped {
		s.dropped.Add(1)
		return false
	}
	select {
	case s.queue <- record:
		s.enqueued.Add(1)
		return true
	default:
		s.dropped.Add(1)
		s.logDrop()
		return false
	}
}

// PersistCapture remains available for deterministic maintenance and tests. The
// gateway must use EnqueueCapture so request completion is never blocked on DB.
func (s *RequestPayloadAuditService) PersistCapture(ctx context.Context, capture RequestPayloadAuditCapture) error {
	if !s.Enabled() {
		return nil
	}
	record, err := s.prepareRecord(capture)
	if err != nil {
		return err
	}
	result, err := s.persistBatch(ctx, []requestPayloadAuditRecord{record})
	if err != nil {
		return err
	}
	s.applyPersistResult(1, result)
	return nil
}

// BindUsage assigns the final usage request ID. It intentionally creates a
// zero-byte placeholder when async usage recording wins the race.
func (s *RequestPayloadAuditService) BindUsage(ctx context.Context, clientRequestID string, apiKeyID int64, usageRequestID string) error {
	if !s.Enabled() {
		return nil
	}
	clientRequestID = strings.TrimSpace(clientRequestID)
	usageRequestID = strings.TrimSpace(usageRequestID)
	if clientRequestID == "" || usageRequestID == "" || apiKeyID <= 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_payload_logs (client_request_id, api_key_id, usage_request_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (client_request_id, api_key_id) DO UPDATE SET
			usage_request_id = EXCLUDED.usage_request_id
	`, clientRequestID, apiKeyID, usageRequestID)
	if err != nil {
		return fmt.Errorf("bind request payload to usage: %w", err)
	}
	return nil
}

func (s *RequestPayloadAuditService) GetByUsageRequest(ctx context.Context, usageRequestID string, apiKeyID int64) (*RequestPayloadAuditDetail, error) {
	if !s.Enabled() {
		return nil, ErrRequestPayloadAuditNotFound
	}
	usageRequestID = strings.TrimSpace(usageRequestID)
	if usageRequestID == "" || apiKeyID <= 0 {
		return nil, ErrRequestPayloadAuditNotFound
	}

	detail := &RequestPayloadAuditDetail{}
	err := s.db.QueryRowContext(ctx, `
		SELECT request_body, response_body, metadata, stored_bytes, created_at, updated_at
		FROM request_payload_logs
		WHERE usage_request_id = $1
		  AND api_key_id = $2
		  AND created_at >= NOW() - ($3::int * INTERVAL '1 day')
	`, usageRequestID, apiKeyID, s.effectiveRetentionDays()).Scan(
		&detail.RequestBody,
		&detail.ResponseBody,
		&detail.Metadata,
		&detail.StoredBytes,
		&detail.CreatedAt,
		&detail.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRequestPayloadAuditNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get request payload by usage request: %w", err)
	}
	return detail, nil
}

// Stats is safe to call from an admin or metrics integration without touching
// the database on the gateway hot path.
func (s *RequestPayloadAuditService) Stats() RequestPayloadAuditStats {
	if s == nil {
		return RequestPayloadAuditStats{}
	}
	stats := RequestPayloadAuditStats{
		Enqueued:      s.enqueued.Load(),
		Dropped:       s.dropped.Load(),
		Persisted:     s.persisted.Load(),
		FailedBatches: s.failedBatches.Load(),
		PrunedRows:    s.prunedRows.Load(),
		PrunedBytes:   s.prunedBytes.Load(),
		StorageBytes:  s.storageBytes.Load(),
	}
	if s.queue != nil {
		stats.QueueCapacity = cap(s.queue)
		stats.QueueDepth = len(s.queue)
	}
	return stats
}

// Stop rejects new captures, drains the bounded queue, and makes one final
// best-effort flush before infrastructure dependencies are closed.
func (s *RequestPayloadAuditService) Stop() {
	if s == nil || !s.Enabled() {
		return
	}
	s.stopOnce.Do(func() {
		s.queueMu.Lock()
		s.stopped = true
		close(s.stopCh)
		s.queueMu.Unlock()
		s.workerWg.Wait()
	})
}

func (s *RequestPayloadAuditService) run() {
	defer s.workerWg.Done()
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()
	cleanupTicker := time.NewTicker(requestPayloadAuditCleanupInterval)
	defer cleanupTicker.Stop()
	s.cleanupExpired()

	pending := make([]requestPayloadAuditRecord, 0, s.batchSize)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
		result, err := s.persistBatch(ctx, pending)
		cancel()
		if err != nil {
			s.failedBatches.Add(1)
			logger.L().With(
				zap.String("component", "service.request_payload_audit"),
				zap.Int("batch_size", len(pending)),
				zap.Error(err),
			).Warn("request_payload_audit.batch_persist_failed")
			return
		}
		s.applyPersistResult(len(pending), result)
		pending = pending[:0]
	}

	for {
		select {
		case record := <-s.queue:
			pending = append(pending, record)
			if len(pending) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-cleanupTicker.C:
			s.cleanupExpired()
		case <-s.stopCh:
			for {
				select {
				case record := <-s.queue:
					pending = append(pending, record)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *RequestPayloadAuditService) prepareRecord(capture RequestPayloadAuditCapture) (requestPayloadAuditRecord, error) {
	if !s.Enabled() {
		return requestPayloadAuditRecord{}, nil
	}
	clientRequestID := strings.TrimSpace(capture.ClientRequestID)
	if clientRequestID == "" || capture.APIKeyID <= 0 {
		return requestPayloadAuditRecord{}, errors.New("request payload audit capture requires client request ID and API key ID")
	}
	requestBody, responseBody := limitPayloadEntry(capture.RequestBody, capture.ResponseBody, s.maxEntryBytes)
	metadata, err := json.Marshal(capture.Metadata)
	if err != nil {
		return requestPayloadAuditRecord{}, fmt.Errorf("marshal request payload metadata: %w", err)
	}
	return requestPayloadAuditRecord{
		clientRequestID: clientRequestID,
		apiKeyID:        capture.APIKeyID,
		requestBody:     requestBody,
		responseBody:    responseBody,
		metadata:        metadata,
		storedBytes:     int64(len(requestBody) + len(responseBody) + len(metadata)),
	}, nil
}

func (s *RequestPayloadAuditService) persistBatch(ctx context.Context, records []requestPayloadAuditRecord) (requestPayloadAuditPersistResult, error) {
	if len(records) == 0 {
		return requestPayloadAuditPersistResult{}, nil
	}
	records = dedupeRequestPayloadAuditRecords(records)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("begin request payload transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// This row serializes only background batches, including across instances.
	// It replaces the former per-request global advisory lock.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO request_payload_audit_state (singleton, total_bytes)
		VALUES (TRUE, 0)
		ON CONFLICT (singleton) DO NOTHING
	`); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("initialize request payload storage state: %w", err)
	}
	var totalBytes int64
	if err := tx.QueryRowContext(ctx, `
		SELECT total_bytes
		FROM request_payload_audit_state
		WHERE singleton = TRUE
		FOR UPDATE
	`).Scan(&totalBytes); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("lock request payload storage state: %w", err)
	}

	result := requestPayloadAuditPersistResult{}
	expiredRows, expiredBytes, err := pruneExpiredRequestPayloadAuditRecords(ctx, tx, s.effectiveRetentionDays())
	if err != nil {
		return requestPayloadAuditPersistResult{}, err
	}
	totalBytes -= expiredBytes
	if totalBytes < 0 {
		totalBytes = 0
	}
	result.prunedRows += expiredRows
	result.prunedBytes += expiredBytes

	oldBytes, err := loadRequestPayloadAuditRecordBytes(ctx, tx, records)
	if err != nil {
		return requestPayloadAuditPersistResult{}, err
	}
	if err := upsertRequestPayloadAuditRecords(ctx, tx, records); err != nil {
		return requestPayloadAuditPersistResult{}, err
	}
	for _, record := range records {
		totalBytes += record.storedBytes - oldBytes[requestPayloadAuditRecordKey(record)]
	}
	if totalBytes > s.maxStorageBytes {
		prunedRows, prunedBytes, err := pruneRequestPayloadAuditRecords(ctx, tx, totalBytes-s.maxStorageBytes)
		if err != nil {
			return requestPayloadAuditPersistResult{}, err
		}
		result.prunedRows += prunedRows
		result.prunedBytes += prunedBytes
		totalBytes -= prunedBytes
	}
	if s.shouldPrunePlaceholders() {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM request_payload_logs
			WHERE stored_bytes = 0
			  AND updated_at < NOW() - INTERVAL '1 day'
		`); err != nil {
			return requestPayloadAuditPersistResult{}, fmt.Errorf("remove stale request payload placeholders: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE request_payload_audit_state
		SET total_bytes = $1, updated_at = NOW()
		WHERE singleton = TRUE
	`, totalBytes); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("update request payload storage state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("commit request payload transaction: %w", err)
	}
	result.storageBytes = totalBytes
	return result, nil
}

// cleanupExpired removes all expired rows in bounded transactions. It runs
// independently of incoming requests so retention continues during idle
// periods; reads still enforce the TTL if a cleanup cycle is delayed.
func (s *RequestPayloadAuditService) cleanupExpired() {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()

	for {
		result, err := s.pruneExpiredBatch(ctx)
		if err != nil {
			logger.L().With(
				zap.String("component", "service.request_payload_audit"),
				zap.Error(err),
			).Warn("request_payload_audit.expiry_cleanup_failed")
			return
		}
		if result.prunedRows == 0 {
			return
		}
		s.applyPersistResult(0, result)
		if result.prunedRows < requestPayloadAuditPruneBatchSize {
			return
		}
	}
}

func (s *RequestPayloadAuditService) pruneExpiredBatch(ctx context.Context) (requestPayloadAuditPersistResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("begin request payload expiry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO request_payload_audit_state (singleton, total_bytes)
		VALUES (TRUE, 0)
		ON CONFLICT (singleton) DO NOTHING
	`); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("initialize request payload storage state: %w", err)
	}
	var totalBytes int64
	if err := tx.QueryRowContext(ctx, `
		SELECT total_bytes
		FROM request_payload_audit_state
		WHERE singleton = TRUE
		FOR UPDATE
	`).Scan(&totalBytes); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("lock request payload storage state: %w", err)
	}

	prunedRows, prunedBytes, err := pruneExpiredRequestPayloadAuditRecords(ctx, tx, s.effectiveRetentionDays())
	if err != nil {
		return requestPayloadAuditPersistResult{}, err
	}
	if prunedRows == 0 {
		if err := tx.Commit(); err != nil {
			return requestPayloadAuditPersistResult{}, fmt.Errorf("commit empty request payload expiry transaction: %w", err)
		}
		return requestPayloadAuditPersistResult{storageBytes: totalBytes}, nil
	}
	totalBytes -= prunedBytes
	if totalBytes < 0 {
		totalBytes = 0
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE request_payload_audit_state
		SET total_bytes = $1, updated_at = NOW()
		WHERE singleton = TRUE
	`, totalBytes); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("update request payload storage state after expiry cleanup: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return requestPayloadAuditPersistResult{}, fmt.Errorf("commit request payload expiry transaction: %w", err)
	}
	return requestPayloadAuditPersistResult{
		storageBytes: totalBytes,
		prunedRows:   prunedRows,
		prunedBytes:  prunedBytes,
	}, nil
}

func loadRequestPayloadAuditRecordBytes(ctx context.Context, tx *sql.Tx, records []requestPayloadAuditRecord) (map[string]int64, error) {
	var query strings.Builder
	query.WriteString(`
		SELECT client_request_id, api_key_id, stored_bytes
		FROM request_payload_logs
		WHERE (client_request_id, api_key_id) IN (`)
	args := make([]any, 0, len(records)*2)
	for index, record := range records {
		if index > 0 {
			query.WriteString(", ")
		}
		first := index*2 + 1
		fmt.Fprintf(&query, "($%d, $%d)", first, first+1)
		args = append(args, record.clientRequestID, record.apiKeyID)
	}
	query.WriteString(`) FOR UPDATE`)

	rows, err := tx.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("load existing request payload sizes: %w", err)
	}
	defer rows.Close()
	result := make(map[string]int64, len(records))
	for rows.Next() {
		var clientRequestID string
		var apiKeyID, storedBytes int64
		if err := rows.Scan(&clientRequestID, &apiKeyID, &storedBytes); err != nil {
			return nil, fmt.Errorf("scan existing request payload size: %w", err)
		}
		result[requestPayloadAuditRecordKey(requestPayloadAuditRecord{clientRequestID: clientRequestID, apiKeyID: apiKeyID})] = storedBytes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing request payload sizes: %w", err)
	}
	return result, nil
}

func upsertRequestPayloadAuditRecords(ctx context.Context, tx *sql.Tx, records []requestPayloadAuditRecord) error {
	var query strings.Builder
	query.WriteString(`
		INSERT INTO request_payload_logs (
			client_request_id, api_key_id, request_body, response_body, metadata, stored_bytes
		) VALUES `)
	args := make([]any, 0, len(records)*6)
	for index, record := range records {
		if index > 0 {
			query.WriteString(", ")
		}
		first := index*6 + 1
		fmt.Fprintf(&query, "($%d, $%d, $%d, $%d, $%d::jsonb, $%d)", first, first+1, first+2, first+3, first+4, first+5)
		args = append(args, record.clientRequestID, record.apiKeyID, record.requestBody, record.responseBody, record.metadata, record.storedBytes)
	}
	query.WriteString(`
		ON CONFLICT (client_request_id, api_key_id) DO UPDATE SET
			request_body = EXCLUDED.request_body,
			response_body = EXCLUDED.response_body,
			metadata = EXCLUDED.metadata,
			stored_bytes = EXCLUDED.stored_bytes,
			updated_at = NOW()
	`)
	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return fmt.Errorf("upsert request payload batch: %w", err)
	}
	return nil
}

func pruneRequestPayloadAuditRecords(ctx context.Context, tx *sql.Tx, overflowBytes int64) (int64, int64, error) {
	var prunedRows, prunedBytes int64
	for prunedBytes < overflowBytes {
		rows, err := tx.QueryContext(ctx, `
			WITH victims AS (
				SELECT client_request_id, api_key_id
				FROM request_payload_logs
				WHERE stored_bytes > 0
				ORDER BY created_at ASC, client_request_id ASC, api_key_id ASC
				LIMIT $1
			)
			DELETE FROM request_payload_logs AS payload
			USING victims
			WHERE payload.client_request_id = victims.client_request_id
			  AND payload.api_key_id = victims.api_key_id
			RETURNING payload.stored_bytes
		`, requestPayloadAuditPruneBatchSize)
		if err != nil {
			return 0, 0, fmt.Errorf("select request payload retention victims: %w", err)
		}
		var batchRows, batchBytes int64
		for rows.Next() {
			var storedBytes int64
			if err := rows.Scan(&storedBytes); err != nil {
				rows.Close()
				return 0, 0, fmt.Errorf("scan pruned request payload size: %w", err)
			}
			batchRows++
			batchBytes += storedBytes
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("iterate pruned request payload sizes: %w", err)
		}
		if err := rows.Close(); err != nil {
			return 0, 0, fmt.Errorf("close pruned request payload rows: %w", err)
		}
		if batchRows == 0 {
			break
		}
		prunedRows += batchRows
		prunedBytes += batchBytes
	}
	return prunedRows, prunedBytes, nil
}

func pruneExpiredRequestPayloadAuditRecords(ctx context.Context, tx *sql.Tx, retentionDays int) (int64, int64, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH victims AS (
			SELECT client_request_id, api_key_id
			FROM request_payload_logs
			WHERE created_at < NOW() - ($1::int * INTERVAL '1 day')
			ORDER BY created_at ASC, client_request_id ASC, api_key_id ASC
			LIMIT $2
		)
		DELETE FROM request_payload_logs AS payload
		USING victims
		WHERE payload.client_request_id = victims.client_request_id
		  AND payload.api_key_id = victims.api_key_id
		RETURNING payload.stored_bytes
	`, retentionDays, requestPayloadAuditPruneBatchSize)
	if err != nil {
		return 0, 0, fmt.Errorf("select expired request payload records: %w", err)
	}
	defer rows.Close()

	var prunedRows, prunedBytes int64
	for rows.Next() {
		var storedBytes int64
		if err := rows.Scan(&storedBytes); err != nil {
			return 0, 0, fmt.Errorf("scan expired request payload size: %w", err)
		}
		prunedRows++
		prunedBytes += storedBytes
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate expired request payload sizes: %w", err)
	}
	return prunedRows, prunedBytes, nil
}

func dedupeRequestPayloadAuditRecords(records []requestPayloadAuditRecord) []requestPayloadAuditRecord {
	if len(records) < 2 {
		return records
	}
	seen := make(map[string]int, len(records))
	unique := make([]requestPayloadAuditRecord, 0, len(records))
	for _, record := range records {
		key := requestPayloadAuditRecordKey(record)
		if index, ok := seen[key]; ok {
			unique[index] = record
			continue
		}
		seen[key] = len(unique)
		unique = append(unique, record)
	}
	return unique
}

func requestPayloadAuditRecordKey(record requestPayloadAuditRecord) string {
	return fmt.Sprintf("%d:%s", record.apiKeyID, record.clientRequestID)
}

func (s *RequestPayloadAuditService) applyPersistResult(recordCount int, result requestPayloadAuditPersistResult) {
	s.persisted.Add(uint64(recordCount))
	s.prunedRows.Add(uint64(result.prunedRows))
	s.prunedBytes.Add(uint64(result.prunedBytes))
	s.storageBytes.Store(result.storageBytes)
	if result.prunedRows > 0 {
		logger.L().With(
			zap.String("component", "service.request_payload_audit"),
			zap.Int64("pruned_rows", result.prunedRows),
			zap.Int64("pruned_bytes", result.prunedBytes),
			zap.Int64("storage_bytes", result.storageBytes),
		).Info("request_payload_audit.retention_pruned")
	}
}

func (s *RequestPayloadAuditService) shouldPrunePlaceholders() bool {
	now := time.Now().UnixNano()
	last := s.lastCleanup.Load()
	if last != 0 && now-last < requestPayloadAuditPlaceholderPruneGap.Nanoseconds() {
		return false
	}
	return s.lastCleanup.CompareAndSwap(last, now)
}

func (s *RequestPayloadAuditService) effectiveRetentionDays() int {
	if s != nil && s.retentionDays > 0 {
		return s.retentionDays
	}
	return defaultRequestPayloadAuditRetentionDays
}

func (s *RequestPayloadAuditService) logDrop() {
	now := time.Now().UnixNano()
	last := s.lastDropLog.Load()
	if now-last < requestPayloadAuditDropLogInterval.Nanoseconds() || !s.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	stats := s.Stats()
	logger.L().With(
		zap.String("component", "service.request_payload_audit"),
		zap.Int("queue_depth", stats.QueueDepth),
		zap.Int("queue_capacity", stats.QueueCapacity),
		zap.Uint64("dropped", stats.Dropped),
	).Warn("request_payload_audit.queue_full")
}

var ErrRequestPayloadAuditNotFound = errors.New("request payload audit not found")

func limitPayloadEntry(requestBody, responseBody string, maxBytes int64) (string, string) {
	if maxBytes <= 0 {
		return "", ""
	}
	requestLimit, responseLimit := requestPayloadCaptureLimits(maxBytes)
	return trimUTF8ToBytes(requestBody, requestLimit), trimUTF8ToBytes(responseBody, responseLimit)
}

func requestPayloadCaptureLimits(maxBytes int64) (requestLimit, responseLimit int64) {
	if maxBytes <= 0 {
		return 0, 0
	}
	responseLimit = maxBytes / 4
	requestLimit = maxBytes - responseLimit
	return requestLimit, responseLimit
}

func trimUTF8ToBytes(value string, maxBytes int64) string {
	if int64(len(value)) <= maxBytes {
		return value
	}
	end := int(maxBytes)
	for end > 0 && (value[end]&0xc0) == 0x80 {
		end--
	}
	return value[:end]
}
