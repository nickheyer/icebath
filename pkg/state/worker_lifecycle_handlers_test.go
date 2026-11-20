package state

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Registered compute worker on the reserved session owned beside a foreign session
func setupStore(t *testing.T) (*SessionDB, *workerStore) {
	t.Helper()
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	for _, n := range []NewSession{{ID: "owned", Source: "cli", Cwd: "/reserved", ProfileName: "fixture"}, {ID: "foreign", Source: "cli", SystemPrompt: "private"}} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "fixture")
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Generation: 0, Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	return db, newWorkerStore(t, db, scope)
}

// Rows of a read only statement on the direct store
func rowsOf(t *testing.T, db *SessionDB, query string, args ...any) []*pyjson.Object {
	t.Helper()
	rows, err := db.readAll(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestReservedConstructorBackfillAndEndAreReceipted(t *testing.T) {
	ctx := context.Background()
	db, store := setupStore(t)
	defer db.Close()
	created, err := store.createSession("owned", "cli", map[string]any{"model": "requested", "system_prompt": "prefix",
		"model_config": map[string]any{"max_iterations": int64(5)}, "cwd": "/reserved", "profile_name": "fixture"})
	if err != nil || created != "owned" {
		t.Fatalf("create %v %v", created, err)
	}
	if row := mustSession(t, db, "owned"); !pyjson.Equal(rowValue(row, "model"), "requested") || !pyjson.Equal(rowValue(row, "system_prompt"), "prefix") {
		t.Fatalf("backfilled row %v", row)
	}
	if _, err := store.createSession("owned", "cli", map[string]any{"model": "ignored", "system_prompt": "ignored",
		"model_config": map[string]any{"max_iterations": int64(10)}}); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "owned"), "model"), "requested") {
		t.Fatal("keep existing constructor overwrote the model")
	}
	if v, err := db.GetSessionModelConfigValue(ctx, "owned", "max_iterations", nil); err != nil || !pyjson.Equal(v, 5) {
		t.Fatalf("max_iterations %v %v", v, err)
	}
	statuses, err := store.sessionLifecycleStatuses([]string{"owned"})
	if err != nil || !pyjson.Equal(statuses, map[string]any{"owned": "empty"}) {
		t.Fatalf("statuses %v %v", statuses, err)
	}
	if _, err := store.createSession("foreign", "cli", nil); err == nil || err.Error() != "permission_denied" {
		t.Fatalf("foreign create %v", err)
	}
	for _, payload := range []map[string]any{{"source": "cli", "parent_session_id": "foreign"}, {"source": "cli", "profile_name": "foreign"},
		{"source": "cli", "cwd": "/foreign"}, {"source": "cli", "model_config": map[string]any{"_delegate_from": "foreign"}}} {
		if r := receiptRefusal(t, db, store.rawReceipt("session.create", payload)); r != "permission_denied" && r != "invalid_params" {
			t.Fatalf("%v -> %s", payload, r)
		}
	}
	if _, err := store.appendMessagesBatch("owned", []map[string]any{{"role": "assistant", "content": "done", "finish_reason": "stop"}}); err != nil {
		t.Fatal(err)
	}
	direct, err := db.SessionLifecycleStatuses(ctx, []string{"owned"})
	if err != nil {
		t.Fatal(err)
	}
	statuses, _ = store.sessionLifecycleStatuses([]string{"owned"})
	if !pyjson.Equal(statuses, map[string]any{"owned": direct["owned"]}) {
		t.Fatalf("statuses %v != %v", statuses, direct)
	}
	original := store.rpc
	store.rpc = lostAckRPC(original, "lost-ack")
	if err := store.endSession("owned", "agent_close"); err == nil || err.Error() != "lost-ack" {
		t.Fatalf("lost ack %v", err)
	}
	ended := mustSession(t, db, "owned")
	store.rpc = original
	if _, err := store.retryPending(); err != nil {
		t.Fatal(err)
	}
	if err := store.endSession("owned", "late-reason"); err != nil {
		t.Fatal(err)
	}
	row := mustSession(t, db, "owned")
	if !pyjson.Equal(rowValue(row, "ended_at"), rowValue(ended, "ended_at")) || !pyjson.Equal(rowValue(row, "end_reason"), "agent_close") {
		t.Fatalf("end stamp %v", row)
	}
	finished, err := store.finish()
	if err != nil || !pyjson.Equal(resultValue(finished, "status"), "terminal") {
		t.Fatalf("finish %v %v", finished, err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "foreign"), "system_prompt"), "private") {
		t.Fatal("foreign prompt changed")
	}
}

func TestLateFinalizersCannotMutateSuccessorOrForeignRows(t *testing.T) {
	ctx := context.Background()
	db, store := setupStore(t)
	defer db.Close()
	if err := store.endSession("owned", "agent_close"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReopenSession(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
	if err := db.writeSQL(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id=?", []any{"owned"}, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []int64{1, 2} {
		in := store.scope
		in.Sequence, in.Operation, in.Payload = sequence, "session.end", pyjson.ObjectOf("end_reason", "agent_close")
		if r := receiptRefusal(t, db, in); r != "stale_generation" {
			t.Fatalf("sequence %d -> %s", sequence, r)
		}
	}
	foreign := store.scope
	foreign.SessionID, foreign.Sequence, foreign.Operation, foreign.Payload = "foreign", 2, "session.end", pyjson.ObjectOf("end_reason", "agent_close")
	if r := receiptRefusal(t, db, foreign); r != "permission_denied" {
		t.Fatal(r)
	}
	for _, sid := range []string{"owned", "foreign"} {
		if rowValue(mustSession(t, db, sid), "ended_at") != nil {
			t.Fatalf("%s ended", sid)
		}
	}
}

func TestWorkerLabelsKeepRankAndRejectLateGenerations(t *testing.T) {
	ctx := context.Background()
	db, store := setupStore(t)
	defer db.Close()
	mustTitle := func(ok bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !mustTitle(store.setAutoTitle("owned", "derived", "derived")) || !mustTitle(store.setAutoTitle("owned", "generated", "llm")) {
		t.Fatal("auto titles refused")
	}
	if mustTitle(store.setAutoTitle("owned", "late-derived", "derived")) {
		t.Fatal("lower rank overwrote")
	}
	if !mustTitle(store.setSessionTitle("owned", "manual")) {
		t.Fatal("manual title refused")
	}
	if mustTitle(store.setAutoTitle("owned", "late-llm", "llm")) {
		t.Fatal("llm overwrote manual")
	}
	if source, err := store.getSessionTitleSource("owned"); err != nil || source != "user" {
		t.Fatalf("source %v %v", source, err)
	}
	next, err := store.getNextTitleInLineage("manual")
	direct, derr := db.GetNextTitleInLineage(ctx, "manual")
	if err != nil || derr != nil || next != direct {
		t.Fatalf("next title %v %v %v %v", next, err, direct, derr)
	}
	if !mustTitle(store.setSessionTitleSource("owned", "llm")) {
		t.Fatal("title source rewrite refused")
	}
	at := now() + 10
	if err := store.touchSessionActivity("owned", at, "  active   work ", "unknown"); err != nil {
		t.Fatal(err)
	}
	expected := mustSession(t, db, "owned")
	if err := store.touchSessionActivity("owned", at-1, "older", nil); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "owned"), "last_activity_description"), rowValue(expected, "last_activity_description")) {
		t.Fatal("older activity moved the clock backwards")
	}
	if err := store.clearSessionActivityLabels("owned"); err != nil {
		t.Fatal(err)
	}
	row := mustSession(t, db, "owned")
	if !pyjson.Equal(rowValue(row, "last_activity_at"), at) || !pyjson.Equal(rowValue(row, "last_activity_description"), "") {
		t.Fatalf("cleared activity %v", row)
	}
	if err := store.touchSessionActivity("owned", at+1, "successor", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.writeSQL(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id=?", []any{"owned"}, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		operation string
		payload   map[string]any
	}{
		{"session.title", map[string]any{"title": "late", "source": "user"}},
		{"session.title_source", map[string]any{"source": "derived"}},
		{"session.activity_clear", map[string]any{}},
		{"session.activity", map[string]any{"ts": at + 2, "description": "late", "provenance": nil}},
	} {
		if r := receiptRefusal(t, db, store.rawReceipt(c.operation, c.payload)); r != "stale_generation" {
			t.Fatalf("%s -> %s", c.operation, r)
		}
	}
	row = mustSession(t, db, "owned")
	if !pyjson.Equal(rowValue(row, "title"), "manual") || !pyjson.Equal(rowValue(row, "last_activity_description"), "successor") {
		t.Fatalf("late writes landed %v", row)
	}
}

func TestWorkerRouteAndAPIContentPreserveExactUsageAndTargetFences(t *testing.T) {
	db, store := setupStore(t)
	defer db.Close()
	if err := store.updateSystemPrompt("owned", "stable-prefix"); err != nil {
		t.Fatal(err)
	}
	if err := store.queueTokenCounts("owned", map[string]any{"input_tokens": int64(11), "output_tokens": int64(7), "model": "first",
		"billing_provider": "provider-one", "billing_base_url": "https://first.invalid", "api_call_count": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := store.updateSessionBillingRoute("owned", "provider-two", "https://second.invalid", "api"); err != nil {
		t.Fatal(err)
	}
	if err := store.queueTokenCounts("owned", map[string]any{"input_tokens": int64(13), "output_tokens": int64(3), "model": "second",
		"billing_provider": "provider-two", "billing_base_url": "https://second.invalid", "api_call_count": int64(1)}); err != nil {
		t.Fatal(err)
	}
	row := mustSession(t, db, "owned")
	if !pyjson.Equal(rowValue(row, "input_tokens"), 24) || !pyjson.Equal(rowValue(row, "output_tokens"), 10) || !pyjson.Equal(rowValue(row, "billing_provider"), "provider-two") {
		t.Fatalf("usage row %v", row)
	}
	if !pyjson.Equal(rowValue(row, "system_prompt"), "stable-prefix") {
		t.Fatal("route write touched the prompt")
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM system_prompts WHERE prompt=?", "stable-prefix"), 1) {
		t.Fatal("prompt table rows")
	}
	visible := []any{map[string]any{"type": "text", "text": "visible"}}
	if _, err := store.appendMessagesBatch("owned", []map[string]any{{"role": "user", "content": visible}}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.setLatestUserAPIContent("owned", "wrong", "must-not-land"); err != nil || !pyjson.Equal(n, 0) {
		t.Fatalf("mismatched content landed %v %v", n, err)
	}
	if n, err := store.setLatestUserAPIContent("owned", visible, "wire-only"); err != nil || !pyjson.Equal(n, 1) {
		t.Fatalf("api content %v %v", n, err)
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT api_content FROM messages WHERE session_id=?", "owned"), "wire-only") {
		t.Fatal("api_content not stored")
	}
	usage := rowsOf(t, db, "SELECT model,billing_provider,input_tokens,output_tokens FROM session_model_usage WHERE session_id=? ORDER BY model", "owned")
	want := []any{pyjson.ObjectOf("model", "first", "billing_provider", "provider-one", "input_tokens", 11, "output_tokens", 7),
		pyjson.ObjectOf("model", "second", "billing_provider", "provider-two", "input_tokens", 13, "output_tokens", 3)}
	if !pyjson.Equal(anyObjects(usage), want) {
		t.Fatalf("usage rows %v", usage)
	}
	original := store.rpc
	store.rpc = lostAckRPC(original, "lost-ack")
	if err := store.updateSessionBillingRoute("owned", "third", "https://third.invalid", nil); err == nil || err.Error() != "lost-ack" {
		t.Fatalf("lost ack %v", err)
	}
	store.rpc = original
	if _, err := store.retryPending(); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "owned"), "billing_provider"), "third") {
		t.Fatal("retried route missing")
	}
	for _, c := range []struct {
		operation string
		payload   map[string]any
	}{
		{"session.billing_route", map[string]any{"provider": "evil", "base_url": "evil", "billing_mode": nil}},
		{"session.api_content", map[string]any{"content": "private", "api_content": "evil"}},
		{"session.title", map[string]any{"title": "evil", "source": "user"}},
		{"session.activity_clear", map[string]any{}},
	} {
		in := store.rawReceipt(c.operation, c.payload)
		in.SessionID = "foreign"
		if r := receiptRefusal(t, db, in); r != "permission_denied" {
			t.Fatalf("%s -> %s", c.operation, r)
		}
	}
	if !pyjson.Equal(rowValue(mustSession(t, db, "foreign"), "system_prompt"), "private") {
		t.Fatal("foreign prompt changed")
	}
}
