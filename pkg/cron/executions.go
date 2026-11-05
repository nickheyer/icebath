package cron

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// MaxTerminalExecutions bounds the terminal rows the ledger keeps
	MaxTerminalExecutions = 1000
	// HandoffAdoptionGraceSeconds is how long a pending worker handoff is fenced from recovery
	HandoffAdoptionGraceSeconds = 30.0
	// LiveOwnerStaleClaimFloorSeconds floors the stale bound for a claim whose owner is alive
	LiveOwnerStaleClaimFloorSeconds = 7200.0
	executionsStatusCheck           = "('claimed','running','completed','failed','unknown','cancelled','missed')"
	startTimeDriftTolerance         = 200
	ownerGoneReason                 = "Scheduler restarted after this execution's owner exited before a durable " +
		"terminal state; whether side effects ran is unknown."
	ownerWedgedReason = "Owner process is still alive but the claim outlived the derived stale bound; " +
		"treated as wedged (#115692). The process was not terminated; whether side effects " +
		"ran is unknown."
)

// FirerFinishedStates are outcomes the attempt's own firer booked, final for every recovery path
var FirerFinishedStates = map[string]bool{"completed": true, "failed": true, "cancelled": true}

var (
	ledgerMu           sync.Mutex
	executionProcessID = newExecutionID()
	ledgerWallTime     = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	ledgerPidExists    = proc.Exists
	ledgerStartTime    = processStartTime
)

// Ledger is the per home executions database recording every claimed attempt
type Ledger struct {
	s    *Store
	path string
}

// Ledger of the store home, the store executions file override when set
func openExecutionsLedger(s *Store) *Ledger {
	path := s.executionsFile
	if path == "" {
		path = filepath.Join(s.cronDir, "executions.db")
	}
	return &Ledger{s: s, path: path}
}

// Random 32 hex digit identifier like uuid4().hex
func newExecutionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// Start time fingerprint of pid from procfs ticks or the process birth in centiseconds
func processStartTime(pid int) (int64, bool) {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if end := bytes.LastIndexByte(data, ')'); end >= 0 {
			if fields := bytes.Fields(data[end+1:]); len(fields) > 19 {
				if n, err := strconv.ParseInt(string(fields[19]), 10, 64); err == nil {
					return n, true
				}
			}
		}
	}
	birth, err := tools.ProcessBirth(pid)
	if err != nil {
		return 0, false
	}
	return int64(math.RoundToEven(birth * 100)), true
}

// Owner pid is alive and the same incarnation, true whenever death cannot be proven
func ownerIsLive(pid int64, startedAt any) (bool, error) {
	if !ledgerPidExists(int(pid)) {
		return false, nil
	}
	if startedAt == nil {
		return int(pid) == os.Getpid(), nil
	}
	current, ok := ledgerStartTime(int(pid))
	if !ok {
		return true, nil
	}
	recorded, err := pyint.Of(startedAt)
	if err != nil {
		return false, err
	}
	if !recorded.IsInt64() {
		return false, nil
	}
	diff := current - recorded.Int64()
	if diff < 0 {
		diff = -diff
	}
	return diff <= startTimeDriftTolerance, nil
}

// Wedge bound for a live owner from the inactivity and script timeouts, false when inactivity is unlimited
func (s *Store) liveOwnerStaleAfterSeconds(ctx context.Context) (float64, bool) {
	inactivity := s.cronInactivitySeconds(ctx)
	if math.IsInf(inactivity, 0) || math.IsNaN(inactivity) || inactivity <= 0 {
		return 0, false
	}
	return math.Max(math.Max(inactivity*ClaimTTLInactivityHeadroom, float64(s.getScriptTimeout(ctx))), LiveOwnerStaleClaimFloorSeconds), true
}

// Seconds since the owner last proved progress, the claim age for rows without a stamp
func (s *Store) staleAgeSeconds(claimedAt string, progressAt any) (float64, error) {
	stamp := claimedAt
	if p, ok := progressAt.(string); ok && p != "" {
		stamp = p
	}
	at, err := ParseISO(stamp)
	if err != nil {
		return 0, err
	}
	return elapsedSeconds(s.clk.now(), at.Time), nil
}

// Opens the ledger database with the schema initialised and the cron dir secured
func (l *Ledger) connect(ctx context.Context) (state.SQLiteConn, error) {
	if err := ensureCronDir(filepath.Dir(l.path)); err != nil {
		return nil, err
	}
	return state.OpenDB(ctx, l.path, "cron/executions.db", state.WithSynchronousFull(), state.WithInitialize(initializeExecutionsSchema))
}

// Runs body in one ledger transaction under the process wide ledger lock
func (l *Ledger) transaction(ctx context.Context, body func(conn state.SQLiteConn) error) error {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	conn, err := l.connect(ctx)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, false, body)
}

// Creates the executions table, its widened status check, late columns and indexes
func initializeExecutionsSchema(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS executions (
             id TEXT PRIMARY KEY,
             job_id TEXT NOT NULL,
             source TEXT NOT NULL,
             process_id TEXT NOT NULL,
             pid INTEGER NOT NULL,
             process_started_at INTEGER,
             status TEXT NOT NULL CHECK(status IN
               `+executionsStatusCheck+`),
             handoff_pending INTEGER NOT NULL DEFAULT 0,
             handoff_started_at REAL,
             claimed_at TEXT NOT NULL,
             started_at TEXT,
             finished_at TEXT,
             error TEXT
           )`); err != nil {
		return err
	}
	if err := widenStatusCheck(ctx, conn); err != nil {
		return err
	}
	steps := []func() error{
		func() error {
			_, err := state.AddColumnIfMissing(ctx, conn, "executions", "handoff_pending", "handoff_pending INTEGER NOT NULL DEFAULT 0")
			return err
		},
		func() error {
			_, err := state.AddColumnIfMissing(ctx, conn, "executions", "handoff_started_at", "handoff_started_at REAL")
			return err
		},
		func() error {
			_, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_executions_job_claimed ON executions(job_id, claimed_at DESC, id DESC)")
			return err
		},
		func() error {
			_, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_executions_status_claimed ON executions(status, claimed_at DESC, id DESC)")
			return err
		},
		func() error {
			_, err := state.AddColumnIfMissing(ctx, conn, "executions", "delivery_outcome", "delivery_outcome TEXT")
			return err
		},
		func() error {
			_, err := state.AddColumnIfMissing(ctx, conn, "executions", "scheduled_instant", "scheduled_instant TEXT")
			return err
		},
		func() error {
			_, err := state.AddColumnIfMissing(ctx, conn, "executions", "progress_at", "progress_at TEXT")
			return err
		},
		func() error {
			_, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_executions_occurrence ON executions(job_id, scheduled_instant) WHERE status='completed'")
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// Ends an open transaction, a no op when none is open like sqlite3 commit and rollback
func endLedgerTransaction(ctx context.Context, conn state.SQLiteConn, verb string) error {
	_, err := conn.ExecContext(ctx, verb)
	if err != nil && strings.Contains(err.Error(), "no transaction is active") {
		return nil
	}
	return err
}

// Rebuilds a ledger created before cancelled and missed existed, once per file under the write lock
func widenStatusCheck(ctx context.Context, conn state.SQLiteConn) error {
	currentSQL := func() (string, error) {
		var text sql.NullString
		err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='executions'").Scan(&text)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !text.Valid) {
			return "'missed'", nil
		}
		return text.String, err
	}
	current, err := currentSQL()
	if err != nil || strings.Contains(current, "'missed'") {
		return err
	}
	if err := endLedgerTransaction(ctx, conn, "COMMIT"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rebuild := func() error {
		oldSQL, err := currentSQL()
		if err != nil || strings.Contains(oldSQL, "'missed'") {
			return err
		}
		rows, err := conn.QueryContext(ctx, "PRAGMA table_info(executions)")
		if err != nil {
			return err
		}
		infos, err := scanRecords(rows)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(infos))
		for _, info := range infos {
			names = append(names, fmt.Sprint(info["name"]))
		}
		columns := strings.Join(names, ", ")
		for _, stmt := range []string{
			"ALTER TABLE executions RENAME TO executions_pre_widen",
			strings.Replace(oldSQL, "'unknown')", "'unknown','cancelled','missed')", 1),
			"INSERT INTO executions (" + columns + ") SELECT " + columns + " FROM executions_pre_widen",
			"DROP TABLE executions_pre_widen",
		} {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}
	if err := rebuild(); err != nil {
		endLedgerTransaction(ctx, conn, "ROLLBACK")
		return err
	}
	if err := endLedgerTransaction(ctx, conn, "COMMIT"); err != nil {
		endLedgerTransaction(ctx, conn, "ROLLBACK")
		return err
	}
	return nil
}

// Every row as a column keyed record with SQLite storage class values
func scanRecords(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			rec[c] = vals[i]
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// First record of a query, nil when it returns no rows
func queryRecord(ctx context.Context, conn state.SQLiteConn, query string, args ...any) (map[string]any, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	recs, err := scanRecords(rows)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0], nil
}

// One execution row by id, nil when absent
func fetchExecution(ctx context.Context, conn state.SQLiteConn, id string) (map[string]any, error) {
	return queryRecord(ctx, conn, "SELECT * FROM executions WHERE id=?", id)
}

// Rows an UPDATE changed
func rowsChanged(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Projects a ledger record to monitoring, never letting monitoring break the ledger
func emitExecutionStateSafe(record map[string]any, deliveryOutcome *string) {
	defer func() { recover() }()
	emitExecutionState(record, deliveryOutcome)
}

// Text column value, empty for NULL
func recText(rec map[string]any, key string) string {
	v, _ := rec[key].(string)
	return v
}

// Integer column value, zero for NULL
func recInt(rec map[string]any, key string) int64 {
	switch v := rec[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// Optional text as a column argument
func nullable(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Stamps progress on a running attempt this process owns, false once it is no longer ours
func (l *Ledger) TouchProgress(ctx context.Context, executionID string) (bool, error) {
	now := l.s.clk.nowISO()
	var n int64
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		var err error
		n, err = rowsChanged(conn.ExecContext(ctx, `UPDATE executions SET progress_at=?
               WHERE id=? AND status IN ('claimed','running') AND process_id=? AND pid=?`,
			now, executionID, executionProcessID, os.Getpid()))
		return err
	})
	return n == 1, err
}

// Drops terminal rows beyond the newest MaxTerminalExecutions
func pruneExecutions(ctx context.Context, conn state.SQLiteConn) error {
	_, err := conn.ExecContext(ctx, `DELETE FROM executions WHERE id IN (
             SELECT id FROM executions
             WHERE status IN ('completed','failed','unknown','cancelled','missed')
             ORDER BY julianday(finished_at) DESC, finished_at DESC,
                      julianday(claimed_at) DESC, claimed_at DESC, id DESC LIMIT -1 OFFSET ?
           )`, max(0, MaxTerminalExecutions))
	return err
}

// Persists a claimed attempt before executor or provider dispatch
func (l *Ledger) Create(ctx context.Context, jobID, source string, scheduled *string) (map[string]any, error) {
	now := l.s.clk.nowISO()
	id := newExecutionID()
	pid := os.Getpid()
	var startedAt any
	if st, ok := ledgerStartTime(pid); ok {
		startedAt = st
	}
	instant := nullable(scheduledInstant(l.s, scheduled))
	var record map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO executions
               (id, job_id, source, process_id, pid, process_started_at,
                status, claimed_at, scheduled_instant)
               VALUES (?, ?, ?, ?, ?, ?, 'claimed', ?, ?)`,
			id, jobID, source, executionProcessID, pid, startedAt, now, instant); err != nil {
			return err
		}
		var err error
		record, err = fetchExecution(ctx, conn, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	emitExecutionStateSafe(record, nil)
	return record, nil
}

// Binds the store claimed occurrence before a provider hands the attempt to a worker
func (l *Ledger) SetOccurrence(ctx context.Context, executionID string, instant *string) error {
	canonical := nullable(scheduledInstant(l.s, instant))
	return l.transaction(ctx, func(conn state.SQLiteConn) error {
		n, err := rowsChanged(conn.ExecContext(ctx, "UPDATE executions SET scheduled_instant=? WHERE id=? AND status='claimed' "+
			"AND handoff_pending=0 AND process_id=? AND pid=?", canonical, executionID, executionProcessID, os.Getpid()))
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("Cron occurrence could not be bound before dispatch")
		}
		return nil
	})
}

// Runs one guarded UPDATE and returns the row after it, nil when the guard matched nothing
func (l *Ledger) updateAndFetch(ctx context.Context, executionID string, prune bool, query string, args ...any) (map[string]any, error) {
	var record map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		n, err := rowsChanged(conn.ExecContext(ctx, query, args...))
		if err != nil || n != 1 {
			return err
		}
		if prune {
			if err := pruneExecutions(ctx, conn); err != nil {
				return err
			}
		}
		record, err = fetchExecution(ctx, conn, executionID)
		return err
	})
	return record, err
}

// Fences restart recovery while an external worker adopts the claim
func (l *Ledger) MarkHandoffPending(ctx context.Context, executionID string) (map[string]any, error) {
	record, err := l.updateAndFetch(ctx, executionID, false, `UPDATE executions
               SET handoff_pending=1, handoff_started_at=?
               WHERE id=? AND status='claimed'
                 AND process_id=? AND pid=?`, ledgerWallTime(), executionID, executionProcessID, os.Getpid())
	if err != nil || record == nil {
		return nil, err
	}
	emitExecutionStateSafe(record, nil)
	return record, nil
}

// Transfers a pending handoff to this worker process and starts it, nil for every loser
func (l *Ledger) AdoptClaimed(ctx context.Context, executionID string) (map[string]any, error) {
	pid := os.Getpid()
	var startedAt any
	if st, ok := ledgerStartTime(pid); ok {
		startedAt = st
	}
	record, err := l.updateAndFetch(ctx, executionID, false, `UPDATE executions
               SET process_id=?, pid=?, process_started_at=?,
                   status='running', started_at=?, handoff_pending=0,
                   handoff_started_at=NULL
               WHERE id=? AND status='claimed' AND handoff_pending=1`,
		executionProcessID, pid, startedAt, l.s.clk.nowISO(), executionID)
	if err != nil || record == nil {
		return nil, err
	}
	emitExecutionStateSafe(record, nil)
	return record, nil
}

// Moves one claimed attempt of this process to running exactly once
func (l *Ledger) MarkRunning(ctx context.Context, executionID string) (map[string]any, error) {
	record, err := l.updateAndFetch(ctx, executionID, false, `UPDATE executions
               SET status='running', started_at=?, handoff_pending=0,
                   handoff_started_at=NULL
               WHERE id=? AND status='claimed' AND handoff_pending=0
                 AND process_id=? AND pid=?`, l.s.clk.nowISO(), executionID, executionProcessID, os.Getpid())
	if err != nil || record == nil {
		return nil, err
	}
	emitExecutionStateSafe(record, nil)
	return record, nil
}

// Failure detail stored for an unsuccessful attempt
func failureDetail(success bool, errText *string) any {
	if success {
		return nil
	}
	if errText != nil && *errText != "" {
		return *errText
	}
	return "unknown failure"
}

// Writes the terminal result of an attempt this process owns, nil when it is not ours or already terminal
func (l *Ledger) Finish(ctx context.Context, executionID string, success bool, errText, deliveryOutcome *string, cancelled bool) (map[string]any, error) {
	status := "failed"
	switch {
	case cancelled:
		status = "cancelled"
	case success:
		status = "completed"
	}
	record, err := l.updateAndFetch(ctx, executionID, true, `UPDATE executions
               SET status=?, finished_at=?, error=?, handoff_pending=0,
                   handoff_started_at=NULL, delivery_outcome=?
               WHERE id=? AND status IN ('claimed','running')
                 AND process_id=? AND pid=?`,
		status, l.s.clk.nowISO(), failureDetail(success, errText), nullable(deliveryOutcome), executionID, executionProcessID, os.Getpid())
	if err != nil || record == nil {
		return nil, err
	}
	emitExecutionStateSafe(record, deliveryOutcome)
	recordCronFinish(record, deliveryOutcome)
	return record, nil
}

// Books skipped occurrences as one terminal missed row naming the count and the earliest slot
func (l *Ledger) RecordMissed(ctx context.Context, jobID string, count int, firstInstant *string, detail string) (map[string]any, error) {
	if count < 1 {
		return nil, nil
	}
	now := l.s.clk.nowISO()
	id := newExecutionID()
	pid := os.Getpid()
	var startedAt any
	if st, ok := ledgerStartTime(pid); ok {
		startedAt = st
	}
	instant := nullable(scheduledInstant(l.s, firstInstant))
	var record map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO executions
               (id, job_id, source, process_id, pid, process_started_at,
                status, claimed_at, finished_at, error, scheduled_instant)
               VALUES (?, ?, 'scheduler', ?, ?, ?, 'missed', ?, ?, ?, ?)`,
			id, jobID, executionProcessID, pid, startedAt, now, now,
			fmt.Sprintf("missed ×%d (%s)", count, detail), instant); err != nil {
			return err
		}
		if err := pruneExecutions(ctx, conn); err != nil {
			return err
		}
		var err error
		record, err = fetchExecution(ctx, conn, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	emitExecutionStateSafe(record, nil)
	return record, nil
}

// Non terminal or unknown row of the job's attempt with its owner columns
func recoverableRow(ctx context.Context, conn state.SQLiteConn, executionID, jobID string) (map[string]any, error) {
	return queryRecord(ctx, conn, `SELECT status, process_id, pid, process_started_at FROM executions
           WHERE id=? AND job_id=? AND status IN ('claimed','running','unknown')`, executionID, jobID)
}

// Row's firer can still finish it, this process for a non unknown row or a live foreign owner
func rowOwnerLive(row map[string]any) (bool, error) {
	if recText(row, "process_id") == executionProcessID {
		return recText(row, "status") != "unknown", nil
	}
	return ownerIsLive(recInt(row, "pid"), row["process_started_at"])
}

// Settles a verified receipt after owner loss, nil when terminal or its firer is still live
func (l *Ledger) RecoverReceipted(ctx context.Context, executionID, jobID string, success bool, errText, deliveryOutcome *string) (map[string]any, error) {
	now := l.s.clk.nowISO()
	var record map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		row, err := recoverableRow(ctx, conn, executionID, jobID)
		if err != nil || row == nil {
			return err
		}
		if live, err := rowOwnerLive(row); err != nil || live {
			return err
		}
		status := "failed"
		if success {
			status = "completed"
		}
		if _, err := conn.ExecContext(ctx, `UPDATE executions SET status=?, finished_at=?, error=?, handoff_pending=0,
               handoff_started_at=NULL, delivery_outcome=? WHERE id=?`,
			status, now, failureDetail(success, errText), nullable(deliveryOutcome), executionID); err != nil {
			return err
		}
		if err := pruneExecutions(ctx, conn); err != nil {
			return err
		}
		record, err = fetchExecution(ctx, conn, executionID)
		return err
	})
	if err != nil || record == nil {
		return nil, err
	}
	emitExecutionStateSafe(record, deliveryOutcome)
	recordCronFinish(record, deliveryOutcome)
	return record, nil
}

// Attempt's own firer can still finish it, so receipt recovery must not act for it
func (l *Ledger) OwnerLive(ctx context.Context, executionID, jobID string) (bool, error) {
	var row map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		var err error
		row, err = recoverableRow(ctx, conn, executionID, jobID)
		return err
	})
	if err != nil || row == nil {
		return false, err
	}
	return rowOwnerLive(row)
}

// Replaces a terminal attempt's queued delivery outcome with the drain's real one
func (l *Ledger) SettleDeliveryOutcome(ctx context.Context, executionID, outcome string) (bool, error) {
	var n int64
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		var err error
		n, err = rowsChanged(conn.ExecContext(ctx, `UPDATE executions SET delivery_outcome=?
               WHERE id=? AND delivery_outcome='queued'
                 AND status IN ('completed','failed','unknown')`, outcome, executionID))
		return err
	})
	return n == 1, err
}

// Closes the receipt of a run that never started, logging a ledger failure instead of raising it
func (l *Ledger) SettleUnstarted(ctx context.Context, executionID, jobID, errText string) {
	if _, err := l.Finish(ctx, executionID, false, &errText, nil, false); err != nil {
		l.s.log.Error(fmt.Sprintf("Job '%s': failed to close execution receipt %s (%s): %v", jobID, executionID, errText, err))
	}
}

// Marks attempts of dead owners and wedged live owners unknown without scheduling retries
func (l *Ledger) RecoverInterrupted(ctx context.Context) (int, error) {
	now := l.s.clk.nowISO()
	changed := 0
	var recovered []map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, `SELECT id, status, process_id, pid, process_started_at,
                      handoff_pending, handoff_started_at, claimed_at, progress_at
               FROM executions
               WHERE status IN ('claimed','running')`)
		if err != nil {
			return err
		}
		recs, err := scanRecords(rows)
		if err != nil {
			return err
		}
		staleAfter, staleKnown, staleResolved := 0.0, false, false
		for _, row := range recs {
			if recText(row, "process_id") == executionProcessID {
				continue
			}
			reason := ownerGoneReason
			live, err := ownerIsLive(recInt(row, "pid"), row["process_started_at"])
			if err != nil {
				return err
			}
			if live {
				if !staleResolved {
					staleAfter, staleKnown = l.s.liveOwnerStaleAfterSeconds(ctx)
					staleResolved = true
				}
				if !staleKnown {
					continue
				}
				age, err := l.s.staleAgeSeconds(recText(row, "claimed_at"), row["progress_at"])
				if err != nil {
					return err
				}
				if age <= staleAfter {
					continue
				}
				reason = ownerWedgedReason
			}
			if handoffFenced(row) {
				continue
			}
			n, err := rowsChanged(conn.ExecContext(ctx, `UPDATE executions
                   SET status='unknown', finished_at=?, error=?,
                       handoff_pending=0, handoff_started_at=NULL
                   WHERE id=? AND status=? AND process_id=? AND pid=?
                     AND handoff_pending=?
                     AND handoff_started_at IS ?
                     AND claimed_at=? AND progress_at IS ?`,
				now, reason, row["id"], row["status"], row["process_id"], row["pid"],
				row["handoff_pending"], row["handoff_started_at"], row["claimed_at"], row["progress_at"]))
			if err != nil {
				return err
			}
			changed += int(n)
			if n != 0 {
				record, err := fetchExecution(ctx, conn, recText(row, "id"))
				if err != nil {
					return err
				}
				if record != nil {
					recovered = append(recovered, record)
				}
			}
		}
		if changed != 0 {
			return pruneExecutions(ctx, conn)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, record := range recovered {
		emitExecutionStateSafe(record, nil)
	}
	return changed, nil
}

// Pending handoff still inside its adoption grace
func handoffFenced(row map[string]any) bool {
	if recInt(row, "handoff_pending") == 0 {
		return false
	}
	var started float64
	switch v := row["handoff_started_at"].(type) {
	case float64:
		started = v
	case int64:
		started = float64(v)
	default:
		return false
	}
	return ledgerWallTime()-started < HandoffAdoptionGraceSeconds
}

// Records one dead owner's attempt unknown with the cause this process observed, false when it may not
func (l *Ledger) TerminalizeDeadOwner(ctx context.Context, executionID, reason string) (bool, error) {
	now := l.s.clk.nowISO()
	var record map[string]any
	done := false
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		row, err := queryRecord(ctx, conn, `SELECT id, status, process_id, pid, process_started_at,
                      handoff_pending, handoff_started_at
               FROM executions WHERE id=?`, executionID)
		if err != nil || row == nil {
			return err
		}
		if st := recText(row, "status"); st != "claimed" && st != "running" {
			return nil
		}
		if recText(row, "process_id") == executionProcessID {
			return nil
		}
		if live, err := ownerIsLive(recInt(row, "pid"), row["process_started_at"]); err != nil || live {
			return err
		}
		if handoffFenced(row) {
			return nil
		}
		n, err := rowsChanged(conn.ExecContext(ctx, `UPDATE executions
               SET status='unknown', finished_at=?, error=?,
                   handoff_pending=0, handoff_started_at=NULL
               WHERE id=? AND status=? AND process_id=? AND pid=?`,
			now, reason, row["id"], row["status"], row["process_id"], row["pid"]))
		if err != nil || n != 1 {
			return err
		}
		if record, err = fetchExecution(ctx, conn, executionID); err != nil {
			return err
		}
		done = true
		return pruneExecutions(ctx, conn)
	})
	if err != nil || !done {
		return false, err
	}
	emitExecutionStateSafe(record, nil)
	return true, nil
}

// Records this process's own in flight attempt unknown when the firer gives up on it
func (l *Ledger) GiveUp(ctx context.Context, executionID, reason string) (bool, error) {
	now := l.s.clk.nowISO()
	var record map[string]any
	done := false
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		n, err := rowsChanged(conn.ExecContext(ctx, `UPDATE executions SET status='unknown', finished_at=?, error=?,
                   handoff_pending=0, handoff_started_at=NULL
               WHERE id=? AND status IN ('claimed','running') AND process_id=? AND pid=?`,
			now, reason, executionID, executionProcessID, os.Getpid()))
		if err != nil || n != 1 {
			return err
		}
		if record, err = fetchExecution(ctx, conn, executionID); err != nil {
			return err
		}
		done = true
		return pruneExecutions(ctx, conn)
	})
	if err != nil || !done {
		return false, err
	}
	emitExecutionStateSafe(record, nil)
	return true, nil
}

// Newest first execution history with a claimed_at cursor, limit clamped to 1..500
func (l *Ledger) List(ctx context.Context, jobID *string, limit int, beforeClaimedAt *string) ([]map[string]any, error) {
	var clauses []string
	var params []any
	if jobID != nil {
		clauses = append(clauses, "job_id=?")
		params = append(params, *jobID)
	}
	if beforeClaimedAt != nil {
		clauses = append(clauses, "(julianday(claimed_at), claimed_at) < (julianday(?), ?)")
		params = append(params, *beforeClaimedAt, *beforeClaimedAt)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	params = append(params, max(1, min(limit, 500)))
	var out []map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT * FROM executions"+where+
			" ORDER BY julianday(claimed_at) DESC, claimed_at DESC, id DESC LIMIT ?", params...)
		if err != nil {
			return err
		}
		out, err = scanRecords(rows)
		return err
	})
	return out, err
}

// One exact execution attempt, nil when absent
func (l *Ledger) Get(ctx context.Context, executionID string) (map[string]any, error) {
	var record map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		var err error
		record, err = fetchExecution(ctx, conn, executionID)
		return err
	})
	return record, err
}

// Newest attempt of the job, nil when it has none
func (l *Ledger) Latest(ctx context.Context, jobID string) (map[string]any, error) {
	rows, err := l.List(ctx, &jobID, 1, nil)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// Job's latest non missed attempt while it is claimed or running under a live owner
func (l *Ledger) LiveInflight(ctx context.Context, jobID string) (map[string]any, error) {
	var record map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		var err error
		record, err = queryRecord(ctx, conn, `SELECT * FROM executions WHERE job_id=? AND status != 'missed'
               ORDER BY julianday(claimed_at) DESC, claimed_at DESC, id DESC LIMIT 1`, jobID)
		return err
	})
	if err != nil || record == nil {
		return nil, err
	}
	if st := recText(record, "status"); st != "claimed" && st != "running" {
		return nil, nil
	}
	live, err := ownerIsLive(recInt(record, "pid"), record["process_started_at"])
	if err != nil || !live {
		return nil, err
	}
	return record, nil
}

// Job's newest claimed or running attempt under a live owner even when a newer row exists
func (l *Ledger) Cancellable(ctx context.Context, jobID string) (map[string]any, error) {
	live, err := l.Live(ctx, []string{jobID})
	if err != nil {
		return nil, err
	}
	return live[jobID], nil
}

// Distinct non empty ids in first seen order
func cleanJobIDs(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Placeholder list and arguments for an IN clause
func inClause(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// Each job's newest claimed or running attempt under a live owner, in one query
func (l *Ledger) Live(ctx context.Context, jobIDs []string) (map[string]map[string]any, error) {
	clean := cleanJobIDs(jobIDs)
	live := map[string]map[string]any{}
	if len(clean) == 0 {
		return live, nil
	}
	marks, args := inClause(clean)
	var recs []map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, `SELECT * FROM executions WHERE job_id IN (`+marks+`) AND status IN ('claimed', 'running')
               ORDER BY julianday(claimed_at) DESC, claimed_at DESC, id DESC`, args...)
		if err != nil {
			return err
		}
		recs, err = scanRecords(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, row := range recs {
		jobID := recText(row, "job_id")
		if _, done := live[jobID]; done {
			continue
		}
		ok, err := rowOwnerLive(row)
		if err != nil {
			return nil, err
		}
		if ok {
			live[jobID] = row
		}
	}
	return live, nil
}

// Latest execution of each job in one query
func (l *Ledger) LatestMany(ctx context.Context, jobIDs []string) (map[string]map[string]any, error) {
	clean := cleanJobIDs(jobIDs)
	out := map[string]map[string]any{}
	if len(clean) == 0 {
		return out, nil
	}
	marks, args := inClause(clean)
	var recs []map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, `SELECT e.* FROM executions e WHERE e.id IN (
                  SELECT id FROM (
                    SELECT id, ROW_NUMBER() OVER (
                             PARTITION BY job_id
                             ORDER BY julianday(claimed_at) DESC, claimed_at DESC, id DESC
                           ) AS rn
                    FROM executions WHERE job_id IN (`+marks+`))
                  WHERE rn=1)`, args...)
		if err != nil {
			return err
		}
		recs, err = scanRecords(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, row := range recs {
		out[recText(row, "job_id")] = row
	}
	return out, nil
}

// Completed attempts of one occurrence of the job
func (l *Ledger) completed(ctx context.Context, jobID, instant string) ([]executionStamp, error) {
	var recs []map[string]any
	err := l.transaction(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT id, finished_at, claimed_at FROM executions WHERE job_id=? AND scheduled_instant=? AND status='completed'", jobID, instant)
		if err != nil {
			return err
		}
		recs, err = scanRecords(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]executionStamp, 0, len(recs))
	for _, r := range recs {
		stamp := executionStamp{ID: recText(r, "id")}
		if v, ok := r["finished_at"].(string); ok {
			stamp.FinishedAt = &v
		}
		if v, ok := r["claimed_at"].(string); ok {
			stamp.ClaimedAt = &v
		}
		out = append(out, stamp)
	}
	return out, nil
}

// Books skipped occurrences of the job in the store ledger
func recordMissedOccurrences(s *Store, jobID string, count int, firstInstant *string, detail string) error {
	_, err := openExecutionsLedger(s).RecordMissed(context.Background(), jobID, count, firstInstant, detail)
	return err
}

// Completed attempts of one occurrence in the store ledger
func completedExecutions(s *Store, jobID, instant string) ([]executionStamp, error) {
	return openExecutionsLedger(s).completed(context.Background(), jobID, instant)
}

// Latest attempt of each job in the store ledger
func latestExecutions(s *Store, ids []string) (map[string]map[string]any, error) {
	return openExecutionsLedger(s).LatestMany(context.Background(), ids)
}

// Live attempt of each job in the store ledger
func liveExecutions(s *Store, ids []string) (map[string]map[string]any, error) {
	return openExecutionsLedger(s).Live(context.Background(), ids)
}
