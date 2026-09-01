package service

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestLimitPayloadEntryPreservesUTF8Boundaries(t *testing.T) {
	request, response := limitPayloadEntry("a界tail", "b界tail", 6)

	if request != "a界t" || response != "b" {
		t.Fatalf("limitPayloadEntry() = (%q, %q), want (\"a界t\", \"b\")", request, response)
	}
}

func TestLimitPayloadEntryPrioritizesInputOverOutput(t *testing.T) {
	request, response := limitPayloadEntry("12345", "abcde", 8)

	if request != "12345" || response != "ab" {
		t.Fatalf("limitPayloadEntry() = (%q, %q), want (\"12345\", \"ab\")", request, response)
	}
}

func TestDedupeRequestPayloadAuditRecordsKeepsLatestRecord(t *testing.T) {
	records := dedupeRequestPayloadAuditRecords([]requestPayloadAuditRecord{
		{clientRequestID: "request-a", apiKeyID: 1, requestBody: "old", storedBytes: 3},
		{clientRequestID: "request-b", apiKeyID: 1, requestBody: "other", storedBytes: 5},
		{clientRequestID: "request-a", apiKeyID: 1, requestBody: "new", storedBytes: 3},
	})

	if len(records) != 2 {
		t.Fatalf("dedupeRequestPayloadAuditRecords() length = %d, want 2", len(records))
	}
	if records[0].requestBody != "new" {
		t.Fatalf("latest duplicate = %q, want new", records[0].requestBody)
	}
}

func TestEnqueueCaptureIsNonBlockingAndTracksQueueOverflow(t *testing.T) {
	svc := &RequestPayloadAuditService{
		db:            &sql.DB{},
		enabled:       true,
		maxEntryBytes: 16,
		queue:         make(chan requestPayloadAuditRecord, 1),
		batchSize:     1,
		flushInterval: defaultRequestPayloadAuditFlushInterval,
		writeTimeout:  defaultRequestPayloadAuditWriteTimeout,
	}
	capture := RequestPayloadAuditCapture{
		ClientRequestID: "request-a",
		APIKeyID:        7,
		RequestBody:     "hello",
		ResponseBody:    "world",
		Metadata:        map[string]any{"status_code": 200},
	}

	if !svc.EnqueueCapture(capture) {
		t.Fatal("first EnqueueCapture() = false, want true")
	}
	if svc.EnqueueCapture(capture) {
		t.Fatal("second EnqueueCapture() = true, want false when queue is full")
	}

	stats := svc.Stats()
	if stats.Enqueued != 1 || stats.Dropped != 1 || stats.QueueDepth != 1 || stats.QueueCapacity != 1 {
		t.Fatalf("Stats() = %+v, want one enqueued record and one dropped record", stats)
	}
}

func TestPersistBatchUpdatesStorageCounterWithoutRetentionScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := &RequestPayloadAuditService{
		db:              db,
		enabled:         true,
		maxStorageBytes: 1024,
	}
	record := requestPayloadAuditRecord{
		clientRequestID: "request-a",
		apiKeyID:        7,
		requestBody:     "input",
		responseBody:    "output",
		metadata:        []byte(`{"status_code":200}`),
		storedBytes:     20,
	}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO request_payload_audit_state").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT total_bytes").WillReturnRows(sqlmock.NewRows([]string{"total_bytes"}).AddRow(100))
	mock.ExpectQuery("DELETE FROM request_payload_logs").
		WithArgs(defaultRequestPayloadAuditRetentionDays, requestPayloadAuditPruneBatchSize).
		WillReturnRows(sqlmock.NewRows([]string{"stored_bytes"}))
	mock.ExpectQuery("SELECT client_request_id, api_key_id, stored_bytes").
		WithArgs("request-a", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"client_request_id", "api_key_id", "stored_bytes"}).AddRow("request-a", 7, 12))
	mock.ExpectExec("INSERT INTO request_payload_logs").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("DELETE FROM request_payload_logs").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("UPDATE request_payload_audit_state").WithArgs(int64(108)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	result, err := svc.persistBatch(t.Context(), []requestPayloadAuditRecord{record})
	if err != nil {
		t.Fatalf("persistBatch() error = %v", err)
	}
	if result.storageBytes != 108 {
		t.Fatalf("storageBytes = %d, want 108", result.storageBytes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
