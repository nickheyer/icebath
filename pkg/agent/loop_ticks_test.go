package agent

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

func wantInterval(t *testing.T, token string, want int64, ok bool) {
	t.Helper()
	got, gotOK := ParseLoopIntervalToken(token)
	if gotOK != ok || (ok && got != want) {
		t.Fatalf("ParseLoopIntervalToken(%q) = %d, %v", token, got, gotOK)
	}
}

func TestMinutes(t *testing.T)                 { wantInterval(t, "5m", 300, true) }
func TestSeconds(t *testing.T)                 { wantInterval(t, "30s", 30, true) }
func TestHours(t *testing.T)                   { wantInterval(t, "2h", 7200, true) }
func TestCompound(t *testing.T)                { wantInterval(t, "1h30m", 5400, true) }
func TestCaseInsensitive(t *testing.T)         { wantInterval(t, "5M", 300, true) }
func TestBareNumberIsNotInterval(t *testing.T) { wantInterval(t, "3", 0, false) }

func TestProseIsNotInterval(t *testing.T) {
	for _, token := range []string{"check", "", "5x"} {
		wantInterval(t, token, 0, false)
	}
}

func TestZeroRejected(t *testing.T) {
	wantInterval(t, "0m", 0, false)
	wantInterval(t, "0s", 0, false)
}

func TestFixedInterval(t *testing.T) {
	p := ParseLoopArgs("5m check the deploy status")
	if p.IntervalSeconds == nil || *p.IntervalSeconds != 300 || p.Prompt != "check the deploy status" || p.Error != "" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestEverySugar(t *testing.T) {
	p := ParseLoopArgs("every 10m /recap")
	if p.IntervalSeconds == nil || *p.IntervalSeconds != 600 || p.Prompt != "/recap" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestSelfPaced(t *testing.T) {
	p := ParseLoopArgs("keep refining the failing test until the suite passes")
	if p.IntervalSeconds != nil || !strings.HasPrefix(p.Prompt, "keep refining") {
		t.Fatalf("parsed %#v", p)
	}
}

func TestTimesFlag(t *testing.T) {
	p := ParseLoopArgs("2m poll CI --times 30")
	if p.IntervalSeconds == nil || *p.IntervalSeconds != 120 || p.Prompt != "poll CI" || p.Times != 30 {
		t.Fatalf("parsed %#v", p)
	}
}

func TestUntilFlag(t *testing.T) {
	p := ParseLoopArgs("5m watch the queue --until queue depth reaches zero")
	if p.IntervalSeconds == nil || *p.IntervalSeconds != 300 || p.Prompt != "watch the queue" || p.Until != "queue depth reaches zero" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestUntilAndTimesTogether(t *testing.T) {
	p := ParseLoopArgs("2m poll --times 5 --until it is green")
	if p.Times != 5 || p.Until != "it is green" || p.Prompt != "poll" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestBadTimes(t *testing.T) {
	if p := ParseLoopArgs("2m poll --times zero"); p.Error == "" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestStartNowWordInPromptKeptVerbatim(t *testing.T) {
	if p := ParseLoopArgs("1h read the file called start-now.md"); p.Prompt != "read the file called start-now.md" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestIntervalOnlyIsError(t *testing.T) {
	if p := ParseLoopArgs("5m"); p.Error == "" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestEmpty(t *testing.T) {
	if p := ParseLoopArgs(""); p.Error != "empty" {
		t.Fatalf("parsed %#v", p)
	}
}

func TestPromptWithLeadingNumberNotEaten(t *testing.T) {
	p := ParseLoopArgs("3 things to verify in the repo")
	if p.IntervalSeconds != nil || !strings.HasPrefix(p.Prompt, "3 things") {
		t.Fatalf("parsed %#v", p)
	}
}

func TestRender(t *testing.T) {
	for in, want := range map[float64]string{30: "30s", 300: "5m", 5400: "1h30m", 90: "1m30s", 0: "0s"} {
		if got := FormatLoopInterval(in); got != want {
			t.Fatalf("FormatLoopInterval(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestMarkerOnOwnLine(t *testing.T) {
	if !ResponseSignalsComplete("Deploy is live.\nLOOP_COMPLETE") {
		t.Fatal("marker missed")
	}
}

func TestMarkerWithTrailingPeriod(t *testing.T) {
	if !ResponseSignalsComplete("done\nLOOP_COMPLETE.") {
		t.Fatal("marker missed")
	}
}

func TestMarkerMidSentenceDoesNotCount(t *testing.T) {
	if ResponseSignalsComplete("I will emit LOOP_COMPLETE when finished") {
		t.Fatal("mid sentence marker counted")
	}
}

func TestNoMarker(t *testing.T) {
	if ResponseSignalsComplete("still building") || ResponseSignalsComplete("") {
		t.Fatal("marker found where there is none")
	}
}

// Loop manager of a fresh session in an isolated home
func newTickLoop(t *testing.T, sessionID, prompt string, interval *int64, times int64, until string) (*LoopManager, *SessionLoopState) {
	t.Helper()
	ctx := context.Background()
	mgr := NewLoopManager(ctx, sessionID)
	st, err := mgr.Set(ctx, prompt, interval, times, until, nil)
	if err != nil {
		t.Fatal(err)
	}
	return mgr, st
}

// Pins the until judge to one verdict, or to a raise when verdict is empty
func pinLoopJudge(t *testing.T, verdict, reason string) {
	t.Helper()
	saved := loopJudgeGoal
	t.Cleanup(func() { loopJudgeGoal = saved })
	loopJudgeGoal = func(context.Context, string, string, goals.JudgeOptions) goals.JudgeResult {
		if verdict == "" {
			panic(&runtimeErrorPanic{"api down"})
		}
		return goals.JudgeResult{Verdict: verdict, Reason: reason}
	}
}

// RuntimeError the faked judge raises
type runtimeErrorPanic struct{ msg string }

func (e *runtimeErrorPanic) Error() string      { return e.msg }
func (e *runtimeErrorPanic) PyTypeName() string { return "RuntimeError" }

func TestDueImmediatelyOnCreate(t *testing.T) {
	goalsTestHome(t)
	mgr, _ := newTickLoop(t, "t1", "poll", loopInterval(300), 0, "")
	if !mgr.IsDue() {
		t.Fatal("new loop not due")
	}
}

func TestDueAfterInterval(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t2", "poll", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() - 1
	if !mgr.IsDue() {
		t.Fatal("elapsed loop not due")
	}
}

func TestNotDueOnceRescheduled(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t2a", "poll", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() + 300
	if mgr.IsDue() {
		t.Fatal("rescheduled loop due")
	}
}

func TestSelfPacedDueImmediatelyOnCreate(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t2c", "keep refining", nil, 0, "")
	if st.Mode != "self_paced" || !mgr.IsDue() {
		t.Fatalf("mode %v due %v", st.Mode, mgr.IsDue())
	}
}

func TestFireMarksAwaitingAndBlocksRefire(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	mgr, st := newTickLoop(t, "t3", "poll the build", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() - 1
	wakeup, ok := mgr.FireTick(ctx)
	if !ok || !strings.Contains(wakeup, "[/loop wakeup #1") || !strings.Contains(wakeup, "poll the build") || !strings.Contains(wakeup, "LOOP_COMPLETE") {
		t.Fatalf("wakeup %q", wakeup)
	}
	if !mgr.State().AwaitingResponse || mgr.IsDue() {
		t.Fatal("tick did not block a refire")
	}
	if _, again := mgr.FireTick(ctx); again {
		t.Fatal("double fire")
	}
}

func TestSlashPromptReturnedRaw(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t4", "/recap", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() - 1
	if wakeup, ok := mgr.FireTick(context.Background()); !ok || wakeup != "/recap" {
		t.Fatalf("wakeup %q", wakeup)
	}
}

func TestAbandonTickRollsBack(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	mgr, st := newTickLoop(t, "t5", "poll", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() - 1
	mgr.FireTick(ctx)
	mgr.AbandonTick(ctx)
	if mgr.State().AwaitingResponse || mgr.State().TicksFired != 0 {
		t.Fatalf("state %#v", mgr.State())
	}
}

// Fires the due tick of a fresh loop and completes it with the response
func fireAndComplete(t *testing.T, mgr *LoopManager, st *SessionLoopState, response string) LoopDecision {
	t.Helper()
	ctx := context.Background()
	st.NextDueAt = nowSeconds() - 1
	mgr.FireTick(ctx)
	return mgr.CompleteTick(ctx, response)
}

func TestCompleteTickMarkerStops(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t6", "poll", loopInterval(300), 0, "")
	if d := fireAndComplete(t, mgr, st, "The deploy is live.\nLOOP_COMPLETE"); !d.Stopped || d.Status != "done" {
		t.Fatalf("decision %#v", d)
	}
}

func TestCompleteTickTimesCap(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t7", "poll", loopInterval(300), 1, "")
	if d := fireAndComplete(t, mgr, st, "still building"); !d.Stopped || d.Status != "done" || !strings.Contains(d.Message, "1/1") {
		t.Fatalf("decision %#v", d)
	}
}

func TestCompleteTickContinuesAndReschedules(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t8", "poll", loopInterval(300), 0, "")
	d := fireAndComplete(t, mgr, st, "still building")
	if d.Stopped || d.Status != "active" || mgr.State().AwaitingResponse || mgr.State().NextDueAt <= nowSeconds()+250 {
		t.Fatalf("decision %#v state %#v", d, mgr.State())
	}
}

func TestCompleteTickMaxTicksPauses(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t9", "poll", loopInterval(300), 0, "")
	st.MaxTicks = 1
	if d := fireAndComplete(t, mgr, st, "still building"); !d.Stopped || d.Status != "paused" {
		t.Fatalf("decision %#v", d)
	}
}

func TestUntilJudgeDoneStops(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t10", "poll", loopInterval(300), 0, "the suite is green")
	pinLoopJudge(t, "done", "suite green")
	if d := fireAndComplete(t, mgr, st, "All 500 tests passed."); !d.Stopped || d.Status != "done" {
		t.Fatalf("decision %#v", d)
	}
}

func TestUntilJudgeContinueLoops(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t11", "poll", loopInterval(300), 0, "the suite is green")
	pinLoopJudge(t, "continue", "3 failures")
	if d := fireAndComplete(t, mgr, st, "3 tests still failing"); d.Stopped {
		t.Fatalf("decision %#v", d)
	}
}

func TestUntilJudgeBlockedPauses(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t11b", "poll", loopInterval(300), 0, "the deleted repo's CI is green")
	pinLoopJudge(t, "blocked", "repo no longer exists")
	d := fireAndComplete(t, mgr, st, "The repository was deleted; there is no CI to watch.")
	if !d.Stopped || d.Status != "paused" || !strings.Contains(d.Message, "unachievable") {
		t.Fatalf("decision %#v", d)
	}
}

func TestUntilJudgeErrorFailsOpen(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "t12", "poll", loopInterval(300), 0, "green")
	pinLoopJudge(t, "", "")
	if d := fireAndComplete(t, mgr, st, "some output"); d.Stopped {
		t.Fatalf("decision %#v", d)
	}
}

func TestBackoffDoublesOnUnchangedAndResetsOnChange(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "sp1", "watch the queue", nil, 0, "")
	floor := st.CurrentDelay
	if st.Mode != "self_paced" {
		t.Fatalf("mode %v", st.Mode)
	}
	for _, step := range []struct {
		response string
		want     float64
	}{{"queue depth is 5", floor}, {"queue depth is 5", floor * 2}, {"queue depth is 5", floor * 4}, {"queue depth is 2 — draining", floor}} {
		fireAndComplete(t, mgr, mgr.State(), step.response)
		if mgr.State().CurrentDelay != step.want {
			t.Fatalf("after %q delay %v want %v", step.response, mgr.State().CurrentDelay, step.want)
		}
	}
}

func TestTimestampOnlyChangesDoNotReset(t *testing.T) {
	goalsTestHome(t)
	mgr, st := newTickLoop(t, "sp2", "watch", nil, 0, "")
	floor := st.CurrentDelay
	fireAndComplete(t, mgr, st, "Still building. Checked at 14:02:33")
	fireAndComplete(t, mgr, mgr.State(), "Still building. Checked at 14:07:33")
	if mgr.State().CurrentDelay != floor*2 {
		t.Fatalf("delay %v want %v", mgr.State().CurrentDelay, floor*2)
	}
}

func TestMinIntervalEnforced(t *testing.T) {
	goalsTestHome(t)
	_, st := newTickLoop(t, "c1", "poll", loopInterval(1), 0, "")
	if st.IntervalSeconds < float64(loopMinIntervalSeconds(context.Background())) {
		t.Fatalf("interval %v", st.IntervalSeconds)
	}
}

func TestPauseResume(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	mgr, st := newTickLoop(t, "c2", "poll", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() - 1
	if _, err := mgr.Pause(ctx, loopDefaultPauseReason); err != nil {
		t.Fatal(err)
	}
	if mgr.IsActive() || mgr.IsDue() {
		t.Fatal("paused loop still active")
	}
	mgr.Resume(ctx)
	if !mgr.IsActive() {
		t.Fatal("resumed loop inactive")
	}
}

func TestPausedMidTickClearsAwaiting(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	mgr, st := newTickLoop(t, "c3", "poll", loopInterval(300), 0, "")
	st.NextDueAt = nowSeconds() - 1
	mgr.FireTick(ctx)
	if _, err := mgr.Pause(ctx, "user-interrupted"); err != nil {
		t.Fatal(err)
	}
	if mgr.State().AwaitingResponse {
		t.Fatal("pause kept the tick in flight")
	}
}

// Goal manager of a session with a goal set
func setGoal(t *testing.T, sessionID, goal string) *goals.GoalManager {
	t.Helper()
	gm := goals.NewGoalManager(context.Background(), sessionID, goals.DefaultGoalMaxTurns)
	if _, err := gm.Set(goal, 0, nil); err != nil {
		t.Fatal(err)
	}
	return gm
}

func TestActiveGoalBlocksTick(t *testing.T) {
	goalsTestHome(t)
	setGoal(t, "g1", "finish the migration")
	if !GoalBlocksLoopTick(context.Background(), "g1") {
		t.Fatal("active goal did not block")
	}
}

func TestNoGoalDoesNotBlock(t *testing.T) {
	goalsTestHome(t)
	if GoalBlocksLoopTick(context.Background(), "g2") {
		t.Fatal("missing goal blocked")
	}
}

func TestPausedGoalDoesNotBlock(t *testing.T) {
	goalsTestHome(t)
	setGoal(t, "g3", "finish it").Pause("user-paused")
	if GoalBlocksLoopTick(context.Background(), "g3") {
		t.Fatal("paused goal blocked")
	}
}

func TestParkedGoalDoesNotBlock(t *testing.T) {
	goalsTestHome(t)
	if _, err := setGoal(t, "g4", "finish it").WaitForSeconds(3600, "waiting on CI", 0); err != nil {
		t.Fatal(err)
	}
	if GoalBlocksLoopTick(context.Background(), "g4") {
		t.Fatal("parked goal blocked")
	}
}

// Dispatches one loop command and fails the test on an error
func dispatchLoop(t *testing.T, mgr *LoopManager, args string, route *pyjson.Object) LoopCommandResult {
	t.Helper()
	result, err := DispatchLoopCommand(context.Background(), mgr, args, route)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCreateSelfPaced(t *testing.T) {
	goalsTestHome(t)
	result := dispatchLoop(t, NewLoopManager(context.Background(), "d2"), "keep fixing the tests", nil)
	if !result.Created || !strings.Contains(result.Output, "Self-paced") {
		t.Fatalf("result %#v", result)
	}
}

func TestCreateFiresImmediately(t *testing.T) {
	goalsTestHome(t)
	mgr := NewLoopManager(context.Background(), "d2a")
	result := dispatchLoop(t, mgr, "1h check the deploy", nil)
	if !result.Created || !strings.Contains(result.Output, "Loop set") || !strings.Contains(result.Output, "fires now") || !mgr.IsDue() {
		t.Fatalf("result %#v", result)
	}
}

func TestStatusEmpty(t *testing.T) {
	goalsTestHome(t)
	result := dispatchLoop(t, NewLoopManager(context.Background(), "d3"), "", nil)
	if result.Created || !strings.Contains(result.Output, "No loop set") {
		t.Fatalf("result %#v", result)
	}
}

func TestPauseResumeStop(t *testing.T) {
	goalsTestHome(t)
	mgr := NewLoopManager(context.Background(), "d4")
	dispatchLoop(t, mgr, "5m poll", nil)
	for _, step := range []struct{ args, want string }{{"pause", "paused"}, {"resume", "resumed"}, {"stop", "stopped"}} {
		if out := strings.ToLower(dispatchLoop(t, mgr, step.args, nil).Output); !strings.Contains(out, step.want) {
			t.Fatalf("%s gave %q", step.args, out)
		}
	}
	if out := dispatchLoop(t, mgr, "stop", nil).Output; !strings.Contains(out, "No active loop") {
		t.Fatalf("second stop gave %q", out)
	}
}

func TestRouteStored(t *testing.T) {
	goalsTestHome(t)
	route := pyjson.ObjectOf("platform", "telegram", "chat_id", "42", "chat_type", "private")
	dispatchLoop(t, NewLoopManager(context.Background(), "d5"), "5m ping", route)
	if loaded := LoadLoop(context.Background(), "d5"); loaded == nil || !pyrepr.Equal(loaded.Route, route) {
		t.Fatalf("loaded %#v", loaded)
	}
}

func TestBadTimesError(t *testing.T) {
	goalsTestHome(t)
	result := dispatchLoop(t, NewLoopManager(context.Background(), "d7"), "5m poll --times banana", nil)
	if result.Created || !strings.Contains(result.Output, "--times") {
		t.Fatalf("result %#v", result)
	}
}

// Session database at the default path of the isolated home
func metaPrefixDB(t *testing.T) *state.SessionDB {
	t.Helper()
	h := goalsTestHome(t)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Meta rows of a prefix scan as a key value map
func metaPrefixRows(t *testing.T, db *state.SessionDB, prefix string) map[string]any {
	t.Helper()
	entries, err := db.ListMetaPrefix(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]any{}
	for _, e := range entries {
		rows[e.Key] = e.Value
	}
	return rows
}

func TestPrefixScan(t *testing.T) {
	db := metaPrefixDB(t)
	ctx := context.Background()
	for _, kv := range [][2]string{{"lmp-test:aaa", "1"}, {"lmp-test:bbb", "2"}, {"other:aaa", "3"}} {
		if err := db.SetMeta(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if rows := metaPrefixRows(t, db, "lmp-test:"); !reflect.DeepEqual(rows, map[string]any{"lmp-test:aaa": "1", "lmp-test:bbb": "2"}) {
		t.Fatalf("rows %#v", rows)
	}
}

func TestWildcardsEscaped(t *testing.T) {
	db := metaPrefixDB(t)
	ctx := context.Background()
	for _, kv := range [][2]string{{"pre%fix:x", "1"}, {"prefix:y", "2"}} {
		if err := db.SetMeta(ctx, kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := db.ListMetaPrefix(ctx, "pre%")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != "pre%fix:x" || entries[0].Value != "1" {
		t.Fatalf("entries %#v", entries)
	}
}

func TestEmptyPrefix(t *testing.T) {
	db := metaPrefixDB(t)
	entries, err := db.ListMetaPrefix(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries %#v", entries)
	}
}
