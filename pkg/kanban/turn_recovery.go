package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

var recoveryLog = slog.Default().With("logger", "agent.kanban_turn_recovery")

// Recovery attempts made when ICEBATH_KANBAN_TURN_RECOVERY is unset
const DefaultMaxRecoveryAttempts = 3

// Backoff seconds before each recovery attempt, the last entry repeating
var RecoveryDelaysSeconds = []float64{15.0, 45.0, 90.0}

var nonRecoverableReasons = map[string]bool{"rate_limit": true, "billing": true, "upstream_rate_limit": true}

var terminalTurnExitPrefixes = []string{"max_iterations_reached"}

var recoveryOffValues = map[string]bool{"0": true, "false": true, "no": true, "off": true}

// Wall clock seconds the claim proof compares leases against
var recoveryNow = func() int64 { return time.Now().Unix() }

// Dispatcher set worker task id stripped once, empty when this process is not a worker run
func KanbanTaskID() string {
	return pystr.Strip(os.Getenv("ICEBATH_KANBAN_TASK"))
}

// Explicit carrier id stripped when given, else the environment pin, empty when neither names a worker
func resolveRecoveryTaskID(taskID *string) string {
	if taskID == nil {
		return KanbanTaskID()
	}
	return pystr.Strip(*taskID)
}

// True when a worker task id is carried and the attempt budget is above zero
func KanbanTurnRecoveryEnabled(taskID *string) bool {
	if resolveRecoveryTaskID(taskID) == "" {
		return false
	}
	return MaxRecoveryAttempts() > 0
}

// Attempt budget from ICEBATH_KANBAN_TURN_RECOVERY clamped to zero through ten, the default when unset or unparseable
func MaxRecoveryAttempts() int {
	raw := pystr.Strip(os.Getenv("ICEBATH_KANBAN_TURN_RECOVERY"))
	if raw == "" {
		return DefaultMaxRecoveryAttempts
	}
	if recoveryOffValues[strings.ToLower(raw)] {
		return 0
	}
	value, ok := pyint.Parse(raw)
	if !ok {
		return DefaultMaxRecoveryAttempts
	}
	if value.Sign() < 0 {
		return 0
	}
	if !value.IsInt64() || value.Int64() > 10 {
		return 10
	}
	return int(value.Int64())
}

// Backoff seconds before the one based recovery attempt
func RecoveryDelaySeconds(attempt int) float64 {
	attempt = max(attempt, 1)
	return RecoveryDelaysSeconds[min(attempt-1, len(RecoveryDelaysSeconds)-1)]
}

// True when the turn already ended through a durable terminal settlement
func turnExitIsTerminal(result map[string]any) bool {
	reason := ""
	if v := result["turn_exit_reason"]; pyrepr.Truthy(v) {
		reason = pyrepr.Str(v)
	}
	for _, prefix := range terminalTurnExitPrefixes {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

// True when a settled worker turn is a retryable failure inside the budget with no wall, cancel or settlement
func ShouldRecoverTurn(result any, attempt int, taskID *string) bool {
	if !KanbanTurnRecoveryEnabled(taskID) {
		return false
	}
	if attempt >= MaxRecoveryAttempts() {
		return false
	}
	dict, ok := result.(map[string]any)
	if !ok || dict == nil {
		return false
	}
	if dict["interrupted"] == true {
		return false
	}
	if turnExitIsTerminal(dict) {
		return false
	}
	if dict["failed"] != true {
		return false
	}
	if dict["failure_retryable"] != true {
		return false
	}
	reason := ""
	if v := dict["failure_reason"]; pyrepr.Truthy(v) {
		reason = pyrepr.Str(v)
	}
	return !nonRecoverableReasons[reason]
}

// Dispatcher pinned board database path, empty when the pin is absent
func pinnedKanbanDBPath() string {
	return pystr.Strip(os.Getenv("ICEBATH_KANBAN_DB"))
}

// Explicit carrier a managed worker passes in place of the environment pins
type ClaimProof struct {
	TaskID    *string
	DBPath    *string
	RunID     *string
	ClaimLock *string
	SkipPid   bool
}

// Absolute form of a path the way pathlib absolute joins it onto the working directory
func pathlibAbsolute(p string) (string, error) {
	if pypath.IsAbs(p) {
		return pypath.PathStr(p), nil
	}
	cwd, err := pypath.Getcwd()
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(cwd, p), nil
}

// Text of a cell when it is truthy text, empty for a falsy cell, not ok for any other value
func truthyCellText(v any) (string, bool) {
	if !pyrepr.Truthy(v) {
		return "", true
	}
	s, ok := v.(string)
	return s, ok
}

// True when this process still owns the pinned live run with an unexpired task and run lease
func WorkerClaimIsLive(ctx context.Context, proof ClaimProof) (bool, error) {
	taskID := resolveRecoveryTaskID(proof.TaskID)
	if taskID == "" {
		return false, nil
	}
	dbPath := pinnedKanbanDBPath()
	if proof.DBPath != nil {
		dbPath = pystr.Strip(*proof.DBPath)
	}
	exists := false
	if dbPath != "" {
		info, err := pypath.PathlibStat(dbPath)
		if err != nil {
			return false, err
		}
		exists = info != nil
	}
	if !exists {
		shown := "None"
		if dbPath != "" {
			shown = pyrepr.Repr(dbPath)
		}
		recoveryLog.Warn(fmt.Sprintf("kanban claim check: pinned board db %s not found — not retrying in place", shown))
		return false, nil
	}
	runIDEnv := pystr.Strip(os.Getenv("ICEBATH_KANBAN_RUN_ID"))
	if proof.RunID != nil {
		runIDEnv = pystr.Strip(*proof.RunID)
	}
	lockEnv := pystr.Strip(os.Getenv("ICEBATH_KANBAN_CLAIM_LOCK"))
	if proof.ClaimLock != nil {
		lockEnv = pystr.Strip(*proof.ClaimLock)
	}
	if runIDEnv == "" || lockEnv == "" {
		recoveryLog.Warn("kanban claim check: missing dispatcher-pinned run-id/claim-lock carrier — no exact authority to re-prove, not retrying in place")
		return false, nil
	}
	now := recoveryNow()
	live, err := claimRowsAreLive(ctx, dbPath, taskID, runIDEnv, lockEnv, now, !proof.SkipPid)
	if err != nil {
		recoveryLog.Warn("kanban claim check failed — not retrying in place", "exc_info", err.Error())
		return false, nil
	}
	return live, nil
}

// Reads the task and its open run read only and compares status, pid, run pointer, lock and both leases
func claimRowsAreLive(ctx context.Context, dbPath, taskID, runID, lock string, now int64, verifyPid bool) (bool, error) {
	abs, err := pathlibAbsolute(dbPath)
	if err != nil {
		return false, err
	}
	uri, err := pypath.AsURI(abs)
	if err != nil {
		return false, err
	}
	conn, err := state.ConnectSQLite(uri+"?mode=ro", state.SQLiteConfig{Timeout: 5.0, URI: true})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	row, err := QueryOne(ctx, conn, "SELECT status, worker_pid, current_run_id, claim_lock, claim_expires FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return false, err
	}
	if status, ok := truthyCellText(row.Get("status")); !ok || status != "running" {
		return false, nil
	}
	if ours, err := pidIsOursOrUnset(row.Get("worker_pid"), verifyPid); err != nil || !ours {
		return false, err
	}
	currentRunID := row.Get("current_run_id")
	if currentRunID == nil {
		return false, nil
	}
	if pyrepr.Str(currentRunID) != runID {
		return false, nil
	}
	if held, ok := truthyCellText(row.Get("claim_lock")); !ok || held != lock {
		return false, nil
	}
	if unexpired, err := leaseIsUnexpired(row.Get("claim_expires"), now); err != nil || !unexpired {
		return false, err
	}
	runKey, err := pyIntOf(currentRunID)
	if err != nil {
		return false, err
	}
	run, err := QueryOne(ctx, conn, "SELECT ended_at, worker_pid, claim_expires FROM task_runs WHERE id = ?", runKey)
	if err != nil || run == nil {
		return false, err
	}
	if run.Get("ended_at") != nil {
		return false, nil
	}
	if ours, err := pidIsOursOrUnset(run.Get("worker_pid"), verifyPid); err != nil || !ours {
		return false, err
	}
	return leaseIsUnexpired(run.Get("claim_expires"), now)
}

// True when pid verification is off, the cell is null or the cell names this process
func pidIsOursOrUnset(cell any, verifyPid bool) (bool, error) {
	if !verifyPid || cell == nil {
		return true, nil
	}
	pid, err := pyIntOf(cell)
	if err != nil {
		return false, err
	}
	return pid == int64(os.Getpid()), nil
}

// True when the lease cell is set and not before now
func leaseIsUnexpired(cell any, now int64) (bool, error) {
	if cell == nil {
		return false, nil
	}
	expires, err := pyIntOf(cell)
	if err != nil {
		return false, err
	}
	return expires >= now, nil
}

// Stripped single line text cut to the limit with an ellipsis
func truncateRecoveryText(text string, limit int) string {
	text = strings.ReplaceAll(pystr.Strip(text), "\n", " ")
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

// Synthetic user turn telling the worker to continue the same session and end with a terminal kanban call
func BuildRecoveryNudge(result any, attempt, maxAttempts int, taskID *string) string {
	errText := ""
	if dict, ok := result.(map[string]any); ok && dict != nil {
		source := dict["error"]
		if !pyrepr.Truthy(source) {
			source = dict["final_response"]
		}
		if pyrepr.Truthy(source) {
			errText = truncateRecoveryText(pyrepr.Str(source), 300)
		}
	}
	if errText == "" {
		errText = "provider stream failure"
	}
	task := resolveRecoveryTaskID(taskID)
	if task == "" {
		task = "this task"
	}
	return "[System: the previous turn ended UNFINISHED — the API call failed after " +
		fmt.Sprintf("all retries and the turn was interrupted (recovery attempt %d/%d). ", attempt, maxAttempts) +
		fmt.Sprintf("Error: %s.\n\n", errText) +
		fmt.Sprintf("Task `%s` is still `running`. This is the SAME session, with your full ", task) +
		"context — nothing you already read, wrote, or computed is lost. Do NOT start over.\n\n" +
		"Do this immediately:\n" +
		"1. If the interrupted turn was mid tool-call, re-check the on-disk state of " +
		"whatever it was writing before continuing (the action may not have executed).\n" +
		"2. Continue the task from where it stopped and finish any remaining deliverable.\n" +
		"3. End with a terminal `kanban_complete(summary=..., artifacts=[...])` if the work " +
		"is done, or `kanban_block(reason=...)` if you are blocked.]"
}

// Calls emit and logs at debug when it panics
func emitRecoveryLine(emit func(string), message string) {
	if emit == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			recoveryLog.Debug("kanban turn-recovery emit failed", "exc_info", fmt.Sprint(r))
		}
	}()
	emit(message)
}

// Logs and emits the one line saying recovery was skipped because the live claim proof failed
func emitRecoverySkipped(emit func(string), taskID *string) {
	task := resolveRecoveryTaskID(taskID)
	if task == "" {
		task = "task"
	}
	message := fmt.Sprintf("[kanban] in-place recovery skipped for %s: this worker no longer holds a live run/claim (already settled, reclaimed, or unverifiable)", task)
	recoveryLog.Warn(message)
	emitRecoveryLine(emit, message)
}

// Seams and explicit carrier of the recovery loop, zero value sleeping on the wall clock with the default proof
type RecoveryOptions struct {
	Sleep      func(seconds float64)
	Emit       func(string)
	ClaimCheck func() (bool, error)
	TaskID     *string
}

// Retries an authorised worker turn in place with proof before and after each backoff, returning attempts made
func RecoverFailedKanbanTurnsWith(ctx context.Context, turnFn func(nudge string), getResult func() any, o RecoveryOptions) (int, error) {
	attempts := 0
	check := o.ClaimCheck
	if check == nil {
		check = func() (bool, error) { return WorkerClaimIsLive(ctx, ClaimProof{}) }
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	}
	for {
		result := getResult()
		if !ShouldRecoverTurn(result, attempts, o.TaskID) {
			return attempts, nil
		}
		live, err := check()
		if err != nil {
			return attempts, err
		}
		if !live {
			emitRecoverySkipped(o.Emit, o.TaskID)
			return attempts, nil
		}
		attempts++
		delay := RecoveryDelaySeconds(attempts)
		task := resolveRecoveryTaskID(o.TaskID)
		if task == "" {
			task = "task"
		}
		message := fmt.Sprintf("[kanban] provider failure on %s — retrying the turn in place (attempt %d/%d) after %ds; session context preserved",
			task, attempts, MaxRecoveryAttempts(), int(delay))
		recoveryLog.Warn(message)
		emitRecoveryLine(o.Emit, message)
		sleep(delay)
		live, err = check()
		if err != nil {
			return attempts, err
		}
		if !live {
			emitRecoverySkipped(o.Emit, o.TaskID)
			return attempts, nil
		}
		turnFn(BuildRecoveryNudge(result, attempts, MaxRecoveryAttempts(), o.TaskID))
	}
}

// Retries a failed worker turn in place under the environment pinned carrier, returning attempts made
func RecoverFailedKanbanTurns(ctx context.Context, runTurn func(nudge string), lastResult func() map[string]any, emit func(string)) (int, error) {
	getResult := func() any {
		if result := lastResult(); result != nil {
			return result
		}
		return nil
	}
	return RecoverFailedKanbanTurnsWith(ctx, runTurn, getResult, RecoveryOptions{Emit: emit})
}
