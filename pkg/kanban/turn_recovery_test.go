package kanban

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const farFutureExpiry = int64(4_102_444_800)

var recoveryKanbanEnv = []string{"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_GOAL_MODE", "ICEBATH_KANBAN_TURN_RECOVERY", "ICEBATH_KANBAN_RUN_ID",
	"ICEBATH_KANBAN_CLAIM_LOCK", "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD"}

// Removes every kanban worker variable for the test and restores the previous values afterwards
func clearKanbanEnv(t *testing.T) {
	t.Helper()
	for _, name := range recoveryKanbanEnv {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Failed turn result carrying the classifier stamps the recovery gate reads
func failedTurn(retryable bool, reason, errText string) map[string]any {
	return map[string]any{"failed": true, "failure_retryable": retryable, "failure_reason": reason, "error": errText,
		"final_response": "API call failed after 3 retries: " + errText, "completed": false, "messages": []any{}}
}

func retryableTimeout() map[string]any { return failedTurn(true, "timeout", "peer closed connection") }

func successTurn() map[string]any {
	return map[string]any{"failed": false, "completed": true, "final_response": "done", "messages": []any{}}
}

func interruptedTurn() map[string]any {
	r := retryableTimeout()
	r["interrupted"] = true
	r["api_calls"] = int64(4)
	return r
}

func settledTurn() map[string]any {
	r := retryableTimeout()
	r["turn_exit_reason"] = "max_iterations_reached(60/60)"
	return r
}

func partialTurn() map[string]any {
	return map[string]any{"partial": true, "completed": false, "final_response": "cut off", "error": "truncated", "messages": []any{}}
}

type recoveryBoard struct {
	status      string
	taskPid     any
	runID       any
	lock        string
	runEnded    any
	runPid      any
	taskExpires any
	runExpires  any
}

// Board whose task carries a live unexpired task and run lease unless a field says otherwise
func liveRecoveryBoard() recoveryBoard {
	return recoveryBoard{status: "running", runID: int64(1), lock: "lk", taskExpires: farFutureExpiry, runExpires: farFutureExpiry}
}

// Writes a two table board database holding the one task and its run row
func makeRecoveryBoard(t *testing.T, b recoveryBoard) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "kanban.db")
	conn := rawSQLite(t, db)
	defer conn.Close()
	mustExec(t, conn,
		"CREATE TABLE tasks (id TEXT PRIMARY KEY, status TEXT, worker_pid INTEGER, current_run_id INTEGER, claim_lock TEXT, claim_expires INTEGER)",
		"CREATE TABLE task_runs (id INTEGER PRIMARY KEY, ended_at INTEGER, worker_pid INTEGER, claim_expires INTEGER)")
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "INSERT INTO tasks VALUES (?, ?, ?, ?, ?, ?)", "t_live", b.status, b.taskPid, b.runID, b.lock, b.taskExpires); err != nil {
		t.Fatal(err)
	}
	if b.runID != nil {
		if _, err := conn.ExecContext(ctx, "INSERT INTO task_runs VALUES (?, ?, ?, ?)", b.runID, b.runEnded, b.runPid, b.runExpires); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Pins the whole dispatcher spawn carrier, database, run id and claim lock
func pinCarrier(t *testing.T, db, task, runID, lock string) {
	t.Helper()
	t.Setenv("ICEBATH_KANBAN_TASK", task)
	t.Setenv("ICEBATH_KANBAN_DB", db)
	t.Setenv("ICEBATH_KANBAN_RUN_ID", runID)
	t.Setenv("ICEBATH_KANBAN_CLAIM_LOCK", lock)
}

// Environment pinned proof result, failing the test when the proof itself errors
func envClaimIsLive(t *testing.T) bool {
	t.Helper()
	live, err := WorkerClaimIsLive(context.Background(), ClaimProof{})
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func alwaysLive() (bool, error) { return true, nil }

func anyResult(fn func() map[string]any) func() any {
	return func() any { return fn() }
}

func TestDisabledWithoutKanbanTask(t *testing.T) {
	clearKanbanEnv(t)
	if KanbanTaskID() != "" {
		t.Fatalf("task id %q", KanbanTaskID())
	}
	if KanbanTurnRecoveryEnabled(nil) {
		t.Fatal("recovery enabled without a worker task")
	}
	var calls []string
	attempts, err := RecoverFailedKanbanTurnsWith(context.Background(), func(nudge string) { calls = append(calls, nudge) },
		anyResult(retryableTimeout), RecoveryOptions{Sleep: func(float64) {}})
	if err != nil || attempts != 0 || len(calls) != 0 {
		t.Fatalf("attempts %d calls %v err %v", attempts, calls, err)
	}
}

func TestEnvZeroDisablesAndBlankIdsAreNotWorkers(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	t.Setenv("ICEBATH_KANBAN_TURN_RECOVERY", "0")
	if KanbanTurnRecoveryEnabled(nil) {
		t.Fatal("zero budget left recovery enabled")
	}
	if ShouldRecoverTurn(retryableTimeout(), 0, nil) {
		t.Fatal("zero budget granted a retry")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "   ")
	if KanbanTaskID() != "" {
		t.Fatalf("blank id read as worker %q", KanbanTaskID())
	}
	if KanbanTurnRecoveryEnabled(nil) {
		t.Fatal("blank id enabled recovery")
	}
	os.Unsetenv("ICEBATH_KANBAN_TURN_RECOVERY")
	t.Setenv("ICEBATH_KANBAN_TASK", "  t_probe  ")
	if KanbanTaskID() != "t_probe" {
		t.Fatalf("padded id %q", KanbanTaskID())
	}
	if !KanbanTurnRecoveryEnabled(nil) {
		t.Fatal("padded id did not enable recovery")
	}
}

func TestMaxAttemptsParsingAndClamp(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	if got := MaxRecoveryAttempts(); got != DefaultMaxRecoveryAttempts {
		t.Fatalf("unset %d", got)
	}
	for _, c := range []struct {
		raw  string
		want int
	}{{"2", 2}, {"99", 10}, {"abc", DefaultMaxRecoveryAttempts}, {"false", 0}} {
		t.Setenv("ICEBATH_KANBAN_TURN_RECOVERY", c.raw)
		if got := MaxRecoveryAttempts(); got != c.want {
			t.Fatalf("%q gave %d want %d", c.raw, got, c.want)
		}
	}
}

func TestDelayScheduleRepeatsLastEntry(t *testing.T) {
	if !reflect.DeepEqual(RecoveryDelaysSeconds, []float64{15.0, 45.0, 90.0}) {
		t.Fatalf("schedule %v", RecoveryDelaysSeconds)
	}
	for attempt, want := range map[int]float64{1: 15.0, 3: 90.0, 9: 90.0, 0: 15.0} {
		if got := RecoveryDelaySeconds(attempt); got != want {
			t.Fatalf("attempt %d delay %v want %v", attempt, got, want)
		}
	}
}

func TestRetryableFailedTurnIsEligible(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	for attempt, want := range map[int]bool{0: true, 2: true, 3: false} {
		if got := ShouldRecoverTurn(retryableTimeout(), attempt, nil); got != want {
			t.Fatalf("attempt %d eligible %v want %v", attempt, got, want)
		}
	}
}

func TestShouldRecoverTurnRefuses(t *testing.T) {
	cases := []struct {
		id     string
		result any
	}{
		{"not-retryable", failedTurn(false, "auth", "peer closed connection")},
		{"success", successTurn()},
		{"nothing-settled", nil},
		{"quota-wall", failedTurn(true, "rate_limit", "peer closed connection")},
		{"billing-wall", failedTurn(true, "billing", "peer closed connection")},
		{"upstream-quota-wall", failedTurn(true, "upstream_rate_limit", "peer closed connection")},
		{"interrupt", interruptedTurn()},
		{"terminal-settlement", settledTurn()},
		{"unfinished-nonfailed", partialTurn()},
		{"completed-false", map[string]any{"completed": false, "final_response": "x"}},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			clearKanbanEnv(t)
			t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
			if ShouldRecoverTurn(c.result, 0, nil) {
				t.Fatal("retry authority granted")
			}
		})
	}
}

func TestClaimOwnershipMatrix(t *testing.T) {
	pid := int64(os.Getpid())
	cases := []struct {
		id     string
		mutate func(b *recoveryBoard)
		live   bool
	}{
		{"owned-live", func(b *recoveryBoard) {}, true},
		{"released-to-the-board", func(b *recoveryBoard) { b.status = "ready" }, false},
		{"other-process-owns-the-claim", func(b *recoveryBoard) { b.taskPid = int64(999_999_999) }, false},
		{"run-already-closed", func(b *recoveryBoard) { b.runEnded = int64(12345) }, false},
		{"run-handed-to-another-pid", func(b *recoveryBoard) { b.runPid = int64(999_999_999) }, false},
		{"no-live-run-pointer", func(b *recoveryBoard) { b.runID = nil }, false},
		{"board-moved-to-another-run", func(b *recoveryBoard) { b.runID = int64(2) }, false},
		{"claim-lock-no-longer-ours", func(b *recoveryBoard) { b.lock = "other-lock" }, false},
		{"task-lease-expired", func(b *recoveryBoard) { b.taskExpires = int64(1) }, false},
		{"run-lease-expired", func(b *recoveryBoard) { b.runExpires = int64(1) }, false},
		{"null-task-lease-is-not-unbounded", func(b *recoveryBoard) { b.taskExpires = nil }, false},
		{"null-run-lease-is-not-unbounded", func(b *recoveryBoard) { b.runExpires = nil }, false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			clearKanbanEnv(t)
			board := liveRecoveryBoard()
			board.taskPid, board.runPid = pid, pid
			c.mutate(&board)
			pinCarrier(t, makeRecoveryBoard(t, board), "t_live", "1", "lk")
			if got := envClaimIsLive(t); got != c.live {
				t.Fatalf("live %v want %v", got, c.live)
			}
		})
	}
}

func TestClaimCheckFailsClosedWithoutABoard(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_live")
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(t.TempDir(), "missing.db"))
	if envClaimIsLive(t) {
		t.Fatal("missing board proved live")
	}
	os.Unsetenv("ICEBATH_KANBAN_DB")
	t.Setenv("ICEBATH_KANBAN_BOARD", "no-such-board-xyz")
	if envClaimIsLive(t) {
		t.Fatal("ambient board rescued the proof")
	}
}

func TestMissingPinnedCarrierFailsClosed(t *testing.T) {
	clearKanbanEnv(t)
	db := makeRecoveryBoard(t, liveRecoveryBoard())
	pinCarrier(t, db, "t_live", "1", "lk")
	if !envClaimIsLive(t) {
		t.Fatal("full carrier did not prove live")
	}
	for _, pin := range [][2]string{{"ICEBATH_KANBAN_DB", db}, {"ICEBATH_KANBAN_RUN_ID", "1"}, {"ICEBATH_KANBAN_CLAIM_LOCK", "lk"}} {
		os.Unsetenv(pin[0])
		if envClaimIsLive(t) {
			t.Fatalf("missing %s still proved live", pin[0])
		}
		t.Setenv(pin[0], pin[1])
	}
}

func TestMissingDbPinNeverResolvesAnAmbientBoard(t *testing.T) {
	clearKanbanEnv(t)
	setupKanbanHome(t)
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn := mustConnect(t, "", nil)
	taskID, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "ambient live task"})
	if err != nil {
		t.Fatal(err)
	}
	ttl, claimer := int64(3600), "lk"
	claimed, err := ClaimTask(ctx, conn, taskID, &ttl, &claimer)
	if err != nil || claimed == nil {
		t.Fatalf("claim %v %v", claimed, err)
	}
	conn.Close()
	for _, name := range []string{"ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_RUN_ID", "ICEBATH_KANBAN_CLAIM_LOCK"} {
		os.Unsetenv(name)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", taskID)
	var consulted []*string
	real := kanbanDBPathFn
	kanbanDBPathFn = func(ctx context.Context, board *string) (string, error) {
		consulted = append(consulted, board)
		return real(ctx, board)
	}
	t.Cleanup(func() { kanbanDBPathFn = real })
	if envClaimIsLive(t) {
		t.Fatal("ambient board proved live without the pin")
	}
	if len(consulted) != 0 {
		t.Fatalf("ambient resolver consulted %d times", len(consulted))
	}
}

func TestLeaseExpiredDuringTheBackoffBlocksModelReentry(t *testing.T) {
	clearKanbanEnv(t)
	board := liveRecoveryBoard()
	board.taskExpires, board.runExpires = int64(1_600), int64(1_600)
	pinCarrier(t, makeRecoveryBoard(t, board), "t_live", "1", "lk")
	clock := []int64{1_000, 1_700}
	real := recoveryNow
	recoveryNow = func() int64 {
		now := clock[0]
		clock = clock[1:]
		return now
	}
	t.Cleanup(func() { recoveryNow = real })
	var turns, emitted []string
	attempts, err := RecoverFailedKanbanTurnsWith(context.Background(), func(nudge string) { turns = append(turns, nudge) },
		anyResult(retryableTimeout), RecoveryOptions{Sleep: func(float64) {}, Emit: func(m string) { emitted = append(emitted, m) }})
	if err != nil || attempts != 1 {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	if len(turns) != 0 {
		t.Fatalf("model re-entered %d times", len(turns))
	}
	if !strings.Contains(emitted[len(emitted)-1], "no longer holds a live run/claim") {
		t.Fatalf("last line %q", emitted[len(emitted)-1])
	}
}

func TestRecoveryLoopRetriesUntilSuccessAndReports(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	latest := retryableTimeout()
	var turns, emitted []string
	var delays []float64
	attempts, err := RecoverFailedKanbanTurnsWith(context.Background(), func(nudge string) {
		turns = append(turns, nudge)
		latest = successTurn()
	}, func() any { return latest }, RecoveryOptions{Sleep: func(s float64) { delays = append(delays, s) },
		Emit: func(m string) { emitted = append(emitted, m) }, ClaimCheck: alwaysLive})
	if err != nil || attempts != 1 {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	if !reflect.DeepEqual(delays, []float64{15.0}) {
		t.Fatalf("delays %v", delays)
	}
	if !strings.Contains(turns[0], "Do NOT start over") || !strings.Contains(turns[0], "t_probe") {
		t.Fatalf("nudge %q", turns[0])
	}
	if len(emitted) != 1 || !strings.Contains(emitted[0], "[kanban]") || !strings.Contains(emitted[0], "attempt 1/3") {
		t.Fatalf("emitted %v", emitted)
	}
}

func TestRecoveryLoopBoundedWhenResultNeverChanges(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	var delays []float64
	attempts, err := RecoverFailedKanbanTurnsWith(context.Background(), func(string) {}, anyResult(retryableTimeout),
		RecoveryOptions{Sleep: func(s float64) { delays = append(delays, s) }, Emit: func(string) {}, ClaimCheck: alwaysLive})
	if err != nil || attempts != DefaultMaxRecoveryAttempts {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	if !reflect.DeepEqual(delays, []float64{15.0, 45.0, 90.0}) {
		t.Fatalf("delays %v", delays)
	}
}

func TestRecoveryLoopOrderIsCheckSleepCheckTurn(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	t.Setenv("ICEBATH_KANBAN_TURN_RECOVERY", "3")
	var events []string
	latest := retryableTimeout()
	proofs := 0
	turnFn := func(string) {
		events = append(events, "turn")
		latest = successTurn()
	}
	sleep := func(float64) { events = append(events, "sleep") }
	attempts, err := RecoverFailedKanbanTurnsWith(context.Background(), turnFn, func() any { return latest },
		RecoveryOptions{Sleep: sleep, Emit: func(string) {}, ClaimCheck: func() (bool, error) {
			proofs++
			events = append(events, "check")
			return proofs == 1, nil
		}})
	if err != nil || attempts != 1 {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	if !reflect.DeepEqual(events, []string{"check", "sleep", "check"}) {
		t.Fatalf("events %v", events)
	}
	events = nil
	latest = retryableTimeout()
	attempts, err = RecoverFailedKanbanTurnsWith(context.Background(), turnFn, func() any { return latest },
		RecoveryOptions{Sleep: sleep, Emit: func(string) {}, ClaimCheck: func() (bool, error) {
			events = append(events, "check")
			return true, nil
		}})
	if err != nil || attempts != 1 {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	if !reflect.DeepEqual(events, []string{"check", "sleep", "check", "turn"}) {
		t.Fatalf("events %v", events)
	}
}

func TestNudgeTerminalContract(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_probe")
	nudge := BuildRecoveryNudge(failedTurn(true, "timeout", "peer closed connection"), 1, 3, nil)
	for _, want := range []string{"t_probe", "1/3", "kanban_complete", "kanban_block", "Do NOT start over", "peer closed connection"} {
		if !strings.Contains(nudge, want) {
			t.Fatalf("nudge lacks %q in %q", want, nudge)
		}
	}
}

func TestExplicitBlankCarrierNeverFallsBackToTheEnvPin(t *testing.T) {
	clearKanbanEnv(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_foreign")
	blank, bound := "   ", "  t_bound  "
	if ShouldRecoverTurn(retryableTimeout(), 0, &blank) {
		t.Fatal("blank explicit carrier fell back to the environment pin")
	}
	if !ShouldRecoverTurn(retryableTimeout(), 0, &bound) {
		t.Fatal("explicit carrier refused")
	}
}

func TestRecoverFailedKanbanTurnsReadsTheEnvironmentCarrier(t *testing.T) {
	clearKanbanEnv(t)
	pid := int64(os.Getpid())
	board := liveRecoveryBoard()
	board.taskPid, board.runPid = pid, pid
	pinCarrier(t, makeRecoveryBoard(t, board), "t_live", "1", "lk")
	t.Setenv("ICEBATH_KANBAN_TURN_RECOVERY", "0")
	attempts, err := RecoverFailedKanbanTurns(context.Background(), func(string) { t.Fatal("turn ran with a zero budget") },
		retryableTimeout, func(string) {})
	if err != nil || attempts != 0 {
		t.Fatalf("attempts %d err %v", attempts, err)
	}
	if got := strconv.Itoa(MaxRecoveryAttempts()); got != "0" {
		t.Fatalf("budget %s", got)
	}
}
