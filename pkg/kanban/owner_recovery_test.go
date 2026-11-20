package kanban

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/state"
)

const ownerTestFingerprint = "6a232414-2dbf-40ab-a534-d9b71db05658:116|4162240"

// A pid that does not exist on this host, so a fixture never hits a process
func absentPid(base int) int {
	pid := base
	for proc.Exists(pid) {
		pid++
	}
	return pid
}

// Scratch board plus the scratch admission ledger an owner receipt points at
type ownerBoard struct {
	t      *testing.T
	conn   state.SQLiteConn
	ledger string
	now    int64
	host   string
}

type deadAttempt struct {
	workerPid         *int
	workerFingerprint any
	workerBound       bool
	skipWorkerPid     bool
	ownerPid          *int
	ownerBirth        any
	admission         []any
	expiresAgeSeconds *int64
	lockHost          string
	heartbeatAge      *int64
}

func newOwnerBoard(t *testing.T) *ownerBoard {
	t.Helper()
	t.Setenv("ICEBATH_KANBAN_CRASH_GRACE_SECONDS", "0")
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	path := filepath.Join(root, "kanban.db")
	if _, err := InitDB(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
	conn, err := Connect(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	b := &ownerBoard{t: t, conn: conn, ledger: filepath.Join(root, "state.db"), now: time.Now().Unix()}
	b.host, _, _ = strings.Cut(claimerID(), ":")
	b.store("CREATE TABLE session_admissions (target_session_id TEXT, request_id TEXT, status TEXT, outcome TEXT)")
	return b
}

// Runs one statement against the owner ledger
func (b *ownerBoard) store(query string, args ...any) {
	b.t.Helper()
	db, err := sql.Open("sqlite", b.ledger)
	if err != nil {
		b.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		b.t.Fatal(err)
	}
}

func (b *ownerBoard) exec(query string, args ...any) int64 {
	b.t.Helper()
	res, err := b.conn.ExecContext(context.Background(), query, args...)
	if err != nil {
		b.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (b *ownerBoard) event(tid string, run int64, kind string, body map[string]any, ts int64) {
	b.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		b.t.Fatal(err)
	}
	b.exec("INSERT INTO task_events (task_id, run_id, kind, payload, created_at) VALUES (?, ?, ?, ?, ?)", tid, run, kind, string(raw), ts)
}

// A running card whose worker died the way t_5ea03658 did
func (b *ownerBoard) deadAttempt(tid string, o deadAttempt) int64 {
	b.t.Helper()
	workerPid := absentPid(3_500_000)
	if o.workerPid != nil {
		workerPid = *o.workerPid
	}
	ownerPid := absentPid(3_600_000)
	if o.ownerPid != nil {
		ownerPid = *o.ownerPid
	}
	fingerprint := o.workerFingerprint
	if fingerprint == nil {
		fingerprint = ownerTestFingerprint
	}
	admission := o.admission
	if admission == nil {
		admission = []any{"terminal", "interrupted"}
	}
	host := b.host
	if o.lockHost != "" {
		host = o.lockHost
	}
	lock := fmt.Sprintf("%s:%d", host, ownerPid)
	expires := b.now - 15*3600
	if o.expiresAgeSeconds != nil {
		expires = b.now - *o.expiresAgeSeconds
	}
	started := b.now - 15*3600
	var recordedPid, recordedPrint any
	if !o.skipWorkerPid {
		recordedPid, recordedPrint = workerPid, fingerprint
	}
	run := b.exec("INSERT INTO task_runs (task_id, profile, status, claim_lock, claim_expires, worker_pid, started_at) VALUES (?, 'default', 'running', ?, ?, ?, ?)",
		tid, lock, expires, recordedPid, started)
	var heartbeat any
	if o.heartbeatAge != nil {
		heartbeat = b.now - *o.heartbeatAge
	}
	b.exec("INSERT INTO tasks (id, title, assignee, status, created_at, claim_lock, claim_expires, worker_pid, worker_started_at, last_heartbeat_at, current_run_id, started_at, priority) "+
		"VALUES (?, ?, 'default', 'running', ?, ?, ?, ?, ?, ?, ?, ?, 5)", tid, tid, b.now-86400, lock, expires, recordedPid, recordedPrint, heartbeat, run, started)
	b.event(tid, run, "claimed", map[string]any{"source_status": "ready"}, started)
	spawned := map[string]any{}
	if !o.skipWorkerPid {
		spawned = map[string]any{"pid": workerPid, "started_at": fingerprint}
	}
	b.event(tid, run, "spawned", spawned, started)
	requestID := fmt.Sprintf(`kanban:["default","%s",1]`, tid)
	receipt := map[string]any{"db": b.ledger, "session_id": "sess-1", "pid": ownerPid, "request_id": requestID}
	if o.ownerBirth != nil {
		receipt["birth"] = o.ownerBirth
	}
	b.event(tid, run, "owner_admitted", receipt, started+30)
	if o.workerBound {
		b.event(tid, run, "worker_bound", map[string]any{"pid": workerPid, "claim_lock": lock, "started_at": fingerprint}, started+60)
	}
	if len(admission) > 0 {
		b.store("INSERT INTO session_admissions VALUES ('sess-1', ?, ?, ?)", requestID, admission[0], admission[1])
	}
	return run
}

func (b *ownerBoard) state(tid string) *Row {
	b.t.Helper()
	return mustRows(b.t, b.conn, "SELECT * FROM tasks WHERE id = ?", tid)[0]
}

func ownFingerprint(t *testing.T) string {
	t.Helper()
	fp := processFingerprint(os.Getpid())
	if fp == nil {
		t.Fatal("no fingerprint for this process")
	}
	return *fp
}

func intp(n int) *int { return &n }

func TestASettledAdmissionIsProofEvenWhileTheOwnerGatewayIsAlive(t *testing.T) {
	b := newOwnerBoard(t)
	ctx := context.Background()
	b.deadAttempt("t_ghost2", deadAttempt{})
	b.deadAttempt("t_gateway", deadAttempt{ownerPid: intp(os.Getpid())})
	b.deadAttempt("t_bound", deadAttempt{workerBound: true, ownerPid: intp(os.Getpid())})
	if paused, err := ownerReclaimPaused(ctx, b.conn, "t_gateway"); err != nil || !paused {
		t.Fatal("pause lifted", paused, err)
	}
	if over, err := ownerAttemptProvablyOver(ctx, b.conn, "t_gateway"); err != nil || !over {
		t.Fatal("a settled admission fails to end the attempt", over, err)
	}
	if over, err := ownerAttemptProvablyOver(ctx, b.conn, "t_bound"); err != nil || over {
		t.Fatal("bound run judged over", over, err)
	}
	if n := mustRelease(t, b.conn, nil); n != 2 {
		t.Fatal("released", n)
	}
	row := b.state("t_gateway")
	if row.Get("status") != "ready" || row.Get("claim_lock") != nil || row.Get("worker_pid") != nil {
		t.Fatal("gateway attempt kept", row.Get("status"))
	}
	run := mustRows(t, b.conn, "SELECT outcome, error FROM task_runs WHERE task_id = 't_gateway'")[0]
	if run.Get("outcome") != "reclaimed" || !strings.Contains(cellStr(run.Get("error")), "owner admission settled interrupted") {
		t.Fatal("run", run.Get("outcome"), run.Get("error"))
	}
	if n, _ := pyIntOf(b.state("t_gateway").Get("consecutive_failures")); n != 1 {
		t.Fatal("consecutive_failures", n)
	}
	if b.state("t_bound").Get("status") != "running" {
		t.Fatal("bound run released")
	}
}

// Reclaim entry points keyed by the parametrize ids
var ownerReclaimPaths = map[string]func(t *testing.T, conn state.SQLiteConn) bool{
	"ttl": func(t *testing.T, conn state.SQLiteConn) bool { return mustRelease(t, conn, nil) != 0 },
	"stale": func(t *testing.T, conn state.SQLiteConn) bool {
		out, err := DetectStaleRunning(context.Background(), conn, 1, func(int, syscall.Signal) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return len(out) != 0
	},
	"runtime": func(t *testing.T, conn state.SQLiteConn) bool {
		return len(mustEnforce(t, conn, func(int, syscall.Signal) error { return nil })) != 0
	},
}

func TestAnInterpreterBoundAfterTheGateKeepsItsClaim(t *testing.T) {
	for _, path := range []string{"runtime", "stale", "ttl"} {
		t.Run(path, func(t *testing.T) {
			b := newOwnerBoard(t)
			run := b.deadAttempt("t_ghost", deadAttempt{})
			b.exec("UPDATE tasks SET max_runtime_seconds = 1 WHERE id = 't_ghost'")
			var calls []bool
			patch(t, &ownerReclaimBlocked, func(ctx context.Context, conn state.SQLiteConn, taskID string, crashSweep bool) (bool, error) {
				verdict, err := ownerReclaimBlockedLive(ctx, conn, taskID, crashSweep)
				if len(calls) == 0 {
					b.event(taskID, run, "worker_bound", map[string]any{"pid": os.Getpid(), "started_at": ownFingerprint(t)}, b.now)
				}
				calls = append(calls, verdict)
				return verdict, err
			})
			if ownerReclaimPaths[path](t, b.conn) {
				t.Fatal("reclaimed beside a recovered owner")
			}
			if len(calls) == 0 || calls[0] || !calls[len(calls)-1] {
				t.Fatal("gate verdicts", calls)
			}
			row := b.state("t_ghost")
			if row.Get("status") != "running" || !pyEqualInt(row.Get("current_run_id"), run) {
				t.Fatal("claim lost", row.Get("status"), row.Get("current_run_id"))
			}
		})
	}
}

func TestAForeignHostInterpreterBoundAfterTheGateKeepsItsClaim(t *testing.T) {
	for _, path := range []string{"ttl", "stale"} {
		t.Run(path, func(t *testing.T) {
			b := newOwnerBoard(t)
			run := b.deadAttempt("t_remote", deadAttempt{lockHost: "some-other-host", admission: []any{"terminal", "completed"}})
			calls := 0
			patch(t, &ownerReclaimBlocked, func(ctx context.Context, conn state.SQLiteConn, taskID string, crashSweep bool) (bool, error) {
				verdict, err := ownerReclaimBlockedLive(ctx, conn, taskID, crashSweep)
				if calls == 0 {
					b.event(taskID, run, "worker_bound", map[string]any{"pid": absentPid(3_700_000)}, b.now)
					b.store("UPDATE session_admissions SET status = 'unknown', outcome = NULL")
				}
				calls++
				return verdict, err
			})
			if ownerReclaimPaths[path](t, b.conn) {
				t.Fatal("reclaimed a remote attempt")
			}
			row := b.state("t_remote")
			if row.Get("status") != "running" || !pyEqualInt(row.Get("current_run_id"), run) {
				t.Fatal("claim lost", row.Get("status"), row.Get("current_run_id"))
			}
		})
	}
}
