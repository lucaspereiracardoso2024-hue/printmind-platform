package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	MaxQueueRows  = 1000
	MaxQueueBytes = 50 * 1024 * 1024 // 50 MB
)

// QueuedTelemetry represents a persisted telemetry snapshot waiting for cloud delivery.
type QueuedTelemetry struct {
	ID           int64
	CreatedAt    string
	PayloadJSON  string
	PayloadBytes int
	RetryCount   int
	NextRetryAt  int64
}

// SQLiteStore manages durable WAL-backed offline buffering, job checkpoints, and supply history.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLiteStore opens or creates the SQLite WAL database at dbPath and initializes the schema.
func OpenSQLiteStore(dbPath string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", filepath.ToSlash(dbPath))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	schema := `
	PRAGMA journal_mode=WAL;
	PRAGMA busy_timeout=5000;
	PRAGMA synchronous=NORMAL;

	CREATE TABLE IF NOT EXISTS telemetry_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		payload_bytes INTEGER NOT NULL,
		retry_count INTEGER NOT NULL DEFAULT 0,
		next_retry_at INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS job_checkpoints (
		source TEXT PRIMARY KEY,
		last_record_id INTEGER NOT NULL DEFAULT 0,
		last_timestamp TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS supply_history (
		device_key TEXT NOT NULL,
		supply_name TEXT NOT NULL,
		serial_number TEXT NOT NULL DEFAULT '',
		last_percent INTEGER,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (device_key, supply_name)
	);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

// Close closes the SQLite connection.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// EnqueueTelemetry saves a SyncRequest to telemetry_queue while enforcing the 1,000 rows / 50 MB cap.
func (s *SQLiteStore) EnqueueTelemetry(req SyncRequest) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sqlite store not open")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return s.enqueueRaw(string(raw))
}

func (s *SQLiteStore) enqueueRaw(payloadJSON string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	size := len(payloadJSON)
	if _, err := s.db.Exec(
		`INSERT INTO telemetry_queue (created_at, payload_json, payload_bytes, retry_count, next_retry_at) VALUES (?, ?, ?, 0, 0)`,
		now, payloadJSON, size,
	); err != nil {
		return err
	}
	return s.enforceQueueCaps()
}

func (s *SQLiteStore) enforceQueueCaps() error {
	for {
		var count int
		var totalBytes sql.NullInt64
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0) FROM telemetry_queue`).Scan(&count, &totalBytes); err != nil {
			return err
		}
		if count <= MaxQueueRows && totalBytes.Int64 <= MaxQueueBytes {
			return nil
		}
		if _, err := s.db.Exec(`DELETE FROM telemetry_queue WHERE id = (SELECT id FROM telemetry_queue ORDER BY id ASC LIMIT 1)`); err != nil {
			return err
		}
	}
}

// QueueCount returns the number of pending telemetry snapshots in the offline queue.
func (s *SQLiteStore) QueueCount() (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM telemetry_queue`).Scan(&count)
	return count, err
}

// MigrateLegacyBuffer imports an old agent_buffer.json file into telemetry_queue if present.
func (s *SQLiteStore) MigrateLegacyBuffer(legacyPath string) error {
	if s == nil || s.db == nil {
		return nil
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil || len(data) == 0 {
		return nil
	}
	var rpc SupabaseRPCPayload
	var req SyncRequest
	if json.Unmarshal(data, &rpc) == nil && rpc.Payload.CollectedAt != "" {
		req = rpc.Payload
	} else if json.Unmarshal(data, &req) != nil {
		_ = os.Remove(legacyPath)
		return nil
	}
	if err := s.EnqueueTelemetry(req); err != nil {
		return err
	}
	_ = os.Remove(legacyPath)
	return nil
}

// ComputeBackoffDuration returns exponential backoff with jitter capped at 300 seconds.
func ComputeBackoffDuration(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	if retryCount > 6 {
		retryCount = 6
	}
	baseSec := 5 << retryCount // 5, 10, 20, 40, 80, 160, 320
	if baseSec > 300 {
		baseSec = 300
	}
	jitterMs := rand.Intn(1500)
	return time.Duration(baseSec)*time.Second + time.Duration(jitterMs)*time.Millisecond
}

// DrainQueue sends queued telemetry snapshots chronologically using the provided sender function.
func (s *SQLiteStore) DrainQueue(ctx context.Context, nowUnix int64, sender func(SyncRequest) error) (drained int, err error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, payload_json, payload_bytes, retry_count, next_retry_at
		 FROM telemetry_queue
		 WHERE next_retry_at <= ?
		 ORDER BY id ASC
		 LIMIT 50`,
		nowUnix,
	)
	if err != nil {
		return 0, err
	}
	var batch []QueuedTelemetry
	for rows.Next() {
		var q QueuedTelemetry
		if scanErr := rows.Scan(&q.ID, &q.CreatedAt, &q.PayloadJSON, &q.PayloadBytes, &q.RetryCount, &q.NextRetryAt); scanErr == nil {
			batch = append(batch, q)
		}
	}
	_ = rows.Close()

	for _, item := range batch {
		if ctx.Err() != nil {
			return drained, ctx.Err()
		}
		var req SyncRequest
		if uErr := json.Unmarshal([]byte(item.PayloadJSON), &req); uErr != nil {
			_, _ = s.db.Exec(`DELETE FROM telemetry_queue WHERE id = ?`, item.ID)
			continue
		}
		if sendErr := sender(req); sendErr != nil {
			nextRetry := nowUnix + int64(ComputeBackoffDuration(item.RetryCount).Seconds())
			_, _ = s.db.Exec(
				`UPDATE telemetry_queue SET retry_count = retry_count + 1, next_retry_at = ? WHERE id = ?`,
				nextRetry, item.ID,
			)
			return drained, sendErr
		}
		_, _ = s.db.Exec(`DELETE FROM telemetry_queue WHERE id = ?`, item.ID)
		drained++
	}
	return drained, nil
}

// GetCheckpoint retrieves the last processed record ID and timestamp for a job audit source.
func (s *SQLiteStore) GetCheckpoint(source string) (int64, string, error) {
	if s == nil || s.db == nil {
		return 0, "", nil
	}
	var recID int64
	var ts string
	err := s.db.QueryRow(`SELECT last_record_id, last_timestamp FROM job_checkpoints WHERE source = ?`, source).Scan(&recID, &ts)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return recID, ts, err
}

// SaveCheckpoint persists the last processed record ID and timestamp for a job audit source.
func (s *SQLiteStore) SaveCheckpoint(source string, lastRecordID int64, lastTimestamp string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO job_checkpoints (source, last_record_id, last_timestamp)
		 VALUES (?, ?, ?)
		 ON CONFLICT(source) DO UPDATE SET last_record_id = excluded.last_record_id, last_timestamp = excluded.last_timestamp`,
		source, lastRecordID, lastTimestamp,
	)
	return err
}

// RecordSupplyHistory stores latest serial and percentage for premature-swap/drain analytics.
func (s *SQLiteStore) RecordSupplyHistory(devices []DiscoveredDevice) {
	if s == nil || s.db == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, dev := range devices {
		key := dev.SerialNumber
		if key == "" {
			key = dev.IPAddress + "|" + dev.Model
		}
		for _, sup := range dev.Supplies {
			var pct sql.NullInt64
			if sup.Percent != nil {
				pct = sql.NullInt64{Int64: int64(*sup.Percent), Valid: true}
			}
			_, _ = s.db.Exec(
				`INSERT INTO supply_history (device_key, supply_name, serial_number, last_percent, updated_at)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(device_key, supply_name) DO UPDATE SET
				   serial_number = CASE WHEN excluded.serial_number <> '' THEN excluded.serial_number ELSE supply_history.serial_number END,
				   last_percent = COALESCE(excluded.last_percent, supply_history.last_percent),
				   updated_at = excluded.updated_at`,
				key, sup.Name, sup.SerialNumber, pct, now,
			)
		}
	}
}
