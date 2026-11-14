package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Worker call answered by a canned function of the method and params
func cannedCall(fn func(method string, params *pyjson.Object) (any, error)) WorkerCall {
	return func(_ context.Context, method string, params *pyjson.Object) (any, error) { return fn(method, params) }
}

// Worker call that persists straight into the canonical store, the hermes_state conftest rpc
func dbWorkerCall(db *state.SessionDB) WorkerCall {
	return func(ctx context.Context, _ string, p *pyjson.Object) (any, error) {
		num := func(k string) int64 { n, _ := p.GetOr(k, int64(0)).(int64); return n }
		text := func(k string) string { s, _ := p.GetOr(k, "").(string); return s }
		return db.MutateWorkerExecution(ctx, state.WorkerReceipt{Epoch: num("epoch"), ExecutionID: text("execution_id"),
			SessionID: text("session_id"), Generation: num("generation"), Sequence: num("sequence"), Operation: text("operation"),
			Payload: p.GetOr("payload", nil)})
	}
}

func mustStore(t *testing.T, rpc WorkerCall, scope *pyjson.Object, dir string, maxBytes int) *RuntimeSessionStore {
	t.Helper()
	store, err := NewRuntimeSessionStore(context.Background(), rpc, scope, dir, nil, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func persistenceReason(err error) string {
	var wpe *tools.WorkerPersistenceError
	if errors.As(err, &wpe) {
		return wpe.Reason
	}
	return ""
}

func readJournal(t *testing.T, store *RuntimeSessionStore) *pyjson.Object {
	t.Helper()
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestOutboxFreezesFailedPayloadAndReopensExclusively(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	scope := pyjson.ObjectOf("session_id", "s", "execution_id", "w", "epoch", int64(1))
	rows := []map[string]any{{"role": "user", "content": "frozen"}}
	unavailable := cannedCall(func(string, *pyjson.Object) (any, error) {
		rows[0]["content"] = "caller changed"
		return nil, &workerStoreError{"TimeoutError", "offline"}
	})
	store := mustStore(t, unavailable, scope, dir, 0)
	if _, err := store.AppendMessagesBatch(context.Background(), "s", rows, state.AppendGuards{}, nil); err == nil || err.Error() != "offline" {
		t.Fatalf("append %v", err)
	}
	disk := readJournal(t, store)
	pending, _ := disk.GetOr("pending", nil).([]any)
	first := pending[0].(*pyjson.Object).GetOr("payload", nil).(*pyjson.Object).GetOr("messages", nil).([]any)[0].(*pyjson.Object)
	if first.GetOr("content", nil) != "frozen" {
		t.Fatalf("journal content %v", first)
	}
	if !pyjson.Equal(store.journal.GetOr("pending", nil), disk.GetOr("pending", nil)) {
		t.Fatal("in memory journal differs from disk")
	}
	if _, err := NewRuntimeSessionStore(context.Background(), unavailable, scope, dir, nil, 0); persistenceReason(err) != "outbox_in_use" {
		t.Fatalf("second open %v", err)
	}
	if err := store.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 1}); persistenceReason(err) != "offline" {
		t.Fatalf("sticky failure %v", err)
	}
	if err := store.Close(); persistenceReason(err) != "offline" {
		t.Fatalf("close %v", err)
	}
	var seen []*pyjson.Object
	commit := cannedCall(func(_ string, p *pyjson.Object) (any, error) {
		seen = append(seen, p)
		return pyjson.ObjectOf("count", int64(1), "annotations", []any{pyjson.ObjectOf("_row_id", int64(42))}), nil
	})
	restored := mustStore(t, commit, scope, dir, 0)
	defer restored.Close()
	if results, err := restored.RetryPending(context.Background()); err != nil || len(results) != 0 {
		t.Fatalf("retry after reopen %v %v", results, err)
	}
	msg := seen[0].GetOr("payload", nil).(*pyjson.Object).GetOr("messages", nil).([]any)[0].(*pyjson.Object)
	if msg.GetOr("content", nil) != "frozen" {
		t.Fatalf("replayed %v", msg)
	}
	if got := readJournal(t, restored).GetOr("pending", nil).([]any); len(got) != 0 {
		t.Fatalf("pending after replay %v", got)
	}
}

func TestOutboxHasPrivatePosixMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	store := mustStore(t, cannedCall(func(string, *pyjson.Object) (any, error) { return pyjson.NewObject(), nil }),
		pyjson.ObjectOf("session_id", "s"), filepath.Join(t.TempDir(), "private"), 0)
	defer store.Close()
	info, err := os.Stat(store.path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %v %v", info, err)
	}
}

func TestOutboxCapacityFailureDoesNotAdvanceOrDiscard(t *testing.T) {
	unavailable := cannedCall(func(string, *pyjson.Object) (any, error) { return nil, &workerStoreError{"TimeoutError", "offline"} })
	store := mustStore(t, unavailable, pyjson.ObjectOf("session_id", "s"), filepath.Join(t.TempDir(), "private"), 500)
	before, _ := os.ReadFile(store.path)
	_, err := store.AppendMessagesBatch(context.Background(), "s", []map[string]any{{"role": "user", "content": strings.Repeat("x", 1000)}}, state.AppendGuards{}, nil)
	if persistenceReason(err) != "outbox_full" {
		t.Fatalf("append %v", err)
	}
	after, _ := os.ReadFile(store.path)
	if string(after) != string(before) {
		t.Fatal("journal changed on capacity failure")
	}
	if f := store.Failure(); f == nil || *f != "outbox_full" {
		t.Fatalf("failure %v", f)
	}
	if err := store.Close(); persistenceReason(err) != "outbox_full" {
		t.Fatalf("close %v", err)
	}
}

func TestWorkerTurnWaitCallbacksRemainLocal(t *testing.T) {
	var attempts []*pyjson.Object
	var notices []float64
	rpc := cannedCall(func(_ string, p *pyjson.Object) (any, error) {
		attempts = append(attempts, p)
		return pyjson.ObjectOf("value", len(attempts) > 1), nil
	})
	store := mustStore(t, rpc, pyjson.ObjectOf("session_id", "s"), filepath.Join(t.TempDir(), "private"), 0)
	defer store.Close()
	wait := state.NewTurnLeaseWait()
	wait.WaitSeconds, wait.PollIntervalSeconds = 3, .01
	wait.OnWait = func(waited float64) error { notices = append(notices, waited); return nil }
	if ok, err := store.AcquireSessionTurnLease("s", "holder", wait); !ok || err != nil {
		t.Fatalf("acquire %v %v", ok, err)
	}
	if len(notices) == 0 || len(attempts) != 2 {
		t.Fatalf("notices %v attempts %d", notices, len(attempts))
	}
	for _, row := range attempts {
		payload := row.GetOr("payload", nil).(*pyjson.Object)
		if payload.Len() != 2 || payload.GetOr("holder", nil) == nil || payload.GetOr("ttl_seconds", nil) == nil {
			t.Fatalf("payload %v", payload)
		}
	}
	abort := state.NewTurnLeaseWait()
	abort.ShouldAbort = func() (bool, error) { return true, nil }
	if ok, err := store.AcquireSessionTurnLease("s", "holder", abort); ok || err != nil {
		t.Fatalf("aborted acquire %v %v", ok, err)
	}
}

func TestOutboxFilesystemFailureIsSticky(t *testing.T) {
	store := mustStore(t, cannedCall(func(string, *pyjson.Object) (any, error) { return pyjson.NewObject(), nil }),
		pyjson.ObjectOf("session_id", "s"), filepath.Join(t.TempDir(), "private"), 0)
	defer store.closeOwner()
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := store.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 1})
	if err == nil {
		t.Fatal("journal replace onto a directory succeeded")
	}
	if f := store.Failure(); f == nil || *f != err.Error() {
		t.Fatalf("failure %v want %q", f, err.Error())
	}
}

func TestWorkerStorePersistsAndRestoresTheToolsPrefixPin(t *testing.T) {
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "owned", Source: "cli", SystemPrompt: "prefix"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Kind: "compute", AdoptionSecret: "test-secret"}); err != nil {
		t.Fatal(err)
	}
	scope := pyjson.ObjectOf("epoch", epoch, "execution_id", "worker", "session_id", "owned", "generation", int64(0))
	store := mustStore(t, dbWorkerCall(db), scope, filepath.Join(t.TempDir(), "outbox"), 0)
	defer store.closeOwner()
	var defs []any
	for _, n := range []string{"terminal", "tool_search", "tool_describe", "tool_call"} {
		defs = append(defs, pyjson.ObjectOf("type", "function", "function", pyjson.ObjectOf("name", n, "parameters", pyjson.NewObject())))
	}
	pin := pyjson.ObjectOf("version", mcp.ToolPinVersion(), "tools", defs)
	if err := store.UpdateSessionToolNames("owned", pin); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "direct", Source: "cli", SystemPrompt: "prefix"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionToolNames("direct", pin); err != nil {
		t.Fatal(err)
	}
	owned, _ := db.GetSession(ctx, "owned")
	direct, _ := db.GetSession(ctx, "direct")
	ownedPin, _ := pyjson.Loads(owned.GetOr("tool_names", "").(string))
	directPin, _ := pyjson.Loads(direct.GetOr("tool_names", "").(string))
	if !pyjson.Equal(ownedPin, directPin) {
		t.Fatalf("pins differ %v %v", ownedPin, directPin)
	}
	viaStore, err := store.GetSession(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := pyjson.Loads(viaStore.GetOr("tool_names", "").(string))
	if !pyjson.Equal(restored.(*pyjson.Object).GetOr("tools", nil), defs) {
		t.Fatalf("restored tools %v", restored)
	}
	if err := store.UpdateSessionToolNames("foreign", []any{"terminal"}); persistenceReason(err) != "permission_denied" {
		t.Fatalf("foreign %v", err)
	}
	if err := store.UpdateSessionToolNames("owned", []any{pyjson.ObjectOf("not", "a name")}); err == nil || !strings.Contains(err.Error(), "invalid_params") {
		t.Fatalf("invalid pin %v", err)
	}
}
