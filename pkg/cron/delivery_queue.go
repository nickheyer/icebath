package cron

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	// Delivery queue file a test points elsewhere
	deliveryDBOverride  string
	deliveryProcessID   = newExecutionID()
	deliveryMu          sync.Mutex
	activeDeliveries    = map[string]bool{}
	deliveryTerminal    = map[string]bool{"delivered": true, "failed": true, "unknown": true, "suppressed": true}
	deliveryPollSleep   = time.Sleep
	deliveryPollMonoNow = time.Now
	// Owner liveness probe and tombstone probe hook a test replaces
	deliveryOwnerIsLive         = ownerIsLive
	deliveryAfterTombstoneProbe = func(ctx context.Context) {}
	// Row terminalizer a test makes fail to prove the same gateway fence
	deliveryFinish = (*DeliveryQueue).finish
)

// MaxTerminalDeliveries bounds the terminal rows kept before older ones become tombstones
var MaxTerminalDeliveries = 1000

// DefaultDeliveryWaitTimeoutSeconds is how long a worker waits for a gateway outcome
const DefaultDeliveryWaitTimeoutSeconds = 300.0

const deliveryTerminalSQL = "('delivered','failed','unknown','suppressed')"

// DeliveryQueue is the profile local durable handoff of cron sends to a live gateway
type DeliveryQueue struct {
	s    *Store
	path string
}

// Queue file of a home, the test override winning
func QueuePath(home string) string {
	if deliveryDBOverride != "" {
		return deliveryDBOverride
	}
	resolved, err := pypath.Realpath(home, pypath.Lax)
	if err != nil {
		resolved = filepath.Clean(home)
	}
	return filepath.Join(resolved, "cron", "deliveries.db")
}

// Delivery queue of the store home
func openDeliveryQueue(s *Store) *DeliveryQueue { return &DeliveryQueue{s: s, path: QueuePath(s.home)} }

// Redacts terminal payloads and turns rows past the retention bound into tombstones
func pruneTerminalDeliveries(ctx context.Context, conn state.SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, `UPDATE deliveries SET job_json='{}', content=''
           WHERE status IN `+deliveryTerminalSQL+`
             AND (job_json != '{}' OR content != '')`); err != nil {
		return err
	}
	keep := max(0, MaxTerminalDeliveries)
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries WHERE status IN "+deliveryTerminalSQL).Scan(&count); err != nil {
		return err
	}
	excess := count - keep
	if excess <= 0 {
		return nil
	}
	order := ` ORDER BY julianday(finished_at), finished_at,
                        julianday(created_at), created_at, execution_id
               LIMIT ?`
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO delivery_tombstones
               (execution_id, terminal_status, finished_at)
               SELECT execution_id, status, finished_at FROM deliveries
               WHERE status IN `+deliveryTerminalSQL+order, excess); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, `DELETE FROM deliveries WHERE execution_id IN (
                 SELECT execution_id FROM deliveries
                 WHERE status IN `+deliveryTerminalSQL+order+`)`, excess)
	return err
}

// Widens a pre suppressed status check in place, then creates the tables and the late column
func initializeDeliverySchema(ctx context.Context, conn state.SQLiteConn) error {
	for _, table := range []string{"deliveries", "delivery_tombstones"} {
		var ddl string
		err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&ddl)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.Contains(ddl, "'suppressed'") {
			continue
		}
		if _, err := conn.ExecContext(ctx, "SAVEPOINT notification_disposition"); err != nil {
			return err
		}
		steps := []string{
			fmt.Sprintf("ALTER TABLE %s RENAME TO %s_old", table, table),
			strings.ReplaceAll(ddl, "'unknown'", "'unknown','suppressed'"),
			fmt.Sprintf("INSERT INTO %s SELECT * FROM %s_old", table, table),
			fmt.Sprintf("DROP TABLE %s_old", table),
		}
		for _, step := range steps {
			if _, err := conn.ExecContext(ctx, step); err != nil {
				_, _ = conn.ExecContext(ctx, "ROLLBACK TO notification_disposition")
				_, _ = conn.ExecContext(ctx, "RELEASE notification_disposition")
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "RELEASE notification_disposition"); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS deliveries (
             execution_id TEXT PRIMARY KEY,
             job_json TEXT NOT NULL,
             content TEXT NOT NULL,
             for_failure INTEGER NOT NULL DEFAULT 0,
             status TEXT NOT NULL CHECK(status IN
               ('pending','delivering','delivered','failed','unknown','suppressed')),
             owner_process_id TEXT,
             owner_pid INTEGER,
             owner_started_at INTEGER,
             created_at TEXT NOT NULL,
             finished_at TEXT,
             error TEXT
           )`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS delivery_tombstones (
             execution_id TEXT PRIMARY KEY,
             terminal_status TEXT NOT NULL CHECK(terminal_status IN
               ('delivered','failed','unknown','suppressed')),
             finished_at TEXT
           )`); err != nil {
		return err
	}
	_, err := state.AddColumnIfMissing(ctx, conn, "deliveries", "for_failure", "for_failure INTEGER NOT NULL DEFAULT 0")
	return err
}

// Runs body in one queue transaction under the queue lock, the file kept private
func (q *DeliveryQueue) transaction(ctx context.Context, immediate bool, body func(conn state.SQLiteConn) error) error {
	deliveryMu.Lock()
	defer deliveryMu.Unlock()
	conn, err := state.OpenDB(ctx, q.path, "cron/deliveries.db", state.WithSynchronousFull(), state.WithInitialize(initializeDeliverySchema))
	if err != nil {
		return err
	}
	_ = os.Chmod(q.path, 0o600)
	return state.Transaction(ctx, conn, immediate, body)
}

// Job record as sorted key JSON keeping non ASCII text raw
func deliveryJobJSON(job *Job) (string, error) {
	raw, err := job.MarshalJSON()
	if err != nil {
		return "", err
	}
	decoded, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return "", err
	}
	return pyjson.Dumps(decoded, pyjson.SortKeys(true), pyjson.EnsureASCII(false))
}

// Tombstone of a pruned terminal row as a status record
func tombstoneRecord(ctx context.Context, conn state.SQLiteConn, executionID string, withError bool) (map[string]any, error) {
	rec, err := queryRecord(ctx, conn, "SELECT execution_id, terminal_status, finished_at FROM delivery_tombstones WHERE execution_id=?", executionID)
	if err != nil || rec == nil {
		return nil, err
	}
	out := map[string]any{"execution_id": rec["execution_id"], "status": rec["terminal_status"], "finished_at": rec["finished_at"]}
	if withError {
		out["error"] = nil
	}
	return out, nil
}

// Persists one idempotent delivery request before the worker waits
func (q *DeliveryQueue) Enqueue(ctx context.Context, executionID string, job *Job, content string, forFailure bool) (map[string]any, error) {
	jobJSON, err := deliveryJobJSON(job)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = q.transaction(ctx, true, func(conn state.SQLiteConn) error {
		tomb, err := tombstoneRecord(ctx, conn, executionID, false)
		if err != nil || tomb != nil {
			out = tomb
			return err
		}
		deliveryAfterTombstoneProbe(ctx)
		flag := 0
		if forFailure {
			flag = 1
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO deliveries
               (execution_id, job_json, content, for_failure, status, created_at)
               VALUES (?, ?, ?, ?, 'pending', ?)`, executionID, jobJSON, content, flag, FormatISO(q.s.clk.now())); err != nil {
			return err
		}
		out, err = queryRecord(ctx, conn, "SELECT * FROM deliveries WHERE execution_id=?", executionID)
		return err
	})
	return out, err
}

// Queue row or tombstone of an execution, nil when neither exists
func (q *DeliveryQueue) GetStatus(ctx context.Context, executionID string) (map[string]any, error) {
	var out map[string]any
	err := q.transaction(ctx, false, func(conn state.SQLiteConn) error {
		row, err := queryRecord(ctx, conn, "SELECT * FROM deliveries WHERE execution_id=?", executionID)
		if err != nil || row != nil {
			out = row
			return err
		}
		out, err = tombstoneRecord(ctx, conn, executionID, true)
		return err
	})
	return out, err
}

// Atomically claims the oldest pending send for this process, the job decoded under the job key
func (q *DeliveryQueue) ClaimNext(ctx context.Context) (map[string]any, error) {
	pid := os.Getpid()
	var started any
	if st, ok := ledgerStartTime(pid); ok {
		started = st
	}
	var claimed map[string]any
	err := q.transaction(ctx, false, func(conn state.SQLiteConn) error {
		row, err := queryRecord(ctx, conn, "SELECT execution_id FROM deliveries WHERE status='pending' ORDER BY julianday(created_at), created_at, execution_id LIMIT 1")
		if err != nil || row == nil {
			return err
		}
		id := recText(row, "execution_id")
		n, err := rowsChanged(conn.ExecContext(ctx, `UPDATE deliveries SET status='delivering', owner_process_id=?,
               owner_pid=?, owner_started_at=?
               WHERE execution_id=? AND status='pending'`, deliveryProcessID, pid, started, id))
		if err != nil || n != 1 {
			return err
		}
		claimed, err = queryRecord(ctx, conn, "SELECT * FROM deliveries WHERE execution_id=?", id)
		activeDeliveries[id] = true
		return err
	})
	if err != nil || claimed == nil {
		return nil, err
	}
	var job Job
	if err := json.Unmarshal([]byte(recText(claimed, "job_json")), &job); err != nil {
		return nil, err
	}
	delete(claimed, "job_json")
	claimed["job"] = &job
	return claimed, nil
}

// Terminalizes a claimed row owned by this process as failed, suppressed or delivered
func (q *DeliveryQueue) finish(ctx context.Context, executionID string, errText *string, suppressed bool) (bool, error) {
	status := "delivered"
	var safe any
	switch {
	case errText != nil && *errText != "":
		status = "failed"
		safe = tools.RedactSensitiveTextWith(&tools.Env{Home: q.s.home}, *errText, tools.RedactOptions{Force: true, URLCredentials: true})
	case suppressed:
		status = "suppressed"
	}
	var n int64
	err := q.transaction(ctx, false, func(conn state.SQLiteConn) error {
		var err error
		n, err = rowsChanged(conn.ExecContext(ctx, `UPDATE deliveries SET status=?, finished_at=?, error=?
               WHERE execution_id=? AND status='delivering'
                 AND owner_process_id=? AND owner_pid=?`, status, FormatISO(q.s.clk.now()), safe, executionID, deliveryProcessID, os.Getpid()))
		if err != nil {
			return err
		}
		return pruneTerminalDeliveries(ctx, conn)
	})
	return n == 1, err
}

// Fences dead delivery owners as unknown without replaying, writing each outcome back
func (q *DeliveryQueue) RecoverAbandoned(ctx context.Context) (int, error) {
	type fencedRow struct{ jobID, executionID string }
	var fenced []fencedRow
	err := q.transaction(ctx, false, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT execution_id, job_json, owner_process_id, owner_pid, owner_started_at FROM deliveries WHERE status='delivering'")
		if err != nil {
			return err
		}
		recs, err := scanRecords(rows)
		if err != nil {
			return err
		}
		for _, row := range recs {
			id := recText(row, "execution_id")
			sameProcess := row["owner_process_id"] == deliveryProcessID
			if sameProcess {
				if activeDeliveries[id] {
					continue
				}
			} else if live, err := deliveryOwnerIsLive(recInt(row, "owner_pid"), row["owner_started_at"]); err != nil {
				return err
			} else if live {
				continue
			}
			msg := "Gateway exited during delivery; send outcome is unknown and was not retried."
			if sameProcess {
				msg = "Gateway finished delivery but could not persist its outcome; send was not retried."
			}
			n, err := rowsChanged(conn.ExecContext(ctx, `UPDATE deliveries SET status='unknown', finished_at=?, error=?
                   WHERE execution_id=? AND status='delivering'`, FormatISO(q.s.clk.now()), msg, id))
			if err != nil {
				return err
			}
			if n > 0 {
				jobJSON := recText(row, "job_json")
				if jobJSON == "" {
					jobJSON = "{}"
				}
				decoded, err := pyjson.Loads(jobJSON)
				if err != nil {
					return err
				}
				jobID := ""
				if obj, ok := decoded.(*pyjson.Object); ok {
					jobID, _ = obj.Vals["id"].(string)
				}
				fenced = append(fenced, fencedRow{jobID, id})
			}
		}
		return pruneTerminalDeliveries(ctx, conn)
	})
	if err != nil {
		return 0, err
	}
	for _, f := range fenced {
		q.s.settleQuietly(ctx, f.jobID, f.executionID)
	}
	return len(fenced), nil
}

// DeliverySend performs one queued send, returning the refusal text or nil when delivered
type DeliverySend func(job *Job, content string, forFailure bool) (*string, error)

// Delivers up to limit pending rows through send, terminalizing every claimed row
func (q *DeliveryQueue) Drain(ctx context.Context, send DeliverySend, limit int) (int, error) {
	if _, err := q.RecoverAbandoned(ctx); err != nil {
		return 0, err
	}
	processed := 0
	for range max(0, limit) {
		row, err := q.ClaimNext(ctx)
		if err != nil {
			return processed, err
		}
		if row == nil {
			break
		}
		id := recText(row, "execution_id")
		deliveryMu.Lock()
		activeDeliveries[id] = true
		deliveryMu.Unlock()
		err = func() error {
			defer func() {
				deliveryMu.Lock()
				delete(activeDeliveries, id)
				deliveryMu.Unlock()
			}()
			job := row["job"].(*Job)
			errText := callDeliverySend(send, job, recText(row, "content"), recInt(row, "for_failure") != 0)
			suppressed := job.run.notificationAllTargetsSuppressed
			if raw, stored := job.Extra["_notification_all_targets_suppressed"]; stored && !suppressed {
				var flag any
				suppressed = json.Unmarshal(raw, &flag) == nil && truthyJSON(flag)
			}
			ok, err := deliveryFinish(q, ctx, id, errText, suppressed)
			if err != nil {
				return err
			}
			if ok {
				q.s.settleQuietly(ctx, job.ID, id)
			}
			return nil
		}()
		if err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

// Runs send turning a returned error or panic into its Python style refusal text
func callDeliverySend(send DeliverySend, job *Job, content string, forFailure bool) (errText *string) {
	defer func() {
		if r := recover(); r != nil {
			text := fmt.Sprintf("RuntimeError: %v", r)
			errText = &text
		}
	}()
	refusal, err := send(job, content, forFailure)
	if err != nil {
		text := cronExceptionName(err) + ": " + err.Error()
		return &text
	}
	return refusal
}

// Python truthiness of a decoded JSON value
func truthyJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Fences a row caught mid send as unknown when the worker stops waiting, leaving pending rows queued
func (q *DeliveryQueue) terminalizeWaitTimeout(ctx context.Context, executionID string) (string, error) {
	now := FormatISO(q.s.clk.now())
	var row map[string]any
	deferred := false
	err := q.transaction(ctx, false, func(conn state.SQLiteConn) error {
		cur, err := queryRecord(ctx, conn, "SELECT status FROM deliveries WHERE execution_id=?", executionID)
		if err != nil {
			return err
		}
		if cur != nil && cur["status"] == "pending" {
			q.s.log.Warn(fmt.Sprintf("Cron delivery %s: no live gateway within the wait budget; left queued for the next gateway", executionID))
			deferred = true
			return nil
		}
		if _, err := conn.ExecContext(ctx, `UPDATE deliveries SET status='unknown', finished_at=?, error=?
               WHERE execution_id=? AND status='delivering'`, now, "timed out while gateway delivery was in progress; outcome is unknown and was not retried", executionID); err != nil {
			return err
		}
		if row, err = queryRecord(ctx, conn, "SELECT status, error FROM deliveries WHERE execution_id=?", executionID); err != nil {
			return err
		}
		return pruneTerminalDeliveries(ctx, conn)
	})
	if err != nil || deferred {
		return "", err
	}
	if row == nil {
		return "timed out waiting for live gateway delivery", nil
	}
	return deliveryRowError(row), nil
}

// Refusal text of a terminal row, empty when delivered or suppressed
func deliveryRowError(row map[string]any) string {
	status := recText(row, "status")
	if status == "delivered" || status == "suppressed" {
		return ""
	}
	if text := recText(row, "error"); text != "" {
		return text
	}
	return "delivery " + status
}

// Queues a delivery and waits for a gateway's terminal at most once outcome, nil when delivered
func (q *DeliveryQueue) EnqueueAndWait(ctx context.Context, executionID string, job *Job, content string, forFailure bool, timeout *float64) (*string, error) {
	queued, err := q.Enqueue(ctx, executionID, job, content, forFailure)
	if err != nil {
		return nil, err
	}
	optional := func(text string) *string {
		if text == "" {
			return nil
		}
		return &text
	}
	if deliveryTerminal[recText(queued, "status")] {
		return optional(deliveryRowError(queued)), nil
	}
	wait := DefaultDeliveryWaitTimeoutSeconds
	if timeout != nil {
		wait = max(0, *timeout)
	}
	deadline := deliveryPollMonoNow().Add(time.Duration(wait * float64(time.Second)))
	for {
		row, err := q.GetStatus(ctx, executionID)
		if err != nil {
			return nil, err
		}
		if row != nil && deliveryTerminal[recText(row, "status")] {
			return optional(deliveryRowError(row)), nil
		}
		remaining := deadline.Sub(deliveryPollMonoNow())
		if remaining <= 0 {
			break
		}
		deliveryPollSleep(min(time.Second, remaining))
	}
	text, err := q.terminalizeWaitTimeout(ctx, executionID)
	return optional(text), err
}
