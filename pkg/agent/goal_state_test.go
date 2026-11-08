package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Empties the per home session database cache
func clearGoalsDBCache() {
	goalsDBMu.Lock()
	goalsDBCache = map[string]*state.DB{}
	goalsDBMu.Unlock()
}

// Isolated home with a fresh cache, every registry handle under it closed afterwards
func goalsTestHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, h)
	clearGoalsDBCache()
	t.Cleanup(func() {
		clearGoalsDBCache()
		state.CloseAllUnder(h)
	})
	return h
}

func TestGoalStateSubgoalsBackcompatOldStateMetaRowLoadsWithoutSubgoals(t *testing.T) {
	legacy, err := pyjson.Dumps(pyjson.ObjectOf("goal", "do a thing", "status", "active", "turns_used", int64(2),
		"max_turns", int64(20), "created_at", 1.0, "last_turn_at", 2.0, "consecutive_parse_failures", int64(0)))
	if err != nil {
		t.Fatal(err)
	}
	st, err := GoalStateFromJSON(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if st.Goal != "do a thing" {
		t.Fatalf("goal %v", st.Goal)
	}
	if st.Subgoals == nil || len(st.Subgoals) != 0 {
		t.Fatalf("subgoals %#v", st.Subgoals)
	}
}

func TestMigrateGoalToSessionMigratesActiveGoalToChild(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	SaveGoal(ctx, "parent-sid", NewGoalState("ship the feature"))
	if !migrateGoalToSession(ctx, "parent-sid", "child-sid", "compression") {
		t.Fatal("migration refused")
	}
	child := LoadGoal(ctx, "child-sid")
	if child == nil || child.Goal != "ship the feature" {
		t.Fatalf("child %#v", child)
	}
	parent := LoadGoal(ctx, "parent-sid")
	if parent == nil || parent.Status != "cleared" {
		t.Fatalf("parent %#v", parent)
	}
}

func TestMigrateGoalToSessionDoesNotClobberExistingChildGoal(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	SaveGoal(ctx, "p3", NewGoalState("parent goal"))
	SaveGoal(ctx, "c3", NewGoalState("child already has one"))
	if migrateGoalToSession(ctx, "p3", "c3", "") {
		t.Fatal("migration clobbered the child")
	}
	if got := LoadGoal(ctx, "c3"); got == nil || got.Goal != "child already has one" {
		t.Fatalf("child %#v", got)
	}
}

func TestSessionDbCacheAfterProfileDeleteDeleteThenRecreateGetsALiveStore(t *testing.T) {
	h := goalsTestHome(t)
	profile := filepath.Join(h, "profiles", "p1")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := home.WithOverride(context.Background(), profile)
	defer state.CloseAllUnder(profile)
	SaveGoal(ctx, "s1", NewGoalState("before delete"))
	stale := goalsSessionDB(ctx)
	if n := state.CloseAllUnder(profile); n != 1 {
		t.Fatalf("closed %d handles", n)
	}
	if err := os.RemoveAll(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	SaveGoal(ctx, "s2", NewGoalState("after recreate"))
	if goalsSessionDB(ctx) == stale {
		t.Fatal("cache kept the torn down handle")
	}
	if _, err := os.Stat(filepath.Join(profile, "state.db")); err != nil {
		t.Fatal(err)
	}
	if got := LoadGoal(ctx, "s2"); got == nil || got.Goal != "after recreate" {
		t.Fatalf("goal %#v", got)
	}
}

func TestGoalContractSerializationRoundtripWithContract(t *testing.T) {
	st := NewGoalState("ship it")
	st.Contract = GoalContract{Verification: "pytest passes", Constraints: "don't break the API"}
	raw, err := st.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := GoalStateFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Goal != "ship it" || restored.Contract.Verification != "pytest passes" ||
		restored.Contract.Constraints != "don't break the API" || !restored.HasContract() {
		t.Fatalf("restored %#v", restored)
	}
}

func TestGoalContractSerializationOldRowWithoutContractLoadsClean(t *testing.T) {
	st, err := GoalStateFromJSON(`{"goal": "old goal", "status": "active", "turns_used": 2}`)
	if err != nil {
		t.Fatal(err)
	}
	if st.Goal != "old goal" || st.TurnsUsed != 2 || !st.Contract.IsEmpty() || st.HasContract() {
		t.Fatalf("state %#v", st)
	}
}

func TestGoalContractSerializationRenderBlockOmitsEmptyFields(t *testing.T) {
	block := GoalContract{Outcome: "X", Verification: "Y"}.RenderBlock()
	if !strings.Contains(block, "Outcome: X") || !strings.Contains(block, "Verification: Y") || strings.Contains(block, "Constraints") {
		t.Fatalf("block %q", block)
	}
}

func TestGoalSessionDbIsTheRegistrySharedHandle(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	db := goalsSessionDB(ctx)
	if db == nil {
		t.Fatal("no session database")
	}
	defer func() {
		clearGoalsDBCache()
		state.ReleaseOrClose(db)
	}()
	shared := false
	for _, s := range state.LiveSharedSessionDBs() {
		shared = shared || s == db
	}
	if !shared {
		t.Fatal("handle is not the registry shared one")
	}
	if goalsSessionDB(ctx) != db {
		t.Fatal("second lookup missed the cache")
	}
}

// Goroutine number of the caller, the threading.get_ident analog
func goalsTestGoroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	field := strings.Fields(strings.TrimPrefix(string(buf), "goroutine "))[0]
	id, _ := strconv.ParseUint(field, 10, 64)
	return id
}

// Records the goroutine of every database construction
type goalsConstructionLog struct {
	mu sync.Mutex
	on []uint64
}

func (l *goalsConstructionLog) ids() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uint64(nil), l.on...)
}

// Swaps the database acquire for one that records its goroutine and sleeps, waiting out bootstraps afterwards
func patchGoalsAcquire(t *testing.T, delay time.Duration) *goalsConstructionLog {
	t.Helper()
	log := &goalsConstructionLog{}
	prev := goalsAcquireSessionDB
	goalsAcquireSessionDB = func(ctx context.Context, h string) (*state.DB, error) {
		log.mu.Lock()
		log.on = append(log.on, goalsTestGoroutineID())
		log.mu.Unlock()
		time.Sleep(delay)
		return state.Acquire(ctx, home.StatePath(h))
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			goalsDBMu.Lock()
			n := len(goalsDBInflight)
			goalsDBMu.Unlock()
			if n == 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		goalsAcquireSessionDB = prev
	})
	return log
}

func TestLoopThreadCacheMissConstructsOffLoop(t *testing.T) {
	goalsTestHome(t)
	log := patchGoalsAcquire(t, 0)
	loopID := goalsTestGoroutineID()
	inline := goalsSessionDB(pyctx.WithRunningLoop(context.Background()))
	if inline == nil {
		t.Fatal("grace window did not return the fast init")
	}
	deadline := time.Now().Add(5 * time.Second)
	populated := func() bool {
		goalsDBMu.Lock()
		defer goalsDBMu.Unlock()
		return len(goalsDBCache) > 0
	}
	for time.Now().Before(deadline) && !populated() {
		time.Sleep(20 * time.Millisecond)
	}
	if !populated() {
		t.Fatal("background bootstrap never populated the cache")
	}
	ids := log.ids()
	if len(ids) == 0 {
		t.Fatal("SessionDB never constructed")
	}
	for _, id := range ids {
		if id == loopID {
			t.Fatal("SessionDB was constructed on the event-loop thread")
		}
	}
}

func TestLoopThreadCacheHitReturnsCachedInstance(t *testing.T) {
	goalsTestHome(t)
	patchGoalsAcquire(t, 0)
	ctx := context.Background()
	sentinel, err := goalsAcquireSessionDB(ctx, home.Dir(ctx))
	if err != nil {
		t.Fatal(err)
	}
	goalsDBMu.Lock()
	goalsDBCache[home.Dir(ctx)] = sentinel
	goalsDBMu.Unlock()
	if got := goalsSessionDB(pyctx.WithRunningLoop(ctx)); got != sentinel {
		t.Fatalf("got %p want %p", got, sentinel)
	}
}

func TestWorkerThreadConstructsInline(t *testing.T) {
	goalsTestHome(t)
	log := patchGoalsAcquire(t, 0)
	if db := goalsSessionDB(context.Background()); db == nil {
		t.Fatal("no session database")
	}
	if ids := log.ids(); len(ids) != 1 || ids[0] != goalsTestGoroutineID() {
		t.Fatalf("constructed on %v", ids)
	}
}

func TestSlowConstructionDoesNotBlockTheLoop(t *testing.T) {
	goalsTestHome(t)
	prevInit, prevLoop := goalsDBInitWait, goalsDBLoopWait
	goalsDBInitWait, goalsDBLoopWait = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { goalsDBInitWait, goalsDBLoopWait = prevInit, prevLoop })
	patchGoalsAcquire(t, 1500*time.Millisecond)
	ctx := pyctx.WithRunningLoop(context.Background())
	t0 := time.Now()
	result := goalsSessionDB(ctx)
	elapsed := time.Since(t0)
	t0 = time.Now()
	result2 := goalsSessionDB(ctx)
	elapsed2 := time.Since(t0)
	if result != nil {
		t.Fatal("contended init must degrade to None")
	}
	if elapsed >= time.Second {
		t.Fatalf("kick call blocked for %s — past the one-time init window", elapsed)
	}
	if result2 != nil {
		t.Fatal("contended init must keep degrading to None")
	}
	if elapsed2 >= time.Second {
		t.Fatalf("in-flight call blocked for %s — watchdog territory", elapsed2)
	}
	if elapsed <= elapsed2 {
		t.Fatal("kick call should wait the one-time init window; in-flight calls the short one")
	}
}

func TestStateRowsMatchPythonJSONLayout(t *testing.T) {
	cases := []struct {
		dump func() (string, error)
		want string
	}{
		{NewGoalState("café").ToJSON, `{"goal": "café", "status": "active", "turns_used": 0, "max_turns": 20, "created_at": 0.0, "last_turn_at": 0.0, "last_verdict": null, "last_reason": null, "paused_reason": null, "consecutive_parse_failures": 0, "consecutive_transport_failures": 0, "subgoals": [], "waiting_on_pid": null, "waiting_on_session": null, "waiting_until": 0.0, "waiting_on_delegations": 0, "waiting_reason": null, "waiting_since": 0.0, "contract": {"outcome": "", "verification": "", "constraints": "", "boundaries": "", "stop_when": ""}, "gates": []}`},
		{NewHeartbeatState("p", 60).ToJSON, `{"prompt": "p", "interval_seconds": 60, "status": "active", "created_at": 0.0, "last_fired_at": 0.0, "fire_count": 0}`},
		{NewSessionLoopState("p").ToJSON, `{"prompt": "p", "status": "active", "mode": "interval", "interval_seconds": 0.0, "current_delay": 0.0, "times": 0, "until": "", "max_ticks": 100, "ticks_fired": 0, "created_at": 0.0, "last_fired_at": 0.0, "next_due_at": 0.0, "awaiting_response": false, "last_response_digest": "", "paused_reason": null, "last_stop_reason": null, "route": {}}`},
	}
	for _, c := range cases {
		got, err := c.dump()
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("got  %s\nwant %s", got, c.want)
		}
	}
	gated := NewGoalState("g")
	gated.Gates = []GoalGate{NewGoalGate("make test")}
	raw, err := gated.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(raw, `"gates": [{"command": "make test", "timeout_seconds": 300, "max_retries": 3, "attempts": 0, "last_exit_code": null, "last_output_tail": ""}]}`) {
		t.Fatalf("gates %s", raw)
	}
	back, err := GoalStateFromJSON(raw)
	if err != nil || len(back.Gates) != 1 || back.Gates[0] != NewGoalGate("make test") {
		t.Fatalf("gates back %#v %v", back, err)
	}
	if _, err := GoalStateFromJSON(`{"goal": "g", "gates": 5}`); err == nil || err.Error() != "'int' object is not iterable" {
		t.Fatalf("scalar gates error %v", err)
	}
	if _, err := GoalStateFromJSON(`[]`); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("list row error %v", err)
	}
	loop, err := SessionLoopStateFromJSON(`{"prompt": "p", "max_ticks": null, "times": "7", "awaiting_response": 1}`)
	if err != nil || loop.MaxTicks != 0 || loop.Times != 7 || !loop.AwaitingResponse {
		t.Fatalf("loop coercion %#v %v", loop, err)
	}
}
