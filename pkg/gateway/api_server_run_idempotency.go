package gateway

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Run statuses after which a replay record may be pruned
var apiRunTerminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true, "interrupted": true}

const (
	apiRunSelectByKey = "SELECT fingerprint, run_id, status_json, owner_pid, owner_started, updated_at " +
		"FROM run_idempotency WHERE scope=? AND idempotency_key=?"
	apiRunExtendRetentionByKey = "UPDATE run_idempotency SET retention_until=MAX(retention_until, ?) " +
		"WHERE scope=? AND idempotency_key=? AND fingerprint=?"
	apiRunExtendRetentionByRun = "UPDATE run_idempotency SET retention_until=MAX(retention_until, ?) " +
		"WHERE scope=? AND run_id=?"
	// Seconds an unacknowledged replay record is kept after its last update
	RunIdempotencyRetentionSeconds = 24 * 60 * 60
	// Seconds a replay record is kept after its acknowledgement
	RunIdempotencyAcknowledgedRetentionSeconds = 24 * 60 * 60
)

// Columns added after the first schema shipped, applied when missing in this order
var apiRunMigrations = [][2]string{{"owner_pid", "INTEGER NOT NULL DEFAULT 0"}, {"owner_started", "INTEGER NOT NULL DEFAULT 0"},
	{"retention_until", "REAL NOT NULL DEFAULT 0"}, {"acknowledged_at", "REAL"}}

// Logger the extracted store shares with the API server
func apiServerLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.api_server")
}

// Canonical JSON of a public run status
func apiEncodeRunStatus(status any) (string, error) {
	return pyjson.Dumps(status, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
}

// RunIdempotencyRecord is the stored public run status of one reservation with its owner
type RunIdempotencyRecord struct {
	RunID        *string
	Status       any
	OwnerPID     int64
	OwnerStarted int64
	UpdatedAt    float64
}

// Stored record decoded from its columns
func apiRunRecord(runID *string, statusJSON string, ownerPID, ownerStarted sql.NullInt64, updatedAt sql.NullFloat64) (RunIdempotencyRecord, error) {
	status, err := pyjson.Loads(statusJSON)
	if err != nil {
		return RunIdempotencyRecord{}, err
	}
	return RunIdempotencyRecord{RunID: runID, Status: status, OwnerPID: ownerPID.Int64, OwnerStarted: ownerStarted.Int64,
		UpdatedAt: updatedAt.Float64}, nil
}

// One stored reservation row in select order
type apiRunRow struct {
	fingerprint, runID, statusJSON string
	ownerPID, ownerStarted         sql.NullInt64
	updatedAt                      sql.NullFloat64
}

// Reused when the fingerprints match in constant time, else conflict, with the stored record
func apiRunOutcome(row apiRunRow, fingerprint string) (string, *RunIdempotencyRecord, error) {
	outcome := "conflict"
	if hmac.Equal([]byte(row.fingerprint), []byte(fingerprint)) {
		outcome = "reused"
	}
	runID := row.runID
	record, err := apiRunRecord(&runID, row.statusJSON, row.ownerPID, row.ownerStarted, row.updatedAt)
	if err != nil {
		return "", nil, err
	}
	return outcome, &record, nil
}

// RunIdempotencyStore holds durable tenant scoped reservations of run requests by idempotency key
type RunIdempotencyStore struct {
	dbPath *string
	conn   state.SQLiteConn
	mu     sync.Mutex
}

// Whether reservations survive this process
func (s *RunIdempotencyStore) Durable() bool { return s.dbPath != nil }

// Store at the path, the home runs_idempotency.db by default, process memory when the file cannot open
func NewRunIdempotencyStore(ctx context.Context, dbPath string) (*RunIdempotencyStore, error) {
	if dbPath == "" {
		dbPath = filepath.Join(home.Dir(ctx), "runs_idempotency.db")
	}
	s := &RunIdempotencyStore{}
	if dbPath != ":memory:" {
		path := dbPath
		s.dbPath = &path
	}
	conn, err := state.ConnectSQLite(dbPath, state.SQLiteConfig{Timeout: 30})
	if err == nil {
		_, err = conn.ExecContext(ctx, "SELECT 1")
		if err != nil {
			conn.Close()
		}
	}
	if err != nil {
		apiServerLog().Warn("Run idempotency storage is unavailable; falling back to process memory, so replay will not survive a restart: " +
			pyrepr.ExceptionMessage(err))
		if conn, err = state.ConnectSQLite(":memory:", state.SQLiteConfig{Timeout: 5}); err != nil {
			return nil, err
		}
		s.dbPath = nil
	}
	s.conn = conn
	if err := s.initSchema(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	s.tightenPermissions()
	return s, nil
}

// Creates the table, applies the column migrations and the run id index
func (s *RunIdempotencyStore) initSchema(ctx context.Context) error {
	if _, err := state.ApplyWALWithFallback(ctx, s.conn, "runs_idempotency.db", false); err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS run_idempotency (
                scope TEXT NOT NULL,
                idempotency_key TEXT NOT NULL,
                fingerprint TEXT NOT NULL,
                run_id TEXT NOT NULL,
                status_json TEXT NOT NULL,
                owner_pid INTEGER NOT NULL DEFAULT 0,
                owner_started INTEGER NOT NULL DEFAULT 0,
                retention_until REAL NOT NULL DEFAULT 0,
                acknowledged_at REAL,
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL,
                PRIMARY KEY (scope, idempotency_key)
            )`); err != nil {
		return err
	}
	rows, err := s.conn.QueryContext(ctx, "PRAGMA table_info(run_idempotency)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	rows.Close()
	for _, migration := range apiRunMigrations {
		if !columns[migration[0]] {
			if _, err := state.AddColumnIfMissing(ctx, s.conn, "run_idempotency", migration[0], migration[0]+" "+migration[1]); err != nil {
				return err
			}
		}
	}
	_, err = s.conn.ExecContext(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS run_idempotency_run_id ON run_idempotency(run_id)")
	return err
}

// Restricts the database and its WAL side files to the owner
func (s *RunIdempotencyStore) tightenPermissions() {
	if s.dbPath == nil {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		candidate := *s.dbPath + suffix
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			apiServerLog().Debug("Failed to restrict run idempotency store permissions", "error", err)
		}
	}
}

// Runs the body under the store lock inside BEGIN IMMEDIATE, rolling back when it fails
func (s *RunIdempotencyStore) immediateTxn(ctx context.Context, body func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := body(); err != nil {
		if _, rollbackErr := s.conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	return nil
}

// Stored row of a scope and key, nil when absent
func (s *RunIdempotencyStore) selectByKey(ctx context.Context, scope, key string) (*apiRunRow, error) {
	var row apiRunRow
	err := s.conn.QueryRowContext(ctx, apiRunSelectByKey, scope, key).Scan(&row.fingerprint, &row.runID, &row.statusJSON,
		&row.ownerPID, &row.ownerStarted, &row.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// Seconds since the epoch as a float, the time.time the API server modules read
var apiNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Atomically reserves a key, created with the new record or reused or conflict with the stored one
func (s *RunIdempotencyStore) Reserve(ctx context.Context, scope, key, fingerprint, runID string, status any, ownerPID, ownerStarted int64,
	retentionUntil float64) (string, *RunIdempotencyRecord, error) {
	now := apiNow()
	retentionUntil = max(0.0, retentionUntil)
	encoded, err := apiEncodeRunStatus(status)
	if err != nil {
		return "", nil, err
	}
	var outcome string
	var record *RunIdempotencyRecord
	err = s.immediateTxn(ctx, func() error {
		if err := s.pruneStaleTerminalLocked(ctx, now); err != nil {
			return err
		}
		row, err := s.selectByKey(ctx, scope, key)
		if err != nil {
			return err
		}
		if row != nil {
			if retentionUntil != 0 {
				if _, err := s.conn.ExecContext(ctx, apiRunExtendRetentionByKey, retentionUntil, scope, key, fingerprint); err != nil {
					return err
				}
			}
			if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			outcome, record, err = apiRunOutcome(*row, fingerprint)
			return err
		}
		if _, err := s.conn.ExecContext(ctx, "INSERT INTO run_idempotency("+
			"scope,idempotency_key,fingerprint,run_id,status_json,"+
			"owner_pid,owner_started,retention_until,created_at,updated_at"+
			") VALUES(?,?,?,?,?,?,?,?,?,?)", scope, key, fingerprint, runID, encoded, ownerPID, ownerStarted,
			retentionUntil, now, now); err != nil {
			return err
		}
		if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		id := runID
		outcome, record = "created", &RunIdempotencyRecord{RunID: &id, Status: status, OwnerPID: ownerPID, OwnerStarted: ownerStarted, UpdatedAt: now}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return outcome, record, nil
}

// Missing, reused or conflict for a key without reserving it
func (s *RunIdempotencyStore) Lookup(ctx context.Context, scope, key, fingerprint string, retentionUntil float64) (string, *RunIdempotencyRecord, error) {
	now := apiNow()
	retentionUntil = max(0.0, retentionUntil)
	var row *apiRunRow
	err := s.immediateTxn(ctx, func() error {
		if retentionUntil != 0 {
			if _, err := s.conn.ExecContext(ctx, apiRunExtendRetentionByKey, retentionUntil, scope, key, fingerprint); err != nil {
				return err
			}
		}
		if err := s.pruneStaleTerminalLocked(ctx, now); err != nil {
			return err
		}
		var err error
		if row, err = s.selectByKey(ctx, scope, key); err != nil {
			return err
		}
		_, err = s.conn.ExecContext(ctx, "COMMIT")
		return err
	})
	if err != nil {
		return "", nil, err
	}
	if row == nil {
		return "missing", nil, nil
	}
	return apiRunOutcome(*row, fingerprint)
}

// Deletes aged replay records whose stored run is terminal, the caller holding the lock and transaction
func (s *RunIdempotencyStore) pruneStaleTerminalLocked(ctx context.Context, now float64) error {
	rows, err := s.conn.QueryContext(ctx, `SELECT scope, idempotency_key, status_json
                 FROM run_idempotency
                WHERE acknowledged_at <= ?
                   OR (retention_until > 0 AND retention_until <= ?)
                   OR (retention_until <= 0 AND updated_at < ?)`,
		now-RunIdempotencyAcknowledgedRetentionSeconds, now, now-RunIdempotencyRetentionSeconds)
	if err != nil {
		return err
	}
	var stale [][2]string
	for rows.Next() {
		var scope, key, statusJSON string
		if err := rows.Scan(&scope, &key, &statusJSON); err != nil {
			rows.Close()
			return err
		}
		terminal := false
		if decoded, err := pyjson.Loads(statusJSON); err == nil {
			if obj, ok := decoded.(*pyjson.Object); ok {
				text, isText := obj.GetOr("status", nil).(string)
				terminal = isText && apiRunTerminalStatuses[text]
			}
		}
		if terminal {
			stale = append(stale, [2]string{scope, key})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, entry := range stale {
		if _, err := s.conn.ExecContext(ctx, "DELETE FROM run_idempotency WHERE scope=? AND idempotency_key=?", entry[0], entry[1]); err != nil {
			return err
		}
	}
	return nil
}

// Durable run status inside its authenticated scope, nil when the scope holds no such run
func (s *RunIdempotencyStore) StatusForRun(ctx context.Context, scope, runID string, retentionUntil float64) (*RunIdempotencyRecord, error) {
	retentionUntil = max(0.0, retentionUntil)
	s.mu.Lock()
	if retentionUntil != 0 {
		if _, err := s.conn.ExecContext(ctx, apiRunExtendRetentionByRun, retentionUntil, scope, runID); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	var statusJSON string
	var ownerPID, ownerStarted sql.NullInt64
	var updatedAt sql.NullFloat64
	err := s.conn.QueryRowContext(ctx, "SELECT status_json, owner_pid, owner_started, updated_at "+
		"FROM run_idempotency WHERE scope=? AND run_id=?", scope, runID).Scan(&statusJSON, &ownerPID, &ownerStarted, &updatedAt)
	s.mu.Unlock()
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record, err := apiRunRecord(nil, statusJSON, ownerPID, ownerStarted, updatedAt)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// Persists the latest verified recovery horizon of a run, true when exactly one row changed
func (s *RunIdempotencyStore) ExtendRetention(ctx context.Context, scope, runID string, until float64) (bool, error) {
	checked := max(0.0, until)
	if checked == 0 {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.conn.ExecContext(ctx, apiRunExtendRetentionByRun, checked, scope, runID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// Whether the scope holds the run
func (s *RunIdempotencyStore) OwnsRun(ctx context.Context, scope, runID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var one int
	err := s.conn.QueryRowContext(ctx, "SELECT 1 FROM run_idempotency WHERE scope=? AND run_id=?", scope, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Replaces the stored public status of a run
func (s *RunIdempotencyStore) UpdateStatus(ctx context.Context, runID string, status any) error {
	encoded, err := apiEncodeRunStatus(status)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.conn.ExecContext(ctx, "UPDATE run_idempotency SET status_json=?, updated_at=? WHERE run_id=?", encoded, apiNow(), runID)
	return err
}

// Releases a reservation whose run was refused before it existed
func (s *RunIdempotencyStore) Forget(ctx context.Context, scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(ctx, "DELETE FROM run_idempotency WHERE scope=? AND idempotency_key=?", scope, key)
	return err
}

// Closes the connection
func (s *RunIdempotencyStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Close()
}
