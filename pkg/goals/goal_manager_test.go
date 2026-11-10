package goals

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Manager with a fresh goal in an isolated home
func managerWithGoal(t *testing.T, sessionID, goal string, defaultMaxTurns int64) *GoalManager {
	t.Helper()
	goalsTestHome(t)
	mgr := NewGoalManager(context.Background(), sessionID, defaultMaxTurns)
	if _, err := mgr.Set(goal, 0, nil); err != nil {
		t.Fatal(err)
	}
	return mgr
}

// Decision of one turn, failing the test when the evaluation errors
func evaluate(t *testing.T, mgr *GoalManager, response string, background ...map[string]any) GoalDecision {
	t.Helper()
	decision, err := mgr.EvaluateAfterTurn(response, background, 0)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// Kanban goal loop run whose raised exception fails the test
func kanbanLoop(t *testing.T, ctx context.Context, o GoalLoopOptions) GoalLoopResult {
	t.Helper()
	result, err := RunKanbanGoalLoop(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Counts the gate runs the manager starts while still running each one
func pinGoalRunGate(t *testing.T) *int {
	t.Helper()
	saved := goalRunGate
	t.Cleanup(func() { goalRunGate = saved })
	runs := 0
	goalRunGate = func(ctx context.Context, gate GoalGate, cwd string) (bool, int64, string, error) {
		runs++
		return saved(ctx, gate, cwd)
	}
	return &runs
}

// Child process that sleeps until killed at the end of the test
func spawnSleeper(t *testing.T) int64 {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return int64(cmd.Process.Pid)
}

func TestGoalManagerSetThenStatus(t *testing.T) {
	goalsTestHome(t)
	mgr := NewGoalManager(context.Background(), "test-sid-2", 5)
	state, err := mgr.Set("port the thing", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Goal != "port the thing" || state.Status != "active" || state.MaxTurns != 5 || state.TurnsUsed != 0 || !mgr.IsActive() {
		t.Fatalf("state %+v", state)
	}
	if line := mgr.StatusLine(); line != "⊙ Goal (active, 0/5 turns): port the thing" {
		t.Fatalf("status line %q", line)
	}
	if _, err := mgr.Set("  ", 0, nil); pyKind(err) != "ValueError" || err.Error() != "goal text is empty" {
		t.Fatalf("empty goal error %v", err)
	}
	if state, _ := mgr.Set("explicit budget", 9, nil); state.MaxTurns != 9 {
		t.Fatalf("explicit budget %d", state.MaxTurns)
	}
}

func TestGoalManagerContinuationPromptShape(t *testing.T) {
	mgr := managerWithGoal(t, "cont-sid", "port goal command to hermes", 0)
	prompt := mgr.NextContinuationPrompt()
	if prompt == nil || !strings.Contains(*prompt, "port goal command to hermes") || strings.TrimSpace(*prompt) == "" {
		t.Fatalf("prompt %v", prompt)
	}
	want := "[Continuing toward your standing goal]\nGoal: port goal command to hermes\n\nContinue working toward this goal. Take the next concrete step. " +
		"If you believe the goal is complete, state so explicitly and stop. If you are blocked and need input from the user, say so clearly and stop."
	if *prompt != want {
		t.Fatalf("prompt %q", *prompt)
	}
	if _, err := mgr.AddSubgoal("write tests"); err != nil {
		t.Fatal(err)
	}
	if prompt = mgr.NextContinuationPrompt(); !strings.Contains(*prompt, "- 1. write tests") {
		t.Fatalf("subgoal prompt %q", *prompt)
	}
	mgr.SetContract(GoalContract{Verification: "suite green"})
	if prompt = mgr.NextContinuationPrompt(); !strings.Contains(*prompt, "- Verification: suite green\n- Extra criterion 1: write tests") {
		t.Fatalf("contract prompt %q", *prompt)
	}
	mgr.Pause(UserPausedReason)
	if mgr.NextContinuationPrompt() != nil {
		t.Fatal("a paused goal produced a continuation prompt")
	}
}

func TestJudgeParseFailureAutoPauseAutoPauseAfterThreeConsecutiveParseFailures(t *testing.T) {
	mgr := managerWithGoal(t, "parse-fail-sid-1", "do a thing", 20)
	pinGoalJudge(t, JudgeResult{Verdict: "continue", Reason: "judge returned empty response", ParseFailed: true})
	for step, want := range []int64{1, 2} {
		if d := evaluate(t, mgr, "step"); !d.ShouldContinue || mgr.State().ConsecutiveParseFailures != want {
			t.Fatalf("step %d decision %+v failures %d", step+1, d, mgr.State().ConsecutiveParseFailures)
		}
	}
	d3 := evaluate(t, mgr, "step 3")
	if d3.ShouldContinue || d3.Status != "paused" || mgr.State().ConsecutiveParseFailures != 3 {
		t.Fatalf("third decision %+v", d3)
	}
	if !strings.Contains(d3.Message, "~/.icebath/config.yaml:\n  auxiliary:\n    goal_judge:\n      provider: openrouter\n      model: google/gemini-3-flash-preview\nThen /goal resume to continue.") {
		t.Fatalf("message %q", d3.Message)
	}
}

func TestJudgeTransportFailuresPauseAfterFiveAndBudgetPausesTheLoop(t *testing.T) {
	mgr := managerWithGoal(t, "transport-sid", "do a thing", 20)
	pinGoalJudge(t, JudgeResult{Verdict: "continue", Reason: "judge error: TimeoutError", TransportFailed: true})
	var last GoalDecision
	for i := 0; i < 5; i++ {
		last = evaluate(t, mgr, "step")
	}
	if last.Status != "paused" || last.ShouldContinue || !strings.Contains(last.Message, "judge API returned errors (5 turns)") ||
		mgr.State().PausedReason != "judge API unreachable 5 turns in a row (check auxiliary.goal_judge provider/key in config.yaml)" {
		t.Fatalf("decision %+v reason %v", last, mgr.State().PausedReason)
	}
	budget := managerWithGoal(t, "budget-sid", "small budget", 2)
	pinGoalJudge(t, JudgeResult{Verdict: "continue", Reason: "more to do"})
	if d := evaluate(t, budget, "one"); !d.ShouldContinue || d.Message != "↻ Continuing toward goal (1/2): more to do" || d.ContinuationPrompt == nil {
		t.Fatalf("first %+v", d)
	}
	d := evaluate(t, budget, "two")
	if d.Status != "paused" || d.Message != "⏸ Goal paused — 2/2 turns used. Use /goal resume to keep going, or /goal clear to stop." {
		t.Fatalf("budget pause %+v", d)
	}
	if line := budget.StatusLine(); line != "⏸ Goal (paused, 2/2 turns — turn budget exhausted (2/2)): small budget" {
		t.Fatalf("status line %q", line)
	}
	if d := evaluate(t, budget, "three"); d.Verdict != "inactive" || d.Status != "paused" || d.Reason != "no active goal" {
		t.Fatalf("inactive %+v", d)
	}
	budget.Resume(true)
	if budget.State().TurnsUsed != 0 || !budget.IsActive() {
		t.Fatal("resume did not reset the budget")
	}
	pinGoalJudge(t, JudgeResult{Verdict: "done", Reason: "shipped"})
	if d := evaluate(t, budget, "four"); d.Status != "done" || d.Message != "✓ Goal achieved: shipped" || budget.StatusLine() != "✓ Goal done (1/2 turns): small budget" {
		t.Fatalf("done %+v %q", d, budget.StatusLine())
	}
	budget.Clear()
	if budget.State() != nil || budget.StatusLine() != "No active goal. Set one with /goal <text>." {
		t.Fatal("clear left a goal")
	}
	if d := evaluate(t, budget, "five"); d.Status != nil || d.Verdict != "inactive" {
		t.Fatalf("no goal %+v", d)
	}
}

func TestGoalManagerSubgoalsAddSubgoal(t *testing.T) {
	mgr := managerWithGoal(t, "sub-add", "main goal", 0)
	text, err := mgr.AddSubgoal("  use bullet points  ")
	if err != nil || text != "use bullet points" || !reflect.DeepEqual(mgr.State().Subgoals, []string{"use bullet points"}) {
		t.Fatalf("%q %v %v", text, err, mgr.State().Subgoals)
	}
	if _, err := mgr.AddSubgoal(" "); pyKind(err) != "ValueError" {
		t.Fatalf("empty subgoal %v", err)
	}
	if mgr.RenderSubgoals() != "- 1. use bullet points" || mgr.StatusLine() != "⊙ Goal (active, 0/20 turns, 1 subgoal): main goal" {
		t.Fatalf("%q %q", mgr.RenderSubgoals(), mgr.StatusLine())
	}
	if n, err := mgr.ClearSubgoals(); err != nil || n != 1 || mgr.RenderSubgoals() != "(no subgoals — use /subgoal <text> to add criteria)" {
		t.Fatalf("clear %d %v", n, err)
	}
}

func TestGoalManagerSubgoalsRemoveSubgoalOutOfRange(t *testing.T) {
	mgr := managerWithGoal(t, "sub-oob", "g", 0)
	if _, err := mgr.AddSubgoal("only"); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int64{5, 0} {
		if _, err := mgr.RemoveSubgoal(index); pyKind(err) != "IndexError" || err.Error() != "index out of range (1..1)" {
			t.Fatalf("index %d error %v", index, err)
		}
	}
	if removed, err := mgr.RemoveSubgoal(1); err != nil || removed != "only" || len(mgr.State().Subgoals) != 0 {
		t.Fatalf("remove %q %v", removed, err)
	}
	empty := NewGoalManager(context.Background(), "sub-none", 0)
	if _, err := empty.AddSubgoal("x"); pyKind(err) != "RuntimeError" || err.Error() != "no active goal" {
		t.Fatalf("no goal error %v", err)
	}
}

func TestWaitBarrierParkedOnLivePidDoesNotContinueOrJudge(t *testing.T) {
	goalsTestHome(t)
	mgr := NewGoalManager(context.Background(), "wb-live", 0)
	if _, err := mgr.Set("ship it", 5, nil); err != nil {
		t.Fatal(err)
	}
	pid := spawnSleeper(t)
	if _, err := mgr.WaitOn(pid, "CI green"); err != nil || !mgr.IsWaiting() {
		t.Fatalf("park %v", err)
	}
	calls := pinGoalJudge(t, JudgeResult{Verdict: "continue", Reason: "x"})
	decision := evaluate(t, mgr, "still waiting on CI")
	if *calls != 0 || decision.Verdict != "waiting" || decision.ShouldContinue || decision.ContinuationPrompt != nil || mgr.State().TurnsUsed != 0 ||
		!strings.Contains(decision.Message, "CI green") || mgr.State().Status != "active" {
		t.Fatalf("decision %+v after %d judge calls", decision, *calls)
	}
	if line := mgr.StatusLine(); !strings.HasPrefix(line, "⏳ Goal (parked on CI green, 0/5 turns): ship it") {
		t.Fatalf("status line %q", line)
	}
}

func TestWaitBarrierWaitOnRejectsAPidNotAliveOnThisHost(t *testing.T) {
	mgr := managerWithGoal(t, "wb-dead", "ship it", 0)
	saved := goalPidExists
	t.Cleanup(func() { goalPidExists = saved })
	goalPidExists = func(int) bool { return false }
	if _, err := mgr.WaitOn(4242, "remote CI"); pyKind(err) != "ValueError" || !strings.Contains(err.Error(), "not alive on this host") {
		t.Fatalf("error %v", err)
	}
	if mgr.State().WaitingOnPID != nil {
		t.Fatal("a barrier was set on a dead pid")
	}
	if _, err := mgr.WaitOn(0, ""); pyKind(err) != "ValueError" || err.Error() != "pid must be a positive integer" {
		t.Fatalf("zero pid error %v", err)
	}
}

func TestWaitBarrierStopWaitingClearsBarrier(t *testing.T) {
	mgr := managerWithGoal(t, "wb-stop", "g", 0)
	if _, err := mgr.WaitOn(spawnSleeper(t), ""); err != nil || !mgr.IsWaiting() {
		t.Fatalf("park %v", err)
	}
	if !mgr.StopWaiting() || mgr.State().WaitingOnPID != nil || mgr.IsWaiting() || mgr.StopWaiting() {
		t.Fatal("stop waiting")
	}
}

func TestWaitBarrierBarrierOnAProcessThatNeverExitsExpires(t *testing.T) {
	mgr := managerWithGoal(t, "wb-expire", "g", 0)
	if _, err := mgr.WaitOn(spawnSleeper(t), "poller"); err != nil || !mgr.IsWaiting() {
		t.Fatalf("park %v", err)
	}
	mgr.State().WaitingSince = goalTime() - maxBarrierWaitS - 1
	mgr.save()
	if mgr.IsWaiting() || mgr.State().WaitingOnPID != nil {
		t.Fatal("an overlong barrier still parks the goal")
	}
}

func TestJudgeDrivenWaitJudgeWaitOnDeadPidContinuesInsteadOfParking(t *testing.T) {
	mgr := managerWithGoal(t, "jw-dead-pid", "ship the PR", 10)
	saved := goalPidExists
	t.Cleanup(func() { goalPidExists = saved })
	goalPidExists = func(int) bool { return false }
	pinGoalJudge(t, JudgeResult{Verdict: "wait", Reason: "remote job still running", Wait: map[string]any{"pid": int64(4242)}})
	decision := evaluate(t, mgr, "Started the job over ssh (pid 4242).")
	if decision.Verdict != "continue" || !decision.ShouldContinue || mgr.State().WaitingOnPID != nil || mgr.IsWaiting() {
		t.Fatalf("decision %+v", decision)
	}
}

func TestJudgeDrivenWaitJudgeWaitOnPidDyingBetweenCheckAndParkContinues(t *testing.T) {
	mgr := managerWithGoal(t, "jw-toctou-pid", "ship the PR", 10)
	savedExists, savedWait := goalPidExists, goalManagerWaitOn
	t.Cleanup(func() { goalPidExists, goalManagerWaitOn = savedExists, savedWait })
	goalPidExists = func(int) bool { return true }
	goalManagerWaitOn = func(*GoalManager, int64, string) (*GoalState, error) {
		return nil, &pyint.Error{Kind: "ValueError", Msg: "pid is not alive on this host"}
	}
	pinGoalJudge(t, JudgeResult{Verdict: "wait", Reason: "job still running", Wait: map[string]any{"pid": int64(4242)}})
	decision := evaluate(t, mgr, "Started the job (pid 4242).")
	if decision.Verdict != "continue" || !decision.ShouldContinue || mgr.State().WaitingOnPID != nil || mgr.IsWaiting() {
		t.Fatalf("decision %+v", decision)
	}
}

func TestJudgeDrivenWaitJudgeWaitPidParksLoop(t *testing.T) {
	mgr := managerWithGoal(t, "jw-pid", "ship the PR", 10)
	pid := spawnSleeper(t)
	pinGoalJudge(t, JudgeResult{Verdict: "wait", Reason: "CI watcher still running", Wait: map[string]any{"pid": pid}})
	decision := evaluate(t, mgr, "Pushed the PR, watching CI.", map[string]any{"pid": pid, "command": "wait_for_pr_green.sh", "status": "running", "uptime_seconds": int64(12)})
	if decision.Verdict != "wait" || decision.ShouldContinue || decision.ContinuationPrompt != nil || mgr.State().WaitingOnPID == nil ||
		*mgr.State().WaitingOnPID != pid || !mgr.IsWaiting() {
		t.Fatalf("decision %+v", decision)
	}
	calls := pinGoalJudge(t, JudgeResult{})
	second := evaluate(t, mgr, "still going")
	if *calls != 0 || second.Verdict != "waiting" || second.ShouldContinue {
		t.Fatalf("second decision %+v after %d judge calls", second, *calls)
	}
}

func TestJudgeDrivenWaitTimeBarrierClearsAfterDeadline(t *testing.T) {
	mgr := managerWithGoal(t, "jw-deadline", "g", 0)
	if _, err := mgr.WaitForSeconds(120, "backoff", 0); err != nil || !mgr.IsWaiting() {
		t.Fatalf("park %v", err)
	}
	if line := mgr.StatusLine(); !strings.HasPrefix(line, "⏳ Goal (parked 1") || !strings.Contains(line, "s — backoff, 0/20 turns): g") {
		t.Fatalf("status line %q", line)
	}
	mgr.State().WaitingUntil = goalTime() - 1
	if mgr.IsWaiting() || mgr.State().WaitingUntil != 0.0 {
		t.Fatal("a passed deadline still parks the goal")
	}
	if _, err := mgr.WaitForSeconds(0, "", 0); pyKind(err) != "ValueError" || err.Error() != "seconds must be a positive integer" {
		t.Fatalf("zero seconds error %v", err)
	}
}

func TestJudgeDrivenWaitContinueVerdictStillContinuesWithBackground(t *testing.T) {
	mgr := managerWithGoal(t, "jw-cont", "do work", 10)
	pinGoalJudge(t, JudgeResult{Verdict: "continue", Reason: "more to do"})
	decision := evaluate(t, mgr, "made progress", map[string]any{"pid": int64(999999), "command": "x", "status": "running"})
	if decision.Verdict != "continue" || !decision.ShouldContinue || mgr.State().WaitingOnPID != nil {
		t.Fatalf("decision %+v", decision)
	}
}

func TestSessionTriggerBarrierWaitOnSessionValidation(t *testing.T) {
	goalsTestHome(t)
	mgr := NewGoalManager(context.Background(), "st-val", 0)
	if _, err := mgr.WaitOnSession("proc_x", ""); pyKind(err) != "RuntimeError" || err.Error() != "no active goal to park" {
		t.Fatalf("no goal error %v", err)
	}
	if _, err := mgr.Set("g", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.WaitOnSession("  ", ""); pyKind(err) != "ValueError" || err.Error() != "session_id must be a non-empty string" {
		t.Fatalf("empty session error %v", err)
	}
	saved := goalIsSessionWaiting
	t.Cleanup(func() { goalIsSessionWaiting = saved })
	waiting := true
	goalIsSessionWaiting = func(id string) bool { return waiting && id == "proc_x" }
	if state, err := mgr.WaitOnSession(" proc_x ", " watcher "); err != nil || *state.WaitingOnSession != "proc_x" || state.WaitingReason != "watcher" || !mgr.IsWaiting() {
		t.Fatalf("park %v", err)
	}
	if d := evaluate(t, mgr, "x"); d.Message != "⏳ Goal parked — waiting on session proc_x: watcher" {
		t.Fatalf("waiting decision %+v", d)
	}
	waiting = false
	if mgr.IsWaiting() || mgr.State().WaitingOnSession != nil {
		t.Fatal("a fired session trigger still parks the goal")
	}
	pinGoalJudge(t, JudgeResult{Verdict: "wait", Reason: "poller", Wait: map[string]any{"session_id": "proc_y"}})
	if d := evaluate(t, mgr, "x"); d.Verdict != "wait" || d.Message != "⏳ Goal parked (judge) — waiting on session proc_y: poller" {
		t.Fatalf("judge parked decision %+v", d)
	}
}

func TestGoalManagerContractSetContractAfterTheFact(t *testing.T) {
	mgr := managerWithGoal(t, "c-after", "ship it", 0)
	if mgr.HasContract() {
		t.Fatal("a fresh goal carries a contract")
	}
	mgr.SetContract(GoalContract{Verification: "x"})
	if !mgr.HasContract() || !NewGoalManager(context.Background(), "c-after", 0).HasContract() {
		t.Fatal("contract not attached or not persisted")
	}
	if mgr.RenderContract() != "- Verification: x" || mgr.StatusLine() != "⊙ Goal (active, 0/20 turns, contract): ship it" {
		t.Fatalf("%q %q", mgr.RenderContract(), mgr.StatusLine())
	}
}

func TestGoalManagerContractPersistenceRoundtrip(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	if _, err := NewGoalManager(ctx, "c-persist", 0).Set("ship it", 0, &GoalContract{Outcome: "O", Verification: "V"}); err != nil {
		t.Fatal(err)
	}
	reloaded := NewGoalManager(ctx, "c-persist", 0)
	if reloaded.State().Contract.Outcome != "O" || reloaded.State().Contract.Verification != "V" {
		t.Fatalf("contract %+v", reloaded.State().Contract)
	}
}

func TestBlockedVerdictBlockedVerdictPausesGoalInsteadOfDone(t *testing.T) {
	mgr := managerWithGoal(t, "blocked-sid", "delete a repository that does not exist", 0)
	pinGoalJudge(t, JudgeResult{Verdict: "blocked", Reason: "the repo does not exist"})
	decision := evaluate(t, mgr, "The repo cannot be deleted: it does not exist.")
	if decision.Verdict != "blocked" || decision.Status != "paused" || decision.ShouldContinue || !strings.Contains(strings.ToLower(decision.Message), "unachievable") {
		t.Fatalf("decision %+v", decision)
	}
	if mgr.State() == nil || mgr.State().Status != "paused" || mgr.State().PausedReason != "judged unachievable: the repo does not exist" {
		t.Fatalf("state %+v", mgr.State())
	}
}

func TestADelegationWaitLiftsWhenABatchReturnsNotOnlyWhenTheTimerRunsOut(t *testing.T) {
	mgr := managerWithGoal(t, "root-wait", "integrate the rounds", 0)
	saved := goalLiveDelegations
	t.Cleanup(func() { goalLiveDelegations = saved })
	live := 4
	goalLiveDelegations = func(string) int { return live }
	if _, err := mgr.WaitForSeconds(1200, "4 batches running", 4); err != nil || !mgr.IsWaiting() {
		t.Fatalf("park %v", err)
	}
	live = 3
	if mgr.IsWaiting() || mgr.State().WaitingUntil != 0.0 || mgr.State().WaitingOnDelegations != 0 {
		t.Fatal("the barrier did not lift when a batch returned")
	}
	if _, err := mgr.WaitForSeconds(1200, "cooldown", 0); err != nil {
		t.Fatal(err)
	}
	live = 0
	if !mgr.IsWaiting() {
		t.Fatal("a plain timer barrier lifted on the delegation count")
	}
}

func TestAddRemoveClearGates(t *testing.T) {
	mgr := managerWithGoal(t, "gate-mgmt-sid", "test goal", 0)
	for _, command := range []string{"echo one", "echo two"} {
		if _, err := mgr.AddGate(command, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(mgr.State().Gates) != 2 || mgr.RenderGates() != "- 1. $ echo one\n- 2. $ echo two" {
		t.Fatalf("gates %q", mgr.RenderGates())
	}
	if removed, err := mgr.RemoveGate(1); err != nil || removed != "echo one" || len(mgr.State().Gates) != 1 {
		t.Fatalf("remove %q %v", removed, err)
	}
	if n, err := mgr.ClearGates(); err != nil || n != 1 || len(mgr.State().Gates) != 0 || mgr.RenderGates() != "(no quality gates — use /goal gate add <command> to require one)" {
		t.Fatalf("clear %d %v", n, err)
	}
	if gate, err := mgr.AddGate(" make test ", 42, 7); err != nil || gate.Command != "make test" || gate.TimeoutSeconds != 42 || gate.MaxRetries != 7 {
		t.Fatalf("gate %+v %v", gate, err)
	}
	if _, err := mgr.AddGate("  ", 0, 0); pyKind(err) != "ValueError" || err.Error() != "gate command is empty" {
		t.Fatalf("empty gate error %v", err)
	}
}

func TestAddGateRequiresActiveGoal(t *testing.T) {
	goalsTestHome(t)
	if _, err := NewGoalManager(context.Background(), "gate-nogoal-sid", 0).AddGate("echo nope", 0, 0); pyKind(err) != "RuntimeError" {
		t.Fatalf("error %v", err)
	}
}

func TestGatesPersistAndReload(t *testing.T) {
	mgr := managerWithGoal(t, "gate-persist-sid", "test goal", 0)
	if _, err := mgr.AddGate("echo persisted", 0, 0); err != nil {
		t.Fatal(err)
	}
	reloaded := NewGoalManager(context.Background(), "gate-persist-sid", 0)
	if len(reloaded.State().Gates) != 1 || reloaded.State().Gates[0].Command != "echo persisted" {
		t.Fatalf("gates %+v", reloaded.State().Gates)
	}
}

func TestFailingGateShortCircuitsJudge(t *testing.T) {
	mgr := managerWithGoal(t, "gate-fail-sid", "test goal", 0)
	if _, err := mgr.AddGate("exit 5", 0, 0); err != nil {
		t.Fatal(err)
	}
	calls := pinGoalJudge(t, JudgeResult{})
	decision := evaluate(t, mgr, "I think it's done!")
	if *calls != 0 || decision.Verdict != "gate_failed" || !decision.ShouldContinue || !strings.Contains(*decision.ContinuationPrompt, "exit 5") ||
		!strings.Contains(strings.ToLower(*decision.ContinuationPrompt), "quality gate") {
		t.Fatalf("decision %+v", decision)
	}
	if decision.Reason != "gate failed (exit 5): $ exit 5" || decision.Message != "✗ Quality gate failed (1/20 turns, attempt 1/3): $ exit 5" {
		t.Fatalf("reason %q message %q", decision.Reason, decision.Message)
	}
	if mgr.RenderGates() != "- 1. $ exit 5 ✗ failing (exit 5, attempt 1/3)" {
		t.Fatalf("gates %q", mgr.RenderGates())
	}
}

func TestPassingGatesFallThroughToJudge(t *testing.T) {
	mgr := managerWithGoal(t, "gate-pass-sid", "test goal", 0)
	if _, err := mgr.AddGate("true", 0, 0); err != nil {
		t.Fatal(err)
	}
	calls := pinGoalJudge(t, JudgeResult{Verdict: "done", Reason: "all good"})
	decision := evaluate(t, mgr, "finished")
	gate := mgr.State().Gates[0]
	if *calls != 1 || decision.Verdict != "done" || gate.Attempts != 0 || gate.LastExitCode == nil || *gate.LastExitCode != 0 || mgr.RenderGates() != "- 1. $ true ✓ passing" {
		t.Fatalf("decision %+v gate %+v", decision, gate)
	}
}

func TestGateRetryExhaustionPausesGoal(t *testing.T) {
	mgr := managerWithGoal(t, "gate-exhaust-sid", "test goal", 0)
	if _, err := mgr.AddGate("exit 1", 0, 0); err != nil {
		t.Fatal(err)
	}
	mgr.State().Gates[0].MaxRetries = 2
	calls := pinGoalJudge(t, JudgeResult{})
	d1, d2, d3 := evaluate(t, mgr, "attempt one"), evaluate(t, mgr, "attempt two"), evaluate(t, mgr, "attempt three")
	if *calls != 0 || !d1.ShouldContinue || !d2.ShouldContinue || d3.Status != "paused" || d3.ShouldContinue || mgr.State().Status != "paused" {
		t.Fatalf("decisions %+v %+v %+v", d1, d2, d3)
	}
	if mgr.State().PausedReason != "quality gate exhausted 2 retries: $ exit 1" ||
		d3.Message != "⏸ Goal paused — quality gate still failing after 2 retries: $ exit 1 (exit 1). Fix it manually or /goal gate remove it, then /goal resume." {
		t.Fatalf("reason %v message %q", mgr.State().PausedReason, d3.Message)
	}
}

func TestFailedGateRerunsWhenUntrackedFileContentChanges(t *testing.T) {
	work := t.TempDir()
	result := filepath.Join(work, "untracked", "result.txt")
	if err := os.MkdirAll(filepath.Dir(result), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "baseline"}} {
		git := exec.Command("git", argv...)
		git.Dir = work
		if out, err := git.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", argv, err, out)
		}
	}
	t.Chdir(work)
	mgr := managerWithGoal(t, "gate-content-sid", "test goal", 0)
	if _, err := mgr.AddGate("grep -q after "+result, 0, 0); err != nil {
		t.Fatal(err)
	}
	calls := pinGoalJudge(t, JudgeResult{Verdict: "done", Reason: "ok"})
	d1 := evaluate(t, mgr, "turn 1")
	if err := os.WriteFile(result, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2 := evaluate(t, mgr, "turn 2")
	if d1.Verdict != "gate_failed" || d2.Verdict != "done" || *calls != 1 || mgr.State().Gates[0].Attempts != 0 {
		t.Fatalf("decisions %+v %+v after %d judge calls", d1, d2, *calls)
	}
}

func TestGateContinuationRespectsTurnBudget(t *testing.T) {
	mgr := managerWithGoal(t, "gate-budget-sid", "budget goal", 1)
	if _, err := mgr.AddGate("exit 1", 0, 0); err != nil {
		t.Fatal(err)
	}
	pinGoalJudge(t, JudgeResult{})
	decision := evaluate(t, mgr, "only turn")
	if decision.Status != "paused" || decision.ShouldContinue || decision.Message != "⏸ Goal paused — 1/1 turns used (a quality gate is still failing). Use /goal resume to keep going, or /goal clear to stop." {
		t.Fatalf("decision %+v", decision)
	}
}

func TestNoGatesBehavesExactlyAsBefore(t *testing.T) {
	mgr := managerWithGoal(t, "gate-none-sid", "test goal", 0)
	calls := pinGoalJudge(t, JudgeResult{Verdict: "continue", Reason: "keep going"})
	if decision := evaluate(t, mgr, "wip"); *calls != 1 || decision.Verdict != "continue" || !decision.ShouldContinue {
		t.Fatalf("decision %+v", decision)
	}
}

// Backend and session directories each holding a check script that prints its directory, the session one failing
func backendAndSession(t *testing.T) (backend, session string) {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	backend, session = filepath.Join(root, "backend"), filepath.Join(root, "session")
	for dir, code := range map[string]string{backend: "0", session: "1"} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("pwd\nexit "+code+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(backend)
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	return backend, session
}

func TestGateRunsInTheSessionWorkspaceNotTheBackendDirectory(t *testing.T) {
	_, session := backendAndSession(t)
	mgr := managerWithGoal(t, "gate-cwd-sid", "test goal", 0)
	mgr.ctx = sessionWorkspaceCtx(t, session)
	if _, err := mgr.AddGate("sh check.sh", 0, 0); err != nil {
		t.Fatal(err)
	}
	calls := pinGoalJudge(t, JudgeResult{Verdict: "done", Reason: "all good"})
	decision := evaluate(t, mgr, "ready")
	gate := mgr.State().Gates[0]
	if *calls != 0 || decision.Verdict != "gate_failed" || *gate.LastExitCode != 1 || !strings.Contains(gate.LastOutputTail, session) {
		t.Fatalf("decision %+v gate %+v", decision, gate)
	}
}

func TestMissingSessionWorkspacePausesInsteadOfRunningElsewhere(t *testing.T) {
	backend, _ := backendAndSession(t)
	missing := filepath.Join(filepath.Dir(backend), "gone")
	mgr := managerWithGoal(t, "gate-missing-cwd-sid", "test goal", 0)
	mgr.ctx = sessionWorkspaceCtx(t, missing)
	if _, err := mgr.AddGate("sh check.sh", 0, 0); err != nil {
		t.Fatal(err)
	}
	calls := pinGoalJudge(t, JudgeResult{Verdict: "done", Reason: "all good"})
	runs := pinGoalRunGate(t)
	decision := evaluate(t, mgr, "ready")
	gate := mgr.State().Gates[0]
	if *runs != 0 || *calls != 0 || decision.Status != "paused" || decision.ShouldContinue || !strings.Contains(decision.Message, missing) || strings.Contains(decision.Message, "still failing") {
		t.Fatalf("decision %+v", decision)
	}
	if !strings.Contains(pyStrOf(mgr.State().PausedReason), missing) || gate.Attempts != 0 || gate.LastExitCode != nil {
		t.Fatalf("reason %v gate %+v", mgr.State().PausedReason, gate)
	}
}

func pyStrOf(v any) string {
	text, _ := v.(string)
	return text
}

func TestGateWithoutASessionWorkspaceKeepsTheLaunchDirectory(t *testing.T) {
	backend, _ := backendAndSession(t)
	mgr := managerWithGoal(t, "gate-launch-cwd-sid", "test goal", 0)
	mgr.ctx = sessionWorkspaceCtx(t, "")
	if _, err := mgr.AddGate("sh check.sh", 0, 0); err != nil {
		t.Fatal(err)
	}
	calls := pinGoalJudge(t, JudgeResult{Verdict: "done", Reason: "all good"})
	decision := evaluate(t, mgr, "ready")
	if *calls != 1 || decision.Verdict != "done" || !strings.Contains(mgr.State().Gates[0].LastOutputTail, backend) {
		t.Fatalf("decision %+v tail %q", decision, mgr.State().Gates[0].LastOutputTail)
	}
}

// Scripts the judge of the kanban loop with one verdict per call, done once the script runs out
func scriptKanbanJudge(t *testing.T, verdicts ...string) {
	t.Helper()
	saved := goalJudgeFn
	t.Cleanup(func() { goalJudgeFn = saved })
	goalJudgeFn = func(context.Context, string, string, JudgeOptions) JudgeResult {
		verdict := "done"
		if len(verdicts) > 0 {
			verdict, verdicts = verdicts[0], verdicts[1:]
		}
		return JudgeResult{Verdict: verdict, Reason: "scripted:" + verdict}
	}
}

func statusOf(status string) func() (*string, error) {
	return func() (*string, error) { return &status, nil }
}

func neverBlock(t *testing.T) func(string) error {
	return func(reason string) error {
		t.Errorf("blocked with %q", reason)
		return nil
	}
}

func TestLoopStopsWhenWorkerAlreadyCompleted(t *testing.T) {
	scriptKanbanJudge(t, "continue")
	var turns []string
	result := kanbanLoop(t, context.Background(), GoalLoopOptions{TaskID: "t1", GoalText: "do the thing", FirstResponse: "done already",
		RunTurn:    func(p string) (any, error) { turns = append(turns, p); return map[string]any{"response": "x"}, nil },
		TaskStatus: statusOf("done"), Block: neverBlock(t)})
	if result.Outcome != "completed_by_worker" || result.Reason != "worker completed the task" || result.TurnsUsed != 1 || len(turns) != 0 {
		t.Fatalf("result %+v turns %v", result, turns)
	}
}

func TestLoopStopsOnJudgeTransportFailure(t *testing.T) {
	saved := goalJudgeFn
	t.Cleanup(func() { goalJudgeFn = saved })
	goalJudgeFn = func(context.Context, string, string, JudgeOptions) JudgeResult {
		return JudgeResult{Verdict: "wait", Reason: "judge transport unreachable", TransportFailed: true}
	}
	var turns []string
	result := kanbanLoop(t, context.Background(), GoalLoopOptions{TaskID: "t1", GoalText: "do the thing", FirstResponse: "first attempt",
		RunTurn:    func(p string) (any, error) { turns = append(turns, p); return map[string]any{"response": "x"}, nil },
		TaskStatus: statusOf("running"), Block: neverBlock(t)})
	if result.Outcome != "stopped" || !strings.Contains(result.Reason, "judge transport failure") || len(turns) != 0 {
		t.Fatalf("result %+v", result)
	}
}

func TestLoopStopsOnWorkerFailedFlag(t *testing.T) {
	scriptKanbanJudge(t, "continue", "continue")
	result := kanbanLoop(t, context.Background(), GoalLoopOptions{TaskID: "t1", GoalText: "do the thing", FirstResponse: "first attempt",
		RunTurn: func(string) (any, error) {
			return map[string]any{"response": "crashed mid-execution", "failed": true, "failure_reason": "context_window_exceeded"}, nil
		}, TaskStatus: statusOf("running"), Block: neverBlock(t)})
	if result.Outcome != "stopped" || result.Reason != "worker failed: context_window_exceeded" {
		t.Fatalf("result %+v", result)
	}
}

func TestLoopStopsOnWorkerFailedFlagDefaultReason(t *testing.T) {
	scriptKanbanJudge(t, "continue", "continue")
	result := kanbanLoop(t, context.Background(), GoalLoopOptions{TaskID: "t1", GoalText: "do the thing", FirstResponse: "first attempt",
		RunTurn: func(string) (any, error) {
			return map[string]any{"response": "crashed without reason", "failed": true}, nil
		},
		TaskStatus: statusOf("running"), Block: neverBlock(t)})
	if result.Outcome != "stopped" || result.Reason != "worker failed: unknown" {
		t.Fatalf("result %+v", result)
	}
}

func TestKanbanGoalLoopNudgesFinalizesBlocksAndBindsThePerTaskAffinityScope(t *testing.T) {
	var logs, blocks, prompts, scopes []string
	run := func(verdicts []string, o GoalLoopOptions) GoalLoopResult {
		logs, blocks, prompts, scopes = nil, nil, nil, nil
		saved := goalJudgeFn
		defer func() { goalJudgeFn = saved }()
		goalJudgeFn = func(ctx context.Context, goal, response string, _ JudgeOptions) JudgeResult {
			scopes = append(scopes, provider.PortalAffinityScope(ctx))
			verdict := "continue"
			if len(verdicts) > 0 {
				verdict, verdicts = verdicts[0], verdicts[1:]
			}
			return JudgeResult{Verdict: verdict, Reason: "why:" + verdict}
		}
		o.TaskID, o.GoalText, o.FirstResponse = "t9", "ship", "first"
		o.Log = func(m string) { logs = append(logs, m) }
		o.Block = func(reason string) error { blocks = append(blocks, reason); return nil }
		if o.RunTurn == nil {
			o.RunTurn = func(p string) (any, error) {
				prompts = append(prompts, p)
				return map[string]any{"response": "more"}, nil
			}
		}
		if o.TaskStatus == nil {
			o.TaskStatus = statusOf("running")
		}
		return kanbanLoop(t, context.Background(), o)
	}
	result := run([]string{"wait", "done", "done"}, GoalLoopOptions{})
	if result != (GoalLoopResult{"blocked_budget", 3, "judged done, never finalized"}) || len(prompts) != 2 || len(blocks) != 1 {
		t.Fatalf("finalize path %+v prompts %d blocks %v", result, len(prompts), blocks)
	}
	if !strings.HasPrefix(prompts[0], "[Continuing toward this kanban task — judge says it is not done yet]\nReason: why:wait\n\n") ||
		!strings.HasPrefix(prompts[1], "[The work looks complete, but the task is still open]\nReason: why:done\n\n") {
		t.Fatalf("prompts %q", prompts)
	}
	if blocks[0] != "Goal-mode worker's output looked complete but it never called kanban_complete after a finalize nudge (why:done)." ||
		!reflect.DeepEqual(scopes, []string{"kanban:t9", "kanban:t9", "kanban:t9"}) || logs[0] != "kanban goal loop: turn 1/20 verdict=continue reason=why:wait" {
		t.Fatalf("blocks %q scopes %v logs %q", blocks, scopes, logs)
	}
	result = run([]string{"blocked"}, GoalLoopOptions{})
	if result != (GoalLoopResult{"blocked_unachievable", 1, "judge verdict blocked: why:blocked"}) || blocks[0] != "Goal-mode judge ruled the goal unachievable: why:blocked" {
		t.Fatalf("blocked path %+v %q", result, blocks)
	}
	result = run(nil, GoalLoopOptions{MaxTurns: 2})
	if result != (GoalLoopResult{"blocked_budget", 2, "turn budget exhausted"}) ||
		blocks[0] != "Goal-mode worker exhausted its turn budget (2/2) without completing the task. Last judge verdict: why:continue" {
		t.Fatalf("budget path %+v %q", result, blocks)
	}
	for status, want := range map[string]GoalLoopResult{"blocked": {"blocked_by_worker", 1, "worker blocked the task"},
		"review": {"review_requested_by_worker", 1, "worker requested review"}, "changes_requested": {"changes_requested_by_reviewer", 1, "reviewer requested changes"},
		"archived": {"stopped", 1, "status=archived"}} {
		if result = run(nil, GoalLoopOptions{TaskStatus: statusOf(status)}); result != want {
			t.Fatalf("status %s gave %+v", status, result)
		}
	}
	if logs[len(logs)-1] == "" {
		t.Fatal("no log line")
	}
	result = run(nil, GoalLoopOptions{TaskStatus: func() (*string, error) { return nil, nil }})
	if result != (GoalLoopResult{"stopped", 1, "status=None"}) || logs[0] != "kanban goal loop: task t9 status=None; stopping" {
		t.Fatalf("missing status %+v %q", result, logs)
	}
	result = run(nil, GoalLoopOptions{TaskStatus: func() (*string, error) { return nil, errors.New("board locked") }})
	if result != (GoalLoopResult{"stopped", 1, "status check failed"}) || logs[0] != "kanban goal loop: status check failed (board locked); stopping" {
		t.Fatalf("status error %+v %q", result, logs)
	}
	result = run(nil, GoalLoopOptions{RunTurn: func(string) (any, error) { return nil, &pyint.Error{Kind: "RuntimeError", Msg: "turn crashed"} }})
	if result.Outcome != "stopped" || !strings.HasPrefix(result.Reason, "run_turn error: ") {
		t.Fatalf("turn error %+v", result)
	}
	bound := provider.WithPortalAffinityScope(context.Background(), "session:abc")
	saved := goalJudgeFn
	defer func() { goalJudgeFn = saved }()
	seen := ""
	goalJudgeFn = func(ctx context.Context, _, _ string, _ JudgeOptions) JudgeResult {
		seen = provider.PortalAffinityScope(ctx)
		return JudgeResult{Verdict: "blocked", Reason: "r"}
	}
	kanbanLoop(t, bound, GoalLoopOptions{TaskID: "t9", GoalText: "g", FirstResponse: "f", TaskStatus: statusOf("ready"), Block: func(string) error { return errors.New("board gone") },
		RunTurn: func(string) (any, error) { return nil, nil }})
	if seen != "session:abc" {
		t.Fatalf("a bound scope was replaced by %q", seen)
	}
}

func TestGoalDecisionDictKeepsThePythonKeyOrder(t *testing.T) {
	prompt := "next"
	dict := GoalDecision{Status: "active", ShouldContinue: true, ContinuationPrompt: &prompt, Verdict: "continue", Reason: "r", Message: "m"}.ToDict()
	if !reflect.DeepEqual(dict.Keys, []string{"status", "should_continue", "continuation_prompt", "verdict", "reason", "message"}) || dict.Vals["continuation_prompt"] != "next" {
		t.Fatalf("dict %v %v", dict.Keys, dict.Vals)
	}
	if none := (GoalDecision{}).ToDict(); none.Vals["status"] != nil || none.Vals["continuation_prompt"] != nil {
		t.Fatalf("empty decision %v", none.Vals)
	}
}

func TestSessionTriggerBarrierRegistryReleasesOnWatchMatchWhileAlive(t *testing.T) {
	goalsTestHome(t)
	reg := tools.DefaultProcessRegistry()
	s := &tools.ProcessSession{ID: "proc_t2", Command: "watcher.sh", TaskID: "t", Cwd: "/tmp", StartedAt: goalTime(), WatchPatterns: []string{"READY"}}
	reg.InsertRunning(s)
	if !reg.IsSessionWaiting("proc_t2") {
		t.Fatal("a live watcher with no match must keep the barrier")
	}
	reg.CheckWatchPatterns(s, "READY\n")
	if s.Exited || reg.IsSessionWaiting("proc_t2") {
		t.Fatalf("exited %v, still waiting after a watch match", s.Exited)
	}
}

func TestGoalLoopJudgeBindsPerTaskAffinityScope(t *testing.T) {
	var seen []string
	saved := goalJudgeFn
	defer func() { goalJudgeFn = saved }()
	goalJudgeFn = func(ctx context.Context, goal, lastResponse string, _ JudgeOptions) JudgeResult {
		seen = append(seen, provider.PortalAffinityScope(ctx))
		return JudgeResult{Verdict: "continue", Reason: "not yet"}
	}
	ctx := context.Background()
	result := kanbanLoop(t, ctx, GoalLoopOptions{TaskID: "task-7", GoalText: "goal",
		RunTurn:    func(string) (any, error) { return "still working", nil },
		TaskStatus: statusOf("running"), Block: func(string) error { return nil }, MaxTurns: 2, FirstResponse: "first"})
	if result.Outcome != "blocked_budget" {
		t.Fatalf("outcome %+v", result)
	}
	if !slices.Equal(seen, []string{"kanban:task-7", "kanban:task-7"}) {
		t.Fatalf("judge scopes %v", seen)
	}
	if scope := provider.PortalAffinityScope(ctx); scope != "" {
		t.Fatalf("the loop left the caller bound to %q", scope)
	}
}
