package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs a re-exec child mode of the async delegation tests and exits, the hermes subprocess producers
func asyncDelegationChildEntry() {
	if mode := os.Getenv("ICEBATH_ASYNC_CHILD"); mode != "" {
		os.Exit(asyncChildMain(mode, os.Getenv("ICEBATH_HOME"), os.Getenv("ICEBATH_ASYNC_ID")))
	}
}

// Child process body, each mode printing its result as the last stdout line
func asyncChildMain(mode, home, delegationID string) int {
	env := &Env{Home: home, Values: &sync.Map{}}
	ctx := context.Background()
	waitIdle := func() {
		deadline := time.Now().Add(10 * time.Second)
		for AsyncDelegationActiveCount() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	switch mode {
	case "orphan":
		r, err := DispatchAsyncDelegationUnit(ctx, env, AsyncDispatch{Goal: "adversarial review", Role: "leaf", Model: ptr("m"),
			SessionKey: "bot-chat", ParentSessionID: ptr("bot-parent"),
			Runner: func(context.Context, *Env) (map[string]any, error) {
				return nil, errors.New("interrupted: waiting for model response")
			}})
		if err != nil {
			fmt.Println(err)
			return 1
		}
		waitIdle()
		fmt.Println(r["delegation_id"])
		return 0
	case "producer":
		r, err := DispatchAsyncDelegationUnit(ctx, env, AsyncDispatch{Goal: "restart", Role: "leaf", Model: ptr("m"),
			SessionKey: "owner-session", ParentSessionID: ptr("durable-parent"),
			Runner: runnerOf(map[string]any{"status": "completed", "summary": "after restart"})})
		if err != nil {
			fmt.Println(err)
			return 1
		}
		waitIdle()
		fmt.Println(r["delegation_id"])
		return 0
	case "consumer":
		if _, ok := DefaultProcessRegistry().CompletionQueue.GetNowait(); ok {
			fmt.Println("import must not restore")
			return 2
		}
		q := newEventQueue()
		if _, err := RestoreUndeliveredCompletions(env, q); err != nil {
			fmt.Println(err)
			return 1
		}
		evt, ok := q.GetNowait()
		if !ok {
			fmt.Println("nothing restored")
			return 3
		}
		text, err := pyjson.Dumps(evt, pyjson.SortKeys(true))
		if err != nil {
			fmt.Println(err)
			return 1
		}
		fmt.Println(text)
		return 0
	case "acker":
		ok, err := MarkCompletionDelivered(env, delegationID)
		fmt.Println(ok, err)
		if !ok || err != nil {
			return 1
		}
		return 0
	case "probe":
		DefaultProcessRegistry().RestoreCompletions()
		fmt.Println(DefaultProcessRegistry().CompletionQueue.Len())
		return 0
	}
	fmt.Println("unknown mode", mode)
	return 4
}

// Runs the test binary in one child mode under home and returns its last stdout line
func asyncRunChild(t *testing.T, mode, home, delegationID string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "ICEBATH_ASYNC_CHILD="+mode, "ICEBATH_HOME="+home, "ICEBATH_ASYNC_ID="+delegationID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child %s failed: %v\n%s", mode, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// One column of a ledger row
func asyncColumn(t *testing.T, env *Env, delegationID, column string) any {
	t.Helper()
	var v any
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT "+column+" FROM async_delegations WHERE delegation_id=?", delegationID).Scan(&v)
	})
	if err != nil {
		t.Fatalf("column %s of %s: %v", column, delegationID, err)
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// Sets ledger columns of a row
func asyncSetColumns(t *testing.T, env *Env, delegationID string, cols map[string]any) {
	t.Helper()
	keys := make([]string, 0, len(cols))
	for k := range cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sets := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)+1)
	for _, k := range keys {
		sets = append(sets, k+"=?")
		args = append(args, cols[k])
	}
	args = append(args, delegationID)
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := asyncExec(ctx, conn, "UPDATE async_delegations SET "+strings.Join(sets, ", ")+" WHERE delegation_id=?", args...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func asyncColumnFloat(t *testing.T, env *Env, delegationID, column string) float64 {
	t.Helper()
	f, ok := asyncFloat(asyncColumn(t, env, delegationID, column))
	if !ok {
		t.Fatalf("column %s is not numeric", column)
	}
	return f
}

// Raw sqlite connection on a state.db path outside the ledger helpers
func asyncRawConn(t *testing.T, path string) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		db.Close()
	})
	return conn
}

func asyncPragma(t *testing.T, conn state.SQLiteConn, pragma string) any {
	t.Helper()
	var v any
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", pragma, err)
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// Shrinks the stale monitor cadence so the tests run in milliseconds
func asyncFastStaleMonitor(t *testing.T, idle, inTool, grace float64) {
	t.Helper()
	prevInterval, prevIdle, prevInTool, prevGrace := asyncStaleCheckInterval, asyncStaleIdleSeconds, asyncStaleInToolSeconds, asyncStallGraceSeconds
	asyncStaleCheckInterval, asyncStaleIdleSeconds, asyncStaleInToolSeconds, asyncStallGraceSeconds = 30*time.Millisecond, idle, inTool, grace
	t.Cleanup(func() {
		resetAsyncDelegationForTests()
		asyncStaleCheckInterval, asyncStaleIdleSeconds, asyncStaleInToolSeconds, asyncStallGraceSeconds = prevInterval, prevIdle, prevInTool, prevGrace
	})
}

// Records still running, stalling or finalizing, the ones whose ledger writes may be in flight
func asyncLiveRecordCount() int {
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	n := 0
	for _, r := range asyncRecords {
		if asyncLiveStates[r.status()] {
			n++
		}
	}
	return n
}

// Waits until every record reached a terminal status, so no worker still writes the ledger
func asyncWaitSettled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for asyncLiveRecordCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if asyncLiveRecordCount() != 0 {
		t.Fatal("records still live")
	}
}

// Releases blocked runners and waits for their completions to settle
func asyncFinishBlocked(t *testing.T, gate chan struct{}) {
	t.Helper()
	close(gate)
	asyncWaitSettled(t)
	drainCompletionQueue()
}

func asyncWaitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for AsyncDelegationActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if AsyncDelegationActiveCount() != 0 {
		t.Fatal("delegations still active")
	}
}

func asyncDrainAll(q *EventQueue) []map[string]any {
	var out []map[string]any
	for {
		evt, ok := q.GetNowait()
		if !ok {
			return out
		}
		out = append(out, evt)
	}
}

func asyncIDs(events []map[string]any) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, asyncText(e["delegation_id"]))
	}
	return out
}

func TestSchemaInitPreservesSharedStateDbJournalMode(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn := asyncRawConn(t, filepath.Join(t.TempDir(), "state.db"))
	if mode := asyncPragma(t, conn, "journal_mode=DELETE"); mode != "delete" {
		t.Fatalf("journal mode %v", mode)
	}
	if err := asyncInitializeSchema(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if mode := asyncPragma(t, conn, "journal_mode"); mode != "delete" {
		t.Fatalf("schema init changed the journal mode to %v", mode)
	}
	var name string
	if err := conn.QueryRowContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name='async_delegations'").Scan(&name); err != nil || name != "async_delegations" {
		t.Fatalf("table %q %v", name, err)
	}
}

func TestSchemaInitPreservesSharedStateDbWalMode(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	conn := asyncRawConn(t, filepath.Join(t.TempDir(), "state.db"))
	if mode := asyncPragma(t, conn, "journal_mode=WAL"); mode != "wal" {
		t.Fatalf("journal mode %v", mode)
	}
	if err := asyncInitializeSchema(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	if mode := asyncPragma(t, conn, "journal_mode"); mode != "wal" {
		t.Fatalf("schema init changed the journal mode to %v", mode)
	}
	var name string
	if err := conn.QueryRowContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name='async_delegations'").Scan(&name); err != nil || name != "async_delegations" {
		t.Fatalf("table %q %v", name, err)
	}
}

func TestConnectPreservesWalAndAppliesMacosDurabilityBarriers(t *testing.T) {
	env := asyncTestEnv(t)
	seed := asyncRawConn(t, filepath.Join(env.Home, "state.db"))
	if mode := asyncPragma(t, seed, "journal_mode=WAL"); mode != "wal" {
		t.Fatalf("journal mode %v", mode)
	}
	seed.Close()
	conn, err := asyncConnect(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if mode := asyncPragma(t, conn, "journal_mode"); mode != "wal" {
		t.Fatalf("connect replaced the journal mode with %v", mode)
	}
	if runtime.GOOS == "darwin" {
		if sync, full := asyncPragma(t, conn, "synchronous"), asyncPragma(t, conn, "checkpoint_fullfsync"); sync != int64(2) || full != int64(1) {
			t.Fatalf("durability barriers synchronous=%v checkpoint_fullfsync=%v", sync, full)
		}
	}
}

func TestAsyncExecutorWorkersAreDaemonThreads(t *testing.T) {
	env := asyncTestEnv(t)
	gate := make(chan struct{})
	defer asyncFinishBlocked(t, gate)
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "daemon check", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-gate
			return map[string]any{"status": "completed", "summary": "done"}, nil
		}})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	asyncRecordsMu.Lock()
	pool := asyncExecutor
	asyncRecordsMu.Unlock()
	if pool == nil {
		t.Fatal("no executor")
	}
	pool.mu.Lock()
	workers := pool.workers
	pool.mu.Unlock()
	if workers < 1 {
		t.Fatalf("daemon pool runs %d workers", workers)
	}
}

func TestRichReinjectionBlockIsSelfContained(t *testing.T) {
	env := asyncTestEnv(t)
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "Compute the meaning of life",
		Context: ptr("User is a philosopher. Respond tersely."), Toolsets: []string{"web"}, Role: "leaf", Model: ptr("test-model"), MaxAsyncChildren: 3,
		Runner: runnerOf(map[string]any{"status": "completed", "summary": "The answer is 42.", "api_calls": 7, "duration_seconds": 3.5, "model": "test-model"})})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	evt := drainFor(nil, 5*time.Second)
	if evt == nil {
		t.Fatal("no completion")
	}
	text := FormatProcessNotification(evt)
	for _, needle := range []string{"Compute the meaning of life", "User is a philosopher", "The answer is 42."} {
		if !strings.Contains(text, needle) {
			t.Fatalf("missing %q in %q", needle, text)
		}
	}
}

func TestInterruptAllSignalsRunningChildren(t *testing.T) {
	env := asyncTestEnv(t)
	release := make(chan struct{})
	interrupted := 0
	var mu sync.Mutex
	r, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "long task", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 3,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-release
			return map[string]any{"status": "interrupted", "summary": nil, "error": "cancelled"}, nil
		},
		InterruptFn: func() error {
			mu.Lock()
			interrupted++
			mu.Unlock()
			close(release)
			return nil
		}})
	if err != nil || r["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", r, err)
	}
	if n := InterruptAllAsyncDelegations("test"); n != 1 {
		t.Fatalf("interrupt_all = %d", n)
	}
	mu.Lock()
	count := interrupted
	mu.Unlock()
	if count != 1 {
		t.Fatalf("interrupt_fn ran %d times", count)
	}
	evt := drainFor(r["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "interrupted" {
		t.Fatalf("completion %v", evt)
	}
}

func TestProgressingRunnerIsNeverStalled(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFastStaleMonitor(t, 0.15, 0.3, 0.15)
	gate := make(chan struct{})
	var mu sync.Mutex
	ticks := 0
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "slow child", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-gate
			return map[string]any{"status": "completed", "summary": "done", "api_calls": 7}, nil
		},
		ProgressFn: func() (any, bool, error) {
			mu.Lock()
			ticks++
			n := ticks
			mu.Unlock()
			return []any{n, nil}, false, nil
		}})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	time.Sleep(600 * time.Millisecond)
	if AsyncDelegationActiveCount() != 1 || DefaultProcessRegistry().CompletionQueue.Len() != 0 {
		t.Fatalf("progressing child treated as stalled: active %d queue %d", AsyncDelegationActiveCount(), DefaultProcessRegistry().CompletionQueue.Len())
	}
	close(gate)
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "completed" || evt["summary"] != "done" {
		t.Fatalf("completion %v", evt)
	}
}

func TestStallingRunnerThatHonorsInterruptKeepsItsResult(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFastStaleMonitor(t, 0.15, 0.3, 5.0)
	interrupted := make(chan struct{})
	var once sync.Once
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "responsive child", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			select {
			case <-interrupted:
			case <-time.After(10 * time.Second):
			}
			return map[string]any{"status": "interrupted", "summary": "partial work saved", "api_calls": 3}, nil
		},
		InterruptFn: func() error { once.Do(func() { close(interrupted) }); return nil },
		ProgressFn:  func() (any, bool, error) { return []any{3, nil}, false, nil }})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "interrupted" || evt["summary"] != "partial work saved" || asyncText(evt["api_calls"]) != "3" {
		t.Fatalf("completion %v", evt)
	}
	if AsyncDelegationActiveCount() != 0 {
		t.Fatal("record still active")
	}
}

func TestStreamingChildCountsAsAlive(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFastStaleMonitor(t, 0.15, 0.3, 0.15)
	gate := make(chan struct{})
	var mu sync.Mutex
	ts := 1000.0
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "streaming child", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-gate
			return map[string]any{"status": "completed", "summary": "streamed"}, nil
		},
		ProgressFn: func() (any, bool, error) {
			mu.Lock()
			ts += 1.0
			now := ts
			mu.Unlock()
			return []any{[]any{1, nil, now}}, false, nil
		}})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	time.Sleep(600 * time.Millisecond)
	if AsyncDelegationActiveCount() != 1 || DefaultProcessRegistry().CompletionQueue.Len() != 0 {
		t.Fatalf("streaming child stalled: active %d queue %d", AsyncDelegationActiveCount(), DefaultProcessRegistry().CompletionQueue.Len())
	}
	close(gate)
	if evt := drainFor(res["delegation_id"], 5*time.Second); evt == nil || evt["status"] != "completed" {
		t.Fatalf("completion %v", evt)
	}
}

func TestStalledEventCarriesStructuredStallMetadata(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFastStaleMonitor(t, 0.15, 0.3, 0.15)
	gate := make(chan struct{})
	defer asyncFinishBlocked(t, gate)
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "stall metadata", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			select {
			case <-gate:
			case <-time.After(10 * time.Second):
			}
			return map[string]any{}, nil
		},
		ProgressFn: func() (any, bool, error) { return []any{0, "terminal"}, true, nil }})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "stalled" {
		t.Fatalf("event %v", evt)
	}
	quiet, _ := asyncFloat(evt["stalled_after_quiet_seconds"])
	if quiet < 0.3 || evt["stall_threshold_seconds"] != asyncStaleInToolSeconds || evt["stall_phase"] != "in_tool" || evt["stall_grace_seconds"] != asyncStallGraceSeconds {
		t.Fatalf("stall metadata %v", evt)
	}
}

func TestListAsyncDelegationsExposesLiveActivity(t *testing.T) {
	env := asyncTestEnv(t)
	prev := asyncStaleCheckInterval
	asyncStaleCheckInterval = 30 * time.Millisecond
	t.Cleanup(func() {
		resetAsyncDelegationForTests()
		asyncStaleCheckInterval = prev
	})
	gate := make(chan struct{})
	defer asyncFinishBlocked(t, gate)
	baseTS := asyncTimeNow() - 12.0
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "live listing", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			select {
			case <-gate:
			case <-time.After(10 * time.Second):
			}
			return map[string]any{}, nil
		},
		ProgressFn: func() (any, bool, error) { return []any{[]any{3, "web_search", baseTS}}, true, nil }})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	time.Sleep(100 * time.Millisecond)
	var item map[string]any
	for _, d := range ListAsyncDelegations() {
		if d["delegation_id"] == res["delegation_id"] {
			item = d
		}
	}
	if item == nil || item["status"] != "running" || item["in_tool"] != true {
		t.Fatalf("item %v", item)
	}
	if _, ok := item["seconds_since_progress"]; !ok {
		t.Fatalf("seconds_since_progress missing %v", item)
	}
	children, _ := item["children_activity"].([]any)
	if len(children) != 1 {
		t.Fatalf("children %v", item["children_activity"])
	}
	child := asyncMap(children[0])
	since, _ := asyncFloat(child["seconds_since_activity"])
	if asyncText(child["api_calls"]) != "3" || child["current_tool"] != "web_search" || since < 10 || since > 20 {
		t.Fatalf("child %v", child)
	}
	for k := range item {
		if strings.HasPrefix(k, "_") || k == "progress_fn" || k == "interrupt_fn" {
			t.Fatalf("listing leaks %s", k)
		}
	}
}

func TestInToolStallUsesHigherThreshold(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFastStaleMonitor(t, 0.1, 10.0, 0.1)
	gate := make(chan struct{})
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "long tool child", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-gate
			return map[string]any{"status": "completed", "summary": "long tool finished"}, nil
		},
		ProgressFn: func() (any, bool, error) { return []any{1, "terminal"}, true, nil }})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	time.Sleep(500 * time.Millisecond)
	if AsyncDelegationActiveCount() != 1 || DefaultProcessRegistry().CompletionQueue.Len() != 0 {
		t.Fatalf("in tool child stalled at the idle threshold: active %d queue %d", AsyncDelegationActiveCount(), DefaultProcessRegistry().CompletionQueue.Len())
	}
	close(gate)
	if evt := drainFor(res["delegation_id"], 5*time.Second); evt == nil || evt["status"] != "completed" {
		t.Fatalf("completion %v", evt)
	}
}

func TestRealProcessRestartRestoresOwnedCompletionOnce(t *testing.T) {
	home := t.TempDir()
	delegationID := asyncRunChild(t, "producer", home, "")
	if !strings.HasPrefix(delegationID, "deleg_") {
		t.Fatalf("producer printed %q", delegationID)
	}
	var evt map[string]any
	if err := json.Unmarshal([]byte(asyncRunChild(t, "consumer", home, "")), &evt); err != nil {
		t.Fatal(err)
	}
	if evt["delegation_id"] != delegationID || evt["session_key"] != "owner-session" || evt["parent_session_id"] != "durable-parent" || evt["summary"] != "after restart" {
		t.Fatalf("restored event %v", evt)
	}
	asyncRunChild(t, "acker", home, delegationID)
	if got := asyncRunChild(t, "probe", home, ""); got != "0" {
		t.Fatalf("delivered completion restored again: queue %s", got)
	}
}

func TestConcurrentDispatchRespectsCapacity(t *testing.T) {
	env := asyncTestEnv(t)
	gate := make(chan struct{})
	defer asyncFinishBlocked(t, gate)
	blocker := func(context.Context, *Env) (map[string]any, error) {
		<-gate
		return map[string]any{"status": "completed", "summary": "x"}, nil
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var statuses []string
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "race", Role: "leaf", Model: ptr("m"), Runner: blocker, MaxAsyncChildren: 1})
			mu.Lock()
			statuses = append(statuses, asyncText(r["status"]))
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	sort.Strings(statuses)
	if strings.Join(statuses, ",") != "dispatched,rejected" {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestUnitsOfOneCallShareASingleCapacitySlot(t *testing.T) {
	env := asyncTestEnv(t)
	gate := make(chan struct{})
	defer asyncFinishBlocked(t, gate)
	blocker := func(context.Context, *Env) (map[string]any, error) {
		<-gate
		return map[string]any{"results": []any{}, "total_duration_seconds": 0}, nil
	}
	common := func(id string, indexes []int, slot string) AsyncDispatch {
		return AsyncDispatch{DelegationID: id, Goals: []string{"a", "b"}, IsBatch: true, Role: "leaf", Model: ptr("m"), Runner: blocker, MaxAsyncChildren: 1,
			TaskIndexes: indexes, SlotKey: slot}
	}
	first, _ := DispatchAsyncDelegationBatch(context.Background(), env, common("deleg_call-1", []int{0}, ""))
	second, _ := DispatchAsyncDelegationBatch(context.Background(), env, common("deleg_call-2", []int{1}, "deleg_call-1"))
	other, _ := DispatchAsyncDelegationBatch(context.Background(), env, common("deleg_other", nil, ""))
	if first["status"] != "dispatched" || second["status"] != "dispatched" || other["status"] != "rejected" {
		t.Fatalf("statuses %v %v %v", first["status"], second["status"], other["status"])
	}
	if AsyncDelegationActiveTaskCount() != 2 {
		t.Fatalf("active tasks %d", AsyncDelegationActiveTaskCount())
	}
}

func TestUnitsBeyondSlotCountStillStartAndAreNotStalledWhileQueued(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFastStaleMonitor(t, 0.3, 1200.0, 0.2)
	release := make(chan struct{})
	defer asyncFinishBlocked(t, release)
	var mu sync.Mutex
	var started []string
	blocker := func(uid string) func(context.Context, *Env) (map[string]any, error) {
		return func(context.Context, *Env) (map[string]any, error) {
			mu.Lock()
			started = append(started, uid)
			mu.Unlock()
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
			return map[string]any{"results": []any{map[string]any{"task_index": 0, "status": "completed"}}, "total_duration_seconds": 0}, nil
		}
	}
	frozen := func() (any, bool, error) { return []any{[]any{0, nil, nil}}, false, nil }
	common := func(id, slot string, runner func(context.Context, *Env) (map[string]any, error), progress func() (any, bool, error), max int) AsyncDispatch {
		return AsyncDispatch{DelegationID: id, Goals: []string{"x"}, IsBatch: true, Role: "leaf", Model: ptr("m"), Runner: runner, ProgressFn: progress, MaxAsyncChildren: max, SlotKey: slot}
	}
	DispatchAsyncDelegationBatch(context.Background(), env, common("deleg_c-1", "", blocker("c-1"), frozen, 1))
	DispatchAsyncDelegationBatch(context.Background(), env, common("deleg_c-2", "deleg_c-1", blocker("c-2"), nil, 1))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(started)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), started...)
	mu.Unlock()
	sort.Strings(got)
	if strings.Join(got, ",") != "c-1,c-2" {
		t.Fatalf("started %v", got)
	}
	prev := asyncGetExecutorFn
	asyncGetExecutorFn = func(int) *DaemonPool { return asyncExecutor }
	t.Cleanup(func() { asyncGetExecutorFn = prev })
	DispatchAsyncDelegationBatch(context.Background(), env, common("deleg_q", "", blocker("q"), frozen, 3))
	time.Sleep(700 * time.Millisecond)
	asyncRecordsMu.Lock()
	status := asyncRecords["deleg_q"].status()
	asyncRecordsMu.Unlock()
	mu.Lock()
	queuedStarted := false
	for _, s := range started {
		queuedStarted = queuedStarted || s == "q"
	}
	mu.Unlock()
	if status != "running" || queuedStarted {
		t.Fatalf("queued unit status %q started %v", status, queuedStarted)
	}
}

func TestPersistFailureStillDeliversResultAndFreesSlot(t *testing.T) {
	env := asyncTestEnv(t)
	prev := asyncPersistCompletionFn
	asyncPersistCompletionFn = func(*Env, map[string]any, map[string]any) error { return errors.New("database is locked") }
	t.Cleanup(func() { asyncPersistCompletionFn = prev })
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "g", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: runnerOf(map[string]any{"status": "completed", "summary": "done"})})
	if err != nil {
		t.Fatal(err)
	}
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "completed" || evt["summary"] != "done" {
		t.Fatalf("completion %v", evt)
	}
	asyncWaitSettled(t)
	asyncRecordsMu.Lock()
	status := asyncRecords[asyncText(res["delegation_id"])].status()
	asyncRecordsMu.Unlock()
	if status != "completed" {
		t.Fatalf("record status %q", status)
	}
}

func TestPruneNeverEvictsLiveRecords(t *testing.T) {
	asyncTestEnv(t)
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	for status, ts := range map[string]float64{"stalling": 1.0, "finalizing": 2.0, "running": 3.0} {
		asyncRecords["live-"+status] = &asyncRecord{m: map[string]any{"delegation_id": "live-" + status, "status": status, "dispatched_at": ts, "completed_at": nil}}
		asyncRecordOrder = append(asyncRecordOrder, "live-"+status)
	}
	for i := 0; i <= asyncMaxRetainedCompleted; i++ {
		id := fmt.Sprintf("done-%d", i)
		asyncRecords[id] = &asyncRecord{m: map[string]any{"delegation_id": id, "status": "completed", "dispatched_at": 100.0 + float64(i), "completed_at": 200.0 + float64(i)}}
		asyncRecordOrder = append(asyncRecordOrder, id)
	}
	pruneCompletedLocked()
	live := 0
	for _, id := range []string{"live-stalling", "live-finalizing", "live-running"} {
		if asyncRecords[id] != nil {
			live++
		}
	}
	if live != 3 || asyncRecords["done-0"] != nil || len(asyncRecords)-live != asyncMaxRetainedCompleted {
		t.Fatalf("survivors %d live, %d total", live, len(asyncRecords))
	}
}

// Log records at or above debug captured for the async delegation tests
func asyncCaptureLogs(t *testing.T) *recordingHandler {
	t.Helper()
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}
