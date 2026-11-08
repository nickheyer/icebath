package kanban

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	DefaultFailureLimit             = 2
	DefaultRateLimitCooldownSeconds = 300
	UnverifiedWorkerFingerprint     = "unverified"
)

// SignalFunc delivers sig to pid the way os.kill does, an error standing for the OSError reclaim absorbs
type SignalFunc func(pid int, sig syscall.Signal) error

// SpawnedTask is one task id, assignee and workspace path a dispatch tick spawned
type SpawnedTask struct{ TaskID, Assignee, WorkspacePath string }

// CappedTask is one task deferred because its assignee was at the per profile running cap
type CappedTask struct {
	TaskID, Assignee string
	Running          int
}

// GuardedTask is one task id the respawn guard skipped with its reason
type GuardedTask struct{ TaskID, Reason string }

// DispatchResult is the outcome of a single dispatch pass
type DispatchResult struct {
	Reclaimed               int
	Promoted                int
	ReconciledOrphans       []string
	ReapedTerminalWorkers   []string
	Spawned                 []SpawnedTask
	SkippedUnassigned       []string
	AutoAssignedDefault     []string
	SkippedNonspawnable     []string
	SkippedPerProfileCapped []CappedTask
	Crashed                 []string
	AutoBlocked             []string
	TimedOut                []string
	Stale                   []string
	RespawnGuarded          []GuardedTask
	RateLimited             []string
	SkippedLocked           bool
	MemoryPressure          *string
}

// Python truthiness of the result field with that dataclass name, false for an unknown name
func (r *DispatchResult) Activity(field string) bool {
	switch field {
	case "reclaimed":
		return r.Reclaimed != 0
	case "promoted":
		return r.Promoted != 0
	case "reconciled_orphans":
		return len(r.ReconciledOrphans) > 0
	case "reaped_terminal_workers":
		return len(r.ReapedTerminalWorkers) > 0
	case "spawned":
		return len(r.Spawned) > 0
	case "skipped_unassigned":
		return len(r.SkippedUnassigned) > 0
	case "auto_assigned_default":
		return len(r.AutoAssignedDefault) > 0
	case "skipped_nonspawnable":
		return len(r.SkippedNonspawnable) > 0
	case "skipped_per_profile_capped":
		return len(r.SkippedPerProfileCapped) > 0
	case "crashed":
		return len(r.Crashed) > 0
	case "auto_blocked":
		return len(r.AutoBlocked) > 0
	case "timed_out":
		return len(r.TimedOut) > 0
	case "stale":
		return len(r.Stale) > 0
	case "respawn_guarded":
		return len(r.RespawnGuarded) > 0
	case "rate_limited":
		return len(r.RateLimited) > 0
	case "skipped_locked":
		return r.SkippedLocked
	case "memory_pressure":
		return r.MemoryPressure != nil && *r.MemoryPressure != ""
	}
	return false
}

// GatewayStatusProbeSet holds the gateway status process facts worker identity checks consult
type GatewayStatusProbeSet struct {
	PidExists          func(pid int) bool
	ProcessStartTime   func(pid int) any
	StartTimesAgree    func(current any, recorded ...any) (bool, error)
	InstantiationEpoch func() string
}

// Gateway status process facts, installed by pkg/gateway at init
var GatewayStatusProbes GatewayStatusProbeSet

// Installed gateway status probes, panicking when pkg/gateway never assigned them
func gatewayStatus() GatewayStatusProbeSet {
	p := GatewayStatusProbes
	if p.PidExists == nil || p.ProcessStartTime == nil || p.StartTimesAgree == nil || p.InstantiationEpoch == nil {
		panic("kanban.GatewayStatusProbes is not installed: pkg/gateway assigns it at init")
	}
	return p
}

// Seams over liveness and the poll sleep, replaced by tests the way hermes tests monkeypatch the module
var (
	pidAlive      = pidAliveLive
	dispatchSleep = time.Sleep
)

// Integer value of an optional Python int pid, false for None or a non number
func pidOf(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	if f, ok := v.(float64); ok {
		return int(f), true
	}
	n, ok := pyint.Instance(v)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return int(n.Int64()), true
}

// True when pid runs on this host, an exited zombie awaiting its reap counting as dead
func pidAliveLive(pid any) bool {
	n, ok := pidOf(pid)
	if !ok || n <= 0 || !gatewayStatus().PidExists(n) {
		return false
	}
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", n))
		if err != nil {
			return true
		}
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if strings.HasPrefix(line, "State:") {
				_, rest, _ := strings.Cut(line, ":")
				return !strings.Contains(rest, "Z")
			}
		}
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(n)).Output()
		var exitErr *exec.ExitError
		if ctx.Err() != nil {
			return true
		}
		if errors.As(err, &exitErr) {
			return false
		}
		if err == nil && strings.Contains(strings.TrimSpace(string(out)), "Z") {
			return false
		}
	}
	return true
}

// Restart stable identity of a live process as instantiation epoch and start time, nil when unreadable
func processFingerprint(pid int) *string {
	p := gatewayStatus()
	start := p.ProcessStartTime(pid)
	if start == nil {
		return nil
	}
	s := fmt.Sprintf("%s|%v", p.InstantiationEpoch(), start)
	return &s
}

// True when pid is live and still the worker fingerprinted at spawn
func workerAlive(pid, startedAt any) bool {
	if !pidAlive(pid) {
		return false
	}
	if startedAt == UnverifiedWorkerFingerprint {
		return true
	}
	return !pidRecycled(pid, startedAt)
}

// True when a live pid is not the process fingerprinted at spawn or its fingerprint is unreadable
func pidRecycled(pid, startedAt any) bool {
	n, ok := pidOf(pid)
	if startedAt == nil || !ok || n == 0 {
		return false
	}
	if startedAt == UnverifiedWorkerFingerprint {
		return true
	}
	if s, ok := startedAt.(string); ok && strings.Contains(s, "|") {
		fp := processFingerprint(n)
		return fp == nil || *fp != s
	}
	p := gatewayStatus()
	current := p.ProcessStartTime(n)
	if current == nil {
		return true
	}
	agree, err := p.StartTimesAgree(current, startedAt)
	return err != nil || !agree
}

// The signal test hook when given, else the platform kill
func killFn(signalFn SignalFunc) SignalFunc {
	if signalFn != nil {
		return signalFn
	}
	return osKill
}

// Polls about five seconds for the worker to die, true once it is gone
func pollWorkerExit(pid int, startedAt any) bool {
	for range 10 {
		if !workerAlive(pid, startedAt) {
			return true
		}
		dispatchSleep(500 * time.Millisecond)
	}
	return false
}

// Sends the hard kill signal, true when it was delivered
func sigkill(kill SignalFunc, pid int) bool {
	return kill(pid, sigkillSignal) == nil
}

// Terminates a host local worker for a reclaim path, never signalling a recycled or unverified pid
func terminateReclaimedWorker(pid any, claimLock *string, signalFn SignalFunc, startedAt any) *pyjson.Object {
	info := payload("prev_pid", nil, "host_local", false, "termination_attempted", false, "terminated", false, "sigkill", false)
	n, ok := pidOf(pid)
	if ok && n != 0 {
		info.Set("prev_pid", n)
	}
	if !ok || n <= 0 || claimLock == nil || *claimLock == "" || !strings.HasPrefix(*claimLock, hostPrefix()) {
		return info
	}
	info.Set("host_local", true)
	kill := killFn(signalFn)
	if startedAt == UnverifiedWorkerFingerprint {
		info.Set("signal_refused", true)
		info.Set("terminated", !pidAlive(n))
		return info
	}
	if pidAlive(n) && pidRecycled(n, startedAt) {
		info.Set("terminated", true)
		info.Set("pid_recycled", true)
		return info
	}
	info.Set("termination_attempted", true)
	if err := kill(n, syscall.SIGTERM); err != nil {
		info.Set("terminated", !workerAlive(n, startedAt))
		return info
	}
	if pollWorkerExit(n, startedAt) {
		info.Set("terminated", true)
		return info
	}
	if workerAlive(n, startedAt) {
		if !sigkill(kill, n) {
			return info
		}
		info.Set("sigkill", true)
	}
	info.Set("terminated", !workerAlive(n, startedAt))
	return info
}

// True when a signalled or refused host local worker is still alive after termination
func workerSurvivedTermination(termination *pyjson.Object) bool {
	get := func(k string) bool {
		v, _ := termination.Get(k)
		return truthy(v)
	}
	return get("host_local") && (get("termination_attempted") || get("signal_refused")) && !get("terminated")
}

// Holds a claim whose worker survived termination by extending it and recording reclaim_deferred
func deferReclaimForLiveWorker(ctx context.Context, conn state.SQLiteConn, taskID string, claimLock *string, now int64, termination *pyjson.Object, reason string) error {
	grace := now + ReclaimDeferGraceSeconds
	return WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET claim_expires = ? WHERE id = ? AND status = 'running' AND claim_lock IS ?",
			grace, taskID, optStrValue(claimLock))
		if err != nil || rowsAffected(res) != 1 {
			return err
		}
		runID, err := currentRunID(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if runID != nil {
			if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET claim_expires = ? WHERE id = ?", grace, *runID); err != nil {
				return err
			}
		}
		body := payload("reason", reason, "claim_lock", optStrValue(claimLock), "claim_expires_now", grace)
		body.Update(termination)
		return appendEvent(ctx, conn, taskID, "reclaim_deferred", body, runID)
	})
}

// Resets the consecutive failure counter and its last error
func clearFailureCounter(ctx context.Context, conn state.SQLiteConn, taskID string) error {
	return WriteTxn(ctx, conn, false, func() error {
		_, err := conn.ExecContext(ctx, "UPDATE tasks SET consecutive_failures = 0, last_failure_error = NULL WHERE id = ?", taskID)
		return err
	})
}

const (
	WorkerOutputSep        = " Worker's last output: "
	protocolViolationError = "worker exited cleanly (rc=0) without kanban_complete, kanban_block " +
		"or kanban_request_review — protocol violation. " +
		"If the prior run already did the work, verify it and " +
		"report it via kanban_complete (or kanban_request_review); " +
		"a run without a terminal kanban call counts as failed no " +
		"matter what it did."
)

// deadWorker is how the crash sweep books one dead worker
type deadWorker struct {
	Kind              string
	Code              *int
	ErrorText         string
	EventKind         string
	EventPayload      *pyjson.Object
	ProtocolViolation bool
	RateLimited       bool
	TerminalProvider  bool
}

// Run outcome of the booking, rate_limited for a quota wall requeue and crashed otherwise
func (d *deadWorker) runOutcome() string {
	if d.RateLimited {
		return "rate_limited"
	}
	return "crashed"
}

// Terminates host local workers past their per task max runtime and books each as timed_out
func EnforceMaxRuntime(ctx context.Context, conn state.SQLiteConn, signalFn SignalFunc) ([]string, error) {
	timedOut := []string{}
	now := time.Now().Unix()
	prefix := hostPrefix()
	rows, err := QueryRows(ctx, conn, "SELECT t.id, t.worker_pid, t.worker_started_at, "+
		"       COALESCE(r.started_at, t.started_at) AS active_started_at, "+
		"       t.max_runtime_seconds, t.claim_lock "+
		"FROM tasks t "+
		"LEFT JOIN task_runs r ON r.id = t.current_run_id "+
		"WHERE t.status = 'running' AND t.max_runtime_seconds IS NOT NULL "+
		"  AND COALESCE(r.started_at, t.started_at) IS NOT NULL "+
		"  AND t.worker_pid IS NOT NULL")
	if err != nil {
		return timedOut, err
	}
	for _, row := range rows {
		tid := cellStr(row.Get("id"))
		blocked, err := ownerReclaimBlocked(ctx, conn, tid, false)
		if err != nil {
			return timedOut, err
		}
		lock, _ := row.Get("claim_lock").(string)
		if blocked || !strings.HasPrefix(lock, prefix) {
			continue
		}
		started := now
		if te := toEpoch(row.Get("active_started_at")); te != nil && *te != 0 {
			started = *te
		}
		elapsed := now - started
		limit, err := pyIntOf(row.Get("max_runtime_seconds"))
		if err != nil {
			return timedOut, err
		}
		if elapsed < limit {
			continue
		}
		pid64, err := pyIntOf(row.Get("worker_pid"))
		if err != nil {
			return timedOut, err
		}
		pid := int(pid64)
		startedAt := rowGet(row, "worker_started_at", nil)
		if startedAt == UnverifiedWorkerFingerprint && pidAlive(pid) {
			log.Warn(fmt.Sprintf("kanban: task %s worker pid %d exceeded max runtime but has no verified identity; not signalled", tid, pid))
			continue
		}
		killed := false
		kill := killFn(signalFn)
		if !(pidAlive(pid) && pidRecycled(pid, startedAt)) {
			_ = kill(pid, syscall.SIGTERM)
			pollWorkerExit(pid, startedAt)
			if workerAlive(pid, startedAt) {
				killed = sigkill(kill, pid)
			}
		}
		errText := fmt.Sprintf("elapsed %ds > limit %ds", elapsed, limit)
		var retryStatus string
		updated := false
		err = WriteTxn(ctx, conn, false, func() error {
			skip, err := ownerReclaimBlockedAfterKill(ctx, conn, tid, true)
			if err != nil || skip {
				return err
			}
			if retryStatus, err = retryStatusForRun(ctx, conn, tid, nil); err != nil {
				return err
			}
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, claim_lock = NULL, "+
				"claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL, "+
				"last_heartbeat_at = NULL "+
				"WHERE id = ? AND status = 'running' "+
				"  AND worker_pid = ? AND claim_lock IS ?", retryStatus, tid, pid, row.Get("claim_lock"))
			if err != nil || rowsAffected(res) != 1 {
				return err
			}
			updated = true
			body := payload("pid", pid, "elapsed_seconds", elapsed, "limit_seconds", limit, "sigkill", killed, "retry_status", retryStatus)
			runID, err := endRun(ctx, conn, tid, "timed_out", nil, &errText, body, "timed_out")
			if err != nil {
				return err
			}
			if err := appendEvent(ctx, conn, tid, "timed_out", body, runID); err != nil {
				return err
			}
			timedOut = append(timedOut, tid)
			return nil
		})
		if err != nil {
			return timedOut, err
		}
		if updated {
			if _, err := recordTaskFailure(ctx, conn, tid, errText, RecordFailureOptions{Outcome: "timed_out",
				EventPayloadExtra: payload("pid", pid, "sigkill", killed, "retry_status", retryStatus)}); err != nil {
				return timedOut, err
			}
		}
	}
	return timedOut, nil
}

// Fingerprint of pid, the unverified marker when it cannot be captured
func spawnFingerprint(pid int) string {
	if fp := processFingerprint(pid); fp != nil && *fp != "" {
		return *fp
	}
	return UnverifiedWorkerFingerprint
}

// Publishes the launcher pid and fingerprint while the claim has no executing worker yet
func setWorkerPid(ctx context.Context, conn state.SQLiteConn, taskID string, pid int, runID *int64, claimLock *string) error {
	startedAt := spawnFingerprint(pid)
	return WriteTxn(ctx, conn, false, func() error {
		if runID == nil {
			var err error
			if runID, err = currentRunID(ctx, conn, taskID); err != nil {
				return err
			}
		}
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET worker_pid = ?, worker_started_at = ? WHERE id = ? AND status = 'running' "+
			"AND current_run_id IS ? AND worker_pid IS NULL "+
			"AND (? IS NULL OR claim_lock = ?)", pid, startedAt, taskID, optIntValue(runID), optStrValue(claimLock), optStrValue(claimLock))
		if err != nil {
			return err
		}
		if rowsAffected(res) != 0 {
			if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET worker_pid = ?, worker_started_at = ? WHERE id = ?", pid, startedAt, optIntValue(runID)); err != nil {
				return err
			}
		}
		return appendEvent(ctx, conn, taskID, "spawned", payload("pid", pid, "started_at", startedAt), runID)
	})
}

// Worker side pid registration before the first model call, false when the run is no longer live
func AdoptWorkerPid(ctx context.Context, conn state.SQLiteConn, taskID string, runID int64, pid int) (bool, error) {
	startedAt := spawnFingerprint(pid)
	live := false
	err := WriteTxn(ctx, conn, false, func() error {
		row, err := QueryOne(ctx, conn, "SELECT status, current_run_id, worker_pid, claim_lock FROM tasks WHERE id = ?", taskID)
		if err != nil || row == nil || row.Get("status") != "running" || !pyEqualInt(row.Get("current_run_id"), runID) {
			return err
		}
		live = true
		lock, _ := row.Get("claim_lock").(string)
		if row.Get("worker_pid") != nil || !strings.HasPrefix(lock, hostPrefix()) {
			return nil
		}
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET worker_pid = ?, worker_started_at = ? WHERE id = ?", pid, startedAt, taskID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET worker_pid = ?, worker_started_at = ? WHERE id = ?", pid, startedAt, runID); err != nil {
			return err
		}
		return appendEvent(ctx, conn, taskID, "worker_registered", payload("pid", pid, "started_at", startedAt), &runID)
	})
	if err != nil {
		return false, err
	}
	return live, nil
}

// Python equality of a column value with an integer
func pyEqualInt(v any, n int64) bool {
	m, ok := pyint.Instance(v)
	return ok && m.IsInt64() && m.Int64() == n
}

const staleHeartbeatGapSeconds = 3600

// Reclaims running tasks with no heartbeat progress past the timeout, zero disabling the check
func DetectStaleRunning(ctx context.Context, conn state.SQLiteConn, staleTimeoutSeconds int64, signalFn SignalFunc) ([]string, error) {
	reclaimed := []string{}
	if staleTimeoutSeconds <= 0 {
		return reclaimed, nil
	}
	now := time.Now().Unix()
	rows, err := QueryRows(ctx, conn, "SELECT t.id, t.worker_pid, t.worker_started_at, t.last_heartbeat_at, t.claim_lock, "+
		"       COALESCE(r.started_at, t.started_at) AS active_started_at "+
		"FROM tasks t "+
		"LEFT JOIN task_runs r ON r.id = t.current_run_id "+
		"WHERE t.status = 'running'")
	if err != nil {
		return reclaimed, err
	}
	for _, row := range rows {
		tid := cellStr(row.Get("id"))
		blocked, err := ownerReclaimBlocked(ctx, conn, tid, false)
		if err != nil {
			return reclaimed, err
		}
		if blocked || row.Get("active_started_at") == nil {
			continue
		}
		started := now
		if te := toEpoch(row.Get("active_started_at")); te != nil && *te != 0 {
			started = *te
		}
		elapsed := now - started
		if elapsed < staleTimeoutSeconds {
			continue
		}
		lastHB := toEpoch(row.Get("last_heartbeat_at"))
		var hbAge *int64
		if lastHB != nil {
			hbAge = i64ptr(now - *lastHB)
		}
		if hbAge != nil && *hbAge < staleHeartbeatGapSeconds {
			continue
		}
		pid := row.Get("worker_pid")
		lock, _ := row.Get("claim_lock").(string)
		termination := terminateReclaimedWorker(pid, &lock, signalFn, rowGet(row, "worker_started_at", nil))
		if workerSurvivedTermination(termination) {
			if err := deferReclaimForLiveWorker(ctx, conn, tid, &lock, now, termination, "heartbeat_stale_worker_alive"); err != nil {
				return reclaimed, err
			}
			continue
		}
		err = WriteTxn(ctx, conn, false, func() error {
			attempted, _ := termination.Get("termination_attempted")
			skip, err := ownerReclaimBlockedAfterKill(ctx, conn, tid, truthy(attempted))
			if err != nil || skip {
				return err
			}
			retryStatus, err := retryStatusForRun(ctx, conn, tid, nil)
			if err != nil {
				return err
			}
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, claim_lock = NULL, "+
				"claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL, "+
				"last_heartbeat_at = NULL "+
				"WHERE id = ? AND status = 'running' "+
				"  AND claim_lock IS ?", retryStatus, tid, row.Get("claim_lock"))
			if err != nil || rowsAffected(res) != 1 {
				return err
			}
			var pidValue any
			if n, ok := pidOf(pid); ok && n != 0 {
				pidValue = n
			}
			body := payload("elapsed_seconds", elapsed, "last_heartbeat_at", optIntValue(lastHB), "heartbeat_age_seconds", optIntValue(hbAge),
				"timeout_seconds", staleTimeoutSeconds, "pid", pidValue, "retry_status", retryStatus)
			body.Update(termination)
			errText := "no heartbeat ever"
			if hbAge != nil {
				errText = fmt.Sprintf("no heartbeat for %ds ", *hbAge)
			}
			errText += fmt.Sprintf(" after %ds running", elapsed)
			runID, err := endRun(ctx, conn, tid, "stale", nil, &errText, body, "stale")
			if err != nil {
				return err
			}
			if err := appendEvent(ctx, conn, tid, "stale", body, runID); err != nil {
				return err
			}
			reclaimed = append(reclaimed, tid)
			return nil
		})
		if err != nil {
			return reclaimed, err
		}
	}
	return reclaimed, nil
}

// Pointer to a copy of n
func i64ptr(n int64) *int64 { return &n }
