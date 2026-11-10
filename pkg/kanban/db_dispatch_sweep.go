package kanban

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	TerminalWorkerReapGraceSeconds = 120
	recentWorkerExitTTLSeconds     = 600
	recentWorkerExitsMax           = 4096
	protocolViolationFailureLimit  = 3
	protocolViolationScanLimit     = 50
)

// WorkerExitTrailer prefixes the exit code line a worker CLI appends to its own log
const WorkerExitTrailer = "[kanban-worker-exit] rc="

// One line naming why the ticks held ready work back, empty when nothing was held
func DescribeSuppression(results []*DispatchResult) string {
	counts := map[string]int{}
	pressure := ""
	for _, res := range results {
		if res == nil {
			continue
		}
		for _, g := range res.RespawnGuarded {
			counts[g.Reason]++
		}
		if len(res.RateLimited) > 0 {
			counts["rate_limited"] += len(res.RateLimited)
		}
		if res.SkippedLocked {
			counts["skipped_locked"]++
		}
		if res.MemoryPressure != nil && *res.MemoryPressure != "" {
			pressure = *res.MemoryPressure
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	if pressure != "" {
		parts = append(parts, "memory_pressure="+pressure)
	}
	return strings.Join(parts, ", ")
}

// workerExit is one reaped child exit status with the time it was recorded
type workerExit struct {
	raw      int
	reapedAt float64
}

// liveWorkerProc is a spawned worker whose exit a waiting goroutine publishes
type liveWorkerProc struct {
	done      chan struct{}
	rawStatus int
}

var (
	workerExitMu      sync.Mutex
	recentWorkerExits = map[int]workerExit{}
	liveWorkerProcs   = map[int]*liveWorkerProc{}
)

// Wait status layout of a plain return code, the exit code in bits 8 to 15
func waitStatusFromReturncode(returncode int) int {
	return (returncode & 0xFF) << 8
}

// Records a reaped child exit status with the latest write winning and the registry bounded by age and size
func recordWorkerExit(pid, rawStatus int) {
	if pid <= 0 {
		return
	}
	workerExitMu.Lock()
	defer workerExitMu.Unlock()
	now := float64(time.Now().UnixNano()) / 1e9
	recentWorkerExits[pid] = workerExit{rawStatus, now}
	if len(recentWorkerExits) > recentWorkerExitsMax/2 {
		cutoff := now - recentWorkerExitTTLSeconds
		for p, e := range recentWorkerExits {
			if e.reapedAt < cutoff {
				delete(recentWorkerExits, p)
			}
		}
	}
	if len(recentWorkerExits) > recentWorkerExitsMax {
		type entry struct {
			pid int
			at  float64
		}
		ordered := make([]entry, 0, len(recentWorkerExits))
		for p, e := range recentWorkerExits {
			ordered = append(ordered, entry{p, e.reapedAt})
		}
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].at < ordered[j].at })
		for _, e := range ordered[:len(ordered)/2] {
			delete(recentWorkerExits, e.pid)
		}
	}
}

// Exit kind and code of a reaped worker pid, unknown when the registry never saw it
func classifyWorkerExit(pid int) (string, *int) {
	workerExitMu.Lock()
	entry, ok := recentWorkerExits[pid]
	workerExitMu.Unlock()
	if !ok {
		return "unknown", nil
	}
	raw := entry.raw
	signalNumber := raw & 0x7F
	if signalNumber == 0 {
		kind, code := exitCodeKind((raw >> 8) & 0xFF)
		return kind, &code
	}
	if signalNumber != 0x7F {
		return "signaled", &signalNumber
	}
	return "unknown", nil
}

// Exit kind of a worker exit code however it was observed
func exitCodeKind(code int) (string, int) {
	switch code {
	case 0:
		return "clean_exit", 0
	case KanbanRateLimitExitCode:
		return "rate_limited", code
	case KanbanTerminalProviderExitCode:
		return "terminal_provider", code
	}
	return "nonzero_exit", code
}

var exitTrailerRE = regexp2.MustCompile("(?m)^"+regexp2.Escape(WorkerExitTrailer)+`(\d+)\s*$`, regexp2.None)

// Exit code from the trailer the worker CLI wrote to its own log, nil when absent
func workerLogExitCode(ctx context.Context, taskID string, board *string) *int {
	raw, err := ReadWorkerLog(ctx, taskID, i64ptr(4000), board)
	if err != nil {
		return nil
	}
	text := ""
	if raw != nil {
		text = *raw
	}
	var last *int
	for m, err := exitTrailerRE.FindStringMatch(text); m != nil && err == nil; m, err = exitTrailerRE.FindNextMatch(m) {
		n, perr := pyIntOf(m.GroupByNumber(1).String())
		if perr != nil {
			return nil
		}
		code := int(n)
		last = &code
	}
	return last
}

// Publishes a spawned worker so the reap loop learns its exit status when it ends
func registerLiveWorker(pid int, cmd *exec.Cmd) {
	proc := &liveWorkerProc{done: make(chan struct{})}
	workerExitMu.Lock()
	liveWorkerProcs[pid] = proc
	workerExitMu.Unlock()
	go func() {
		err := cmd.Wait()
		proc.rawStatus = rawWaitStatus(cmd.ProcessState, err)
		close(proc.done)
	}()
}

// Records every spawned worker that has exited since the last call and returns their pids
func reapWorkerZombies() []int {
	workerExitMu.Lock()
	pids := make([]int, 0, len(liveWorkerProcs))
	for pid := range liveWorkerProcs {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	workerExitMu.Unlock()
	var reaped []int
	for _, pid := range pids {
		workerExitMu.Lock()
		proc := liveWorkerProcs[pid]
		workerExitMu.Unlock()
		if proc == nil {
			continue
		}
		select {
		case <-proc.done:
		default:
			continue
		}
		recordWorkerExit(pid, proc.rawStatus)
		workerExitMu.Lock()
		delete(liveWorkerProcs, pid)
		workerExitMu.Unlock()
		reaped = append(reaped, pid)
	}
	return reaped
}

// Ends host local workers that outlived their closed run, returning the task ids whose worker was terminated
func ReapTerminalWorkers(ctx context.Context, conn state.SQLiteConn, signalFn SignalFunc) ([]string, error) {
	rows, err := QueryRows(ctx, conn, "SELECT id, task_id, worker_pid, worker_started_at, claim_lock FROM task_runs "+
		"WHERE ended_at IS NOT NULL AND ended_at <= ? "+
		"AND worker_pid IS NOT NULL AND worker_started_at IS NOT NULL", time.Now().Unix()-TerminalWorkerReapGraceSeconds)
	if err != nil {
		return nil, err
	}
	prefix := hostPrefix()
	var reaped []string
	for _, row := range rows {
		if err := reapTerminalWorkerRow(ctx, conn, row, prefix, signalFn, &reaped); err != nil {
			log.Debug(fmt.Sprintf("kanban dispatch: terminal worker reap failed for run %s (task %s): %s", cellStr(row.Get("id")), cellStr(row.Get("task_id")), excText(err)))
		}
	}
	return reaped, nil
}

// Clears or terminates the retained worker of one closed run row
func reapTerminalWorkerRow(ctx context.Context, conn state.SQLiteConn, row *Row, prefix string, signalFn SignalFunc, reaped *[]string) error {
	pid64, err := pyIntOf(row.Get("worker_pid"))
	if err != nil {
		return err
	}
	pid := int(pid64)
	fingerprint := row.Get("worker_started_at")
	if pid == os.Getpid() || !strings.HasPrefix(cellStr(row.Get("claim_lock")), prefix) {
		return nil
	}
	if fingerprint == UnverifiedWorkerFingerprint && pidAlive(pid) {
		return nil
	}
	alive := workerAlive(pid, fingerprint)
	var termination *pyjson.Object
	if alive {
		termination = terminateReclaimedWorker(pid, cellOptStr(row.Get("claim_lock")), signalFn, fingerprint)
		if terminated, _ := termination.Get("terminated"); !truthy(terminated) {
			return nil
		}
	}
	runID := row.Get("id")
	taskID := cellStr(row.Get("task_id"))
	err = WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET worker_pid = NULL, worker_started_at = NULL "+
			"WHERE id = ? AND worker_pid = ? AND worker_started_at = ?", runID, pid, fingerprint); err != nil {
			return err
		}
		if !alive {
			return nil
		}
		body := payload("pid", pid, "worker_started_at", fingerprint)
		body.Update(termination)
		return appendEvent(ctx, conn, taskID, "terminal_worker_reaped", body, cellOptInt(runID))
	})
	if err != nil {
		return err
	}
	if alive {
		*reaped = append(*reaped, taskID)
	}
	return nil
}

// Records a heartbeat event and touches last_heartbeat_at, false when the task is not running under that run
func HeartbeatWorker(ctx context.Context, conn state.SQLiteConn, taskID string, note *string, expectedRunID *int64) (bool, error) {
	return HeartbeatWorkerNote(ctx, conn, taskID, optStrValue(note), expectedRunID)
}

// Heartbeat whose note is any JSON value, stored on the event only when truthy
func HeartbeatWorkerNote(ctx context.Context, conn state.SQLiteConn, taskID string, note any, expectedRunID *int64) (bool, error) {
	now := time.Now().Unix()
	beat := false
	err := WriteTxn(ctx, conn, false, func() error {
		sql := "UPDATE tasks SET last_heartbeat_at = ? WHERE id = ? AND status = 'running'"
		params := []any{now, taskID}
		if expectedRunID != nil {
			if err := runIDBindable(expectedRunID); err != nil {
				return err
			}
			sql += " AND current_run_id = ?"
			params = append(params, *expectedRunID)
		}
		res, err := conn.ExecContext(ctx, sql, params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		runID := expectedRunID
		if runID == nil {
			if runID, err = currentRunID(ctx, conn, taskID); err != nil {
				return err
			}
		}
		if runID != nil {
			if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET last_heartbeat_at = ? WHERE id = ?", now, *runID); err != nil {
				return err
			}
		}
		var body any
		if pyrepr.Truthy(note) {
			body = payload("note", note)
		}
		beat = true
		return appendEvent(ctx, conn, taskID, "heartbeat", body, runID)
	})
	return beat && err == nil, err
}

// Requeues running cards with broken claim bookkeeping and no live local worker, returning their ids
func ReconcileOrphanedRunning(ctx context.Context, conn state.SQLiteConn) ([]string, error) {
	now := time.Now().Unix()
	var reconciled []string
	rows, err := QueryRows(ctx, conn, "SELECT id, claim_lock, claim_expires, worker_pid, worker_started_at FROM tasks "+
		"WHERE status = 'running' "+
		"  AND (claim_lock IS NULL OR claim_expires IS NULL)")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		tid := cellStr(row.Get("id"))
		blocked, err := ownerReclaimBlocked(ctx, conn, tid, false)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		pid := row.Get("worker_pid")
		if truthy(pid) && workerAlive(pid, rowGet(row, "worker_started_at", nil)) {
			log.Debug(fmt.Sprintf("kanban reconcile: task %s has broken claim bookkeeping but pid %v is alive on this host — deferring", tid, pid))
			continue
		}
		requeued := false
		err = WriteTxn(ctx, conn, false, func() error {
			blocked, err := ownerReclaimBlocked(ctx, conn, tid, false)
			if err != nil || blocked {
				return err
			}
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready', claim_lock = NULL, "+
				"claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL, "+
				"last_heartbeat_at = NULL "+
				"WHERE id = ? AND status = 'running' "+
				"  AND claim_lock IS ? AND claim_expires IS ?", tid, row.Get("claim_lock"), row.Get("claim_expires"))
			if err != nil {
				return err
			}
			if rowsAffected(res) != 1 {
				return nil
			}
			var workerPid any
			if truthy(pid) {
				n, err := pyIntOf(pid)
				if err != nil {
					return err
				}
				workerPid = n
			}
			body := payload("reason", "orphaned_running", "claim_lock", row.Get("claim_lock"), "claim_expires", optIntValue(toEpoch(row.Get("claim_expires"))), "worker_pid", workerPid, "now", now)
			errText := "orphaned running card (broken claim bookkeeping)"
			runID, err := endRun(ctx, conn, tid, "reclaimed", nil, &errText, body, "reclaimed")
			if err != nil {
				return err
			}
			if err := insertComment(ctx, conn, tid, "dispatcher", "reconciliation: card was 'running' with no valid claim "+
				"(dead/gone worker) — requeued to ready", now); err != nil {
				return err
			}
			if err := appendEvent(ctx, conn, tid, "reconciled", body, runID); err != nil {
				return err
			}
			requeued = true
			return nil
		})
		if err != nil {
			return nil, err
		}
		if requeued {
			reconciled = append(reconciled, tid)
		}
		log.Info(fmt.Sprintf("kanban reconcile: requeued orphaned running task %s (claim_lock=%s, worker_pid=%s)", tid, pyrepr.Repr(row.Get("claim_lock")), pyrepr.Repr(pid)))
	}
	return reconciled, nil
}

var (
	fingerprintPidRE = regexp2.MustCompile(`\bpid \d+\b`, regexp2.None)
	fingerprintTSRE  = regexp2.MustCompile(`\b\d{10,}\b`, regexp2.None)
)

// Error message with pids and timestamps normalized so same root cause errors group
func errorFingerprint(errorText string) string {
	head := errorText
	if rs := []rune(head); len(rs) > 80 {
		head = string(rs[:80])
	}
	fp, _ := fingerprintPidRE.Replace(head, "pid N", -1, -1)
	fp, _ = fingerprintTSRE.Replace(fp, "<TS>", -1, -1)
	return pystr.Strip(pystr.Lower(fp))
}

// Trailing run of clean exit protocol violations on a task, rate limited runs skipped
func protocolViolationStreak(ctx context.Context, conn state.SQLiteConn, taskID string) (int, error) {
	rows, err := QueryRows(ctx, conn, "SELECT outcome, error, metadata FROM task_runs "+
		"WHERE task_id = ? AND ended_at IS NOT NULL "+
		"ORDER BY id DESC LIMIT ?", taskID, protocolViolationScanLimit)
	if err != nil {
		return 0, err
	}
	streak := 0
	for _, row := range rows {
		outcome := cellStr(row.Get("outcome"))
		if outcome == "rate_limited" {
			continue
		}
		head, _, _ := strings.Cut(cellStr(row.Get("error")), WorkerOutputSep)
		marker, _ := jsonDict(row.Get("metadata")).Get("protocol_violation")
		if outcome == "crashed" && (pyTruthy(marker) || strings.Contains(head, "protocol violation")) {
			streak++
			continue
		}
		break
	}
	return streak, nil
}

var logChromeRE = regexp2.MustCompile(`[─━═╭╮╰╯│┃┌┐└┘]+|☤\s*Hermes`, regexp2.None)

// The CLI exit summary header in the active language
func exitSummaryMarker(ctx context.Context) string {
	return i18n.T(ctx, "cli.session.exit_resume_hint", "", nil)
}

// Prefixes of CLI preamble lines the worker output diagnostic drops
func logNoisePrefixes(ctx context.Context) []string {
	return []string{"session_id:", "Query:", i18n.T(ctx, "cli.chat.initializing_agent", "", nil)}
}

// Last printed text of a dead worker from its log with CLI chrome trimmed, empty when there is none
func workerFinalOutput(ctx context.Context, taskID string, board *string) string {
	raw, err := ReadWorkerLog(ctx, taskID, i64ptr(4000), board)
	if err != nil || raw == nil || *raw == "" {
		return ""
	}
	text, _ := exitTrailerRE.Replace(*raw, "", -1, -1)
	if cut := strings.LastIndex(text, exitSummaryMarker(ctx)); cut != -1 {
		text = text[:cut]
	}
	prefixes := logNoisePrefixes(ctx)
	var lines []string
	for _, ln := range pystr.Splitlines(text, false) {
		stripped, _ := logChromeRE.Replace(ln, "", -1, -1)
		stripped = pystr.Strip(stripped)
		if stripped == "" {
			continue
		}
		noise := false
		for _, p := range prefixes {
			if strings.HasPrefix(stripped, p) {
				noise = true
				break
			}
		}
		if !noise {
			lines = append(lines, stripped)
		}
	}
	joined := []rune(strings.Join(lines, " "))
	if len(joined) > 400 {
		joined = joined[len(joined)-400:]
	}
	return string(joined)
}

// Reclaim bookkeeping of a dead worker with its own last output folded into the error and payload
func classifyDeadWorker(ctx context.Context, pid int, claimer *string, exitCode *int64, taskID *string, board *string) *deadWorker {
	dead := classifyDeadWorkerExit(ctx, pid, claimer, exitCode, taskID, board)
	if taskID != nil && *taskID != "" && !dead.RateLimited {
		if output := workerFinalOutput(ctx, *taskID, board); output != "" {
			dead.ErrorText += WorkerOutputSep + pyrepr.Repr(output)
			dead.EventPayload.Set("worker_output", output)
		}
	}
	return dead
}

// Exit status of a dead worker mapped to its reclaim bookkeeping before its own words are folded in
func classifyDeadWorkerExit(ctx context.Context, pid int, claimer *string, exitCode *int64, taskID *string, board *string) *deadWorker {
	kind, code := classifyWorkerExit(pid)
	if exitCode != nil {
		k, c := exitCodeKind(int(*exitCode))
		kind, code = k, &c
	} else if kind == "unknown" && taskID != nil && *taskID != "" {
		if logged := workerLogExitCode(ctx, *taskID, board); logged != nil {
			k, c := exitCodeKind(*logged)
			kind, code = k, &c
		}
	}
	var codeValue any
	if code != nil {
		codeValue = int64(*code)
	}
	switch kind {
	case "clean_exit":
		return &deadWorker{Kind: kind, Code: code, ErrorText: protocolViolationError, EventKind: "protocol_violation",
			EventPayload: payload("pid", pid, "claimer", optStrValue(claimer), "exit_code", codeValue, "protocol_violation", true), ProtocolViolation: true}
	case "rate_limited":
		return &deadWorker{Kind: kind, Code: code, ErrorText: fmt.Sprintf("pid %d exited rate-limited (quota wall) — requeued without counting a failure", pid),
			EventKind: "rate_limited", EventPayload: payload("pid", pid, "claimer", optStrValue(claimer), "exit_code", codeValue), RateLimited: true}
	case "terminal_provider":
		return &deadWorker{Kind: kind, Code: code, ErrorText: fmt.Sprintf("pid %d exited on a terminal provider error (exit %d): the provider rejected "+
			"this profile's credential or model — fix the configuration, then unblock.", pid, *code),
			EventKind: "crashed", EventPayload: payload("pid", pid, "claimer", optStrValue(claimer), "exit_kind", kind, "exit_code", codeValue, "terminal_provider", true), TerminalProvider: true}
	}
	errorText := fmt.Sprintf("pid %d not alive", pid)
	if kind == "nonzero_exit" {
		errorText = fmt.Sprintf("pid %d exited with code %d", pid, *code)
	} else if kind == "signaled" {
		errorText = fmt.Sprintf("pid %d killed by signal %d", pid, *code)
	}
	body := payload("pid", pid, "claimer", optStrValue(claimer))
	if code != nil && kind != "unknown" {
		body.Set("exit_kind", kind)
		body.Set("exit_code", codeValue)
	}
	return &deadWorker{Kind: kind, Code: code, ErrorText: errorText, EventKind: "crashed", EventPayload: body}
}

// crashDetail is one dead worker awaiting breaker accounting after the reclaim transaction
type crashDetail struct {
	TaskID  string
	Pid     int
	Claimer *string
	Dead    *deadWorker
}

// crashSweep is everything the crash sweep collects inside its reclaim transaction
type crashSweep struct {
	Crashed            []string
	RateLimited        []string
	CrashDetails       []crashDetail
	ExitedHookPayloads []map[string]any
}

// Releases every host local running task whose worker pid is dead
func reclaimDeadWorkers(ctx context.Context, conn state.SQLiteConn, board *string) (*crashSweep, error) {
	sweep := &crashSweep{}
	err := WriteTxn(ctx, conn, false, func() error {
		rows, err := QueryRows(ctx, conn, "SELECT id, worker_pid, worker_started_at, claim_lock, started_at, assignee "+
			"FROM tasks "+
			"WHERE status = 'running' AND worker_pid IS NOT NULL")
		if err != nil {
			return err
		}
		prefix := hostPrefix()
		for _, row := range rows {
			tid := cellStr(row.Get("id"))
			lock := cellStr(row.Get("claim_lock"))
			if !strings.HasPrefix(lock, prefix) {
				continue
			}
			if startedAt := toEpoch(rowGet(row, "started_at", nil)); startedAt != nil && float64(time.Now().UnixNano())/1e9-float64(*startedAt) < float64(resolveCrashGraceSeconds()) {
				continue
			}
			if workerAlive(row.Get("worker_pid"), rowGet(row, "worker_started_at", nil)) {
				continue
			}
			blocked, err := ownerReclaimBlocked(ctx, conn, tid, true)
			if err != nil {
				return err
			}
			if blocked {
				continue
			}
			pid64, err := pyIntOf(row.Get("worker_pid"))
			if err != nil {
				return err
			}
			pid := int(pid64)
			claimLock := cellOptStr(row.Get("claim_lock"))
			exitRaw, err := managedRunExitCode(ctx, conn, tid, pid, row.Get("claim_lock"))
			if err != nil {
				return err
			}
			var exitCode *int64
			if exitRaw != nil {
				n, err := pyIntOf(exitRaw)
				if err != nil {
					return err
				}
				exitCode = &n
			}
			dead := classifyDeadWorker(ctx, pid, claimLock, exitCode, &tid, board)
			if err := applyAttemptNote(ctx, conn, tid, pid, dead); err != nil {
				return err
			}
			retryStatus, err := retryStatusForRun(ctx, conn, tid, nil)
			if err != nil {
				return err
			}
			dead.EventPayload.Set("retry_status", retryStatus)
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, claim_lock = NULL, "+
				"claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL "+
				"WHERE id = ? AND status = 'running' "+
				"  AND worker_pid = ? AND claim_lock IS ?", retryStatus, tid, pid, row.Get("claim_lock"))
			if err != nil {
				return err
			}
			if rowsAffected(res) != 1 {
				continue
			}
			outcome := dead.runOutcome()
			runID, err := endRun(ctx, conn, tid, outcome, nil, &dead.ErrorText, dead.EventPayload.Copy(), outcome)
			if err != nil {
				return err
			}
			if err := appendEvent(ctx, conn, tid, dead.EventKind, dead.EventPayload, runID); err != nil {
				return err
			}
			var codeValue any
			if dead.Code != nil {
				codeValue = int64(*dead.Code)
			}
			sweep.ExitedHookPayloads = append(sweep.ExitedHookPayloads, map[string]any{
				"task_id": tid, "assignee": row.Get("assignee"), "run_id": optIntValue(runID), "worker_pid": pid,
				"exit_kind": dead.Kind, "exit_code": codeValue, "outcome": outcome, "retry_status": retryStatus,
			})
			if dead.RateLimited || dead.ProtocolViolation {
				stamped := dead.ErrorText
				if rs := []rune(stamped); len(rs) > 500 {
					stamped = string(rs[:500])
				}
				if _, err := conn.ExecContext(ctx, "UPDATE tasks SET last_failure_error = ? WHERE id = ?", stamped, tid); err != nil {
					return err
				}
			}
			if dead.RateLimited {
				sweep.RateLimited = append(sweep.RateLimited, tid)
			} else {
				sweep.Crashed = append(sweep.Crashed, tid)
				sweep.CrashDetails = append(sweep.CrashDetails, crashDetail{tid, pid, claimLock, dead})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sweep, nil
}

// Counts each crash against the breaker and returns the task ids it tripped
func accountCrashes(ctx context.Context, conn state.SQLiteConn, details []crashDetail) ([]string, error) {
	var autoBlocked []string
	fpCounts := map[string]int{}
	for _, d := range details {
		fpCounts[errorFingerprint(d.Dead.ErrorText)]++
	}
	for _, d := range details {
		errorText := d.Dead.ErrorText
		var tripped bool
		var err error
		switch {
		case d.Dead.ProtocolViolation:
			streak, err := protocolViolationStreak(ctx, conn, d.TaskID)
			if err != nil {
				return nil, err
			}
			trow, err := QueryOne(ctx, conn, "SELECT max_retries FROM tasks WHERE id = ?", d.TaskID)
			if err != nil {
				return nil, err
			}
			if trow == nil {
				continue
			}
			violationLimit := int64(protocolViolationFailureLimit)
			if override := rowGet(trow, "max_retries", nil); override != nil {
				if violationLimit, err = pyIntOf(override); err != nil {
					return nil, err
				}
			}
			if int64(streak) < violationLimit {
				continue
			}
			tripped, err = recordTaskFailure(ctx, conn, d.TaskID, errorText, RecordFailureOptions{Outcome: "crashed", FailureLimit: &violationLimit, ForceTrip: true,
				EventPayloadExtra: payload("pid", d.Pid, "claimer", optStrValue(d.Claimer), "protocol_violations", streak, "protocol_violation_limit", violationLimit)})
			if err != nil {
				return nil, err
			}
		case d.Dead.TerminalProvider:
			tripped, err = recordTaskFailure(ctx, conn, d.TaskID, errorText, RecordFailureOptions{Outcome: "crashed", ForceTrip: true,
				EventPayloadExtra: payload("pid", d.Pid, "claimer", optStrValue(d.Claimer), "terminal_provider", true)})
			if err != nil {
				return nil, err
			}
		default:
			systemic := fpCounts[errorFingerprint(errorText)] >= 3
			extra := payload("pid", d.Pid, "claimer", optStrValue(d.Claimer))
			var limit *int64
			if systemic {
				extra.Set("sticky", true)
				limit = i64ptr(1)
			}
			tripped, err = recordTaskFailure(ctx, conn, d.TaskID, errorText, RecordFailureOptions{Outcome: "crashed", FailureLimit: limit, EventPayloadExtra: extra})
			if err != nil {
				return nil, err
			}
		}
		if tripped {
			autoBlocked = append(autoBlocked, d.TaskID)
		}
	}
	return autoBlocked, nil
}

// CrashSweepOutcome is what one crash sweep reclaimed, auto blocked and requeued after a quota wall
type CrashSweepOutcome struct {
	Crashed     []string
	AutoBlocked []string
	RateLimited []string
}

// Reclaims running tasks whose host local worker pid is dead and books each death
func DetectCrashedWorkers(ctx context.Context, conn state.SQLiteConn, board *string) ([]string, error) {
	outcome, err := detectCrashedWorkersFull(ctx, conn, board)
	if err != nil {
		return nil, err
	}
	return outcome.Crashed, nil
}

// Crash sweep with the breaker accounting and the worker exited observers fired after every commit
func detectCrashedWorkersFull(ctx context.Context, conn state.SQLiteConn, board *string) (*CrashSweepOutcome, error) {
	sweep, err := reclaimDeadWorkers(ctx, conn, board)
	if err != nil {
		return nil, err
	}
	var autoBlocked []string
	if len(sweep.CrashDetails) > 0 {
		if autoBlocked, err = accountCrashes(ctx, conn, sweep.CrashDetails); err != nil {
			return nil, err
		}
	}
	if len(sweep.ExitedHookPayloads) > 0 && kanbanObserverConsumed(ctx, "on_kanban_worker_exited") {
		currentBoard := GetCurrentBoard(ctx)
		for _, fields := range sweep.ExitedHookPayloads {
			kwargs := map[string]any{"board": currentBoard}
			taskID := ""
			for k, v := range fields {
				if k == "task_id" {
					taskID = cellStr(v)
					continue
				}
				kwargs[k] = v
			}
			fireKanbanLifecycleHook(ctx, "on_kanban_worker_exited", taskID, kwargs)
		}
	}
	return &CrashSweepOutcome{Crashed: sweep.Crashed, AutoBlocked: autoBlocked, RateLimited: sweep.RateLimited}, nil
}
