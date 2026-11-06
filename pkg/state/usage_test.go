package state

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func usageDB(t *testing.T) *SessionDB {
	t.Helper()
	return usageOpen(t, filepath.Join(t.TempDir(), "test_state.db"))
}

func usageOpen(t *testing.T, path string) *SessionDB {
	t.Helper()
	d := mustSessionDB(t, path, false)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type usageTotalsRow struct {
	input, output, apiCalls int64
	estimated               float64
}

func usageTotals(t *testing.T, d *SessionDB, id string) usageTotalsRow {
	t.Helper()
	row, err := d.readOne(context.Background(), "SELECT input_tokens, output_tokens, api_call_count, estimated_cost_usd FROM sessions WHERE id = ?", id)
	if err != nil || row == nil {
		t.Fatalf("totals of %s: %v %v", id, row, err)
	}
	est, _ := pyFloatOf(orZero(rowValue(row, "estimated_cost_usd")))
	n := func(k string) int64 { v, _ := pyIntOf(orZero(rowValue(row, k))); return v.Int64() }
	return usageTotalsRow{n("input_tokens"), n("output_tokens"), n("api_call_count"), est}
}

func strPtr(s string) *string     { return &s }
func floatPtr(f float64) *float64 { return &f }

func setUpdateHook(d *SessionDB, hook func(string, TokenCountDelta) error) {
	d.tokens.tokenMu.Lock()
	d.tokens.updateHook = hook
	d.tokens.tokenMu.Unlock()
}

func TestTokenDeltasApplyInEnqueueOrder(t *testing.T) {
	d := usageDB(t)
	for _, id := range []string{"s-a", "s-b"} {
		if err := d.CreateSession(NewSession{ID: id, Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var applied, expected []string
	setUpdateHook(d, func(id string, delta TokenCountDelta) error {
		mu.Lock()
		applied = append(applied, id+":"+pyStr(delta.InputTokens))
		mu.Unlock()
		return d.UpdateTokenCounts(id, delta)
	})
	for i := int64(1); i < 7; i++ {
		id := "s-b"
		if i%2 == 1 {
			id = "s-a"
		}
		if err := d.QueueTokenCounts(id, TokenCountDelta{InputTokens: i, APICallCount: 1}); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, id+":"+pyStr(i))
	}
	if !d.FlushTokenCounts(5.0) {
		t.Fatal("flush timed out")
	}
	setUpdateHook(d, nil)
	if pyStr(applied) != pyStr(expected) {
		t.Fatalf("applied %v want %v", applied, expected)
	}
	if usageTotals(t, d, "s-a").input != 9 || usageTotals(t, d, "s-b").input != 12 {
		t.Fatal("per session totals wrong")
	}
}

func TestAbsoluteDeltaIsAnOrderingBarrier(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-abs", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []TokenCountDelta{{InputTokens: 100, APICallCount: 1},
		{InputTokens: 500, OutputTokens: 50, APICallCount: 3, Absolute: true}, {InputTokens: 7, APICallCount: 1}} {
		if err := d.QueueTokenCounts("s-abs", delta); err != nil {
			t.Fatal(err)
		}
	}
	if !d.FlushTokenCounts(5.0) {
		t.Fatal("flush timed out")
	}
	if got := usageTotals(t, d, "s-abs"); got.input != 507 || got.output != 50 || got.apiCalls != 4 {
		t.Fatalf("totals %+v", got)
	}
}

func TestTokenBacklogCoalescesAndSumsMatch(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-c", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	setUpdateHook(d, func(id string, delta TokenCountDelta) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
		return d.UpdateTokenCounts(id, delta)
	})
	delta := TokenCountDelta{InputTokens: 1, OutputTokens: 1, EstimatedCostUSD: floatPtr(0.001), Model: strPtr("m1"),
		BillingProvider: strPtr("p1"), APICallCount: 1}
	const n = 20
	_ = d.QueueTokenCounts("s-c", delta)
	<-started
	for range n - 1 {
		_ = d.QueueTokenCounts("s-c", delta)
	}
	close(release)
	if !d.FlushTokenCounts(5.0) {
		t.Fatal("flush timed out")
	}
	setUpdateHook(d, nil)
	if calls >= n {
		t.Fatalf("backlog never coalesced, %d applies", calls)
	}
	got := usageTotals(t, d, "s-c")
	if got.input != n || got.output != n || got.apiCalls != n || math.Abs(got.estimated-0.001*n) > 1e-9 {
		t.Fatalf("totals %+v", got)
	}
	rows, err := d.readAll(context.Background(), "SELECT input_tokens, api_call_count FROM session_model_usage WHERE session_id = ?", "s-c")
	if err != nil || len(rows) != 1 || pyStr(rowValue(rows[0], "input_tokens")) != "20" || pyStr(rowValue(rows[0], "api_call_count")) != "20" {
		t.Fatalf("model usage rows %v %v", rows, err)
	}
}

func TestCoalescedApplyEqualsSequentialApply(t *testing.T) {
	d := usageDB(t)
	seq := usageOpen(t, filepath.Join(t.TempDir(), "sequential.db"))
	batch := []queuedTokenDelta{
		{"s-eq", TokenCountDelta{InputTokens: 10, OutputTokens: 2, Model: strPtr("m1"), BillingProvider: strPtr("p1"), EstimatedCostUSD: floatPtr(0.01), CostStatus: strPtr("estimated"), APICallCount: 1}},
		{"s-eq", TokenCountDelta{InputTokens: 20, OutputTokens: 4, Model: strPtr("m1"), BillingProvider: strPtr("p1"), EstimatedCostUSD: floatPtr(0.02), CostStatus: strPtr("estimated"), APICallCount: 1}},
		{"s-eq", TokenCountDelta{InputTokens: 5, OutputTokens: 1, Model: strPtr("m2"), BillingProvider: strPtr("p1"), EstimatedCostUSD: floatPtr(0.005), CostStatus: strPtr("estimated"), APICallCount: 1}},
	}
	for _, db := range []*SessionDB{d, seq} {
		if err := db.CreateSession(NewSession{ID: "s-eq", Source: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	d.applyTokenBatch(append([]queuedTokenDelta(nil), batch...))
	for _, item := range batch {
		if err := seq.UpdateTokenCounts(item.sessionID, item.delta); err != nil {
			t.Fatal(err)
		}
	}
	q := "SELECT model, input_tokens, output_tokens, api_call_count, ROUND(estimated_cost_usd, 9) AS est FROM session_model_usage WHERE session_id = 's-eq' ORDER BY model"
	a, _ := d.readAll(context.Background(), q)
	b, _ := seq.readAll(context.Background(), q)
	if pyStr(usageTotals(t, d, "s-eq")) != pyStr(usageTotals(t, seq, "s-eq")) || len(a) != 2 || dumpRows(a) != dumpRows(b) {
		t.Fatalf("coalesced %v sequential %v", a, b)
	}
}

func TestGetSessionSeesQueuedDeltas(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-r", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	setUpdateHook(d, func(id string, delta TokenCountDelta) error {
		time.Sleep(50 * time.Millisecond)
		return d.UpdateTokenCounts(id, delta)
	})
	for i := int64(1); i < 5; i++ {
		_ = d.QueueTokenCounts("s-r", TokenCountDelta{InputTokens: i, Model: strPtr("m" + pyStr(i%2)), APICallCount: 1})
	}
	row, err := d.GetSession(context.Background(), "s-r")
	setUpdateHook(d, nil)
	if err != nil || pyStr(rowValue(row, "input_tokens")) != "10" || pyStr(rowValue(row, "api_call_count")) != "4" {
		t.Fatalf("row %v %v", row, err)
	}
}

func TestReadBarrierOnRunningLoopNeverWaits(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-loop", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	setUpdateHook(d, func(id string, delta TokenCountDelta) error {
		<-release
		return d.UpdateTokenCounts(id, delta)
	})
	_ = d.QueueTokenCounts("s-loop", TokenCountDelta{InputTokens: 3, APICallCount: 1})
	begin := time.Now()
	if d.FlushForRead(pyctx.WithRunningLoop(context.Background())) {
		t.Fatal("a loop read reported drained while the writer was blocked")
	}
	if time.Since(begin) > time.Second {
		t.Fatal("the loop read waited on the writer")
	}
	close(release)
	if !d.FlushForRead(context.Background()) {
		t.Fatal("the off loop read did not drain")
	}
	setUpdateHook(d, nil)
}

func TestConcurrentFlushWaitsForCallerDrain(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-cc", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	d.FlushTokenCounts(5.0)
	d.stopTokenWriter(10.0)
	applied, gate := make(chan struct{}), make(chan struct{})
	setUpdateHook(d, func(id string, delta TokenCountDelta) error {
		close(applied)
		<-gate
		return d.UpdateTokenCounts(id, delta)
	})
	d.tokens.tokenMu.Lock()
	d.tokens.tokenQueue = append(d.tokens.tokenQueue, queuedTokenDelta{"s-cc", TokenCountDelta{InputTokens: 4, APICallCount: 1}})
	d.tokens.tokenMu.Unlock()
	result := make(chan bool)
	go func() { result <- d.FlushTokenCounts(5.0) }()
	<-applied
	if d.FlushTokenCounts(0.3) {
		t.Fatal("a second flush reported drained while the first caller's batch was unapplied")
	}
	close(gate)
	if !<-result || !d.FlushTokenCounts(5.0) {
		t.Fatal("caller drain did not complete")
	}
	setUpdateHook(d, nil)
	if usageTotals(t, d, "s-cc").input != 4 {
		t.Fatal("drained delta missing")
	}
}

func TestEnqueueAfterCloseAppliesInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.db")
	d := usageOpen(t, path)
	if err := d.CreateSession(NewSession{ID: "s-closed", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	_ = d.QueueTokenCounts("s-closed", TokenCountDelta{InputTokens: 1, APICallCount: 1})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.QueueTokenCounts("s-closed", TokenCountDelta{InputTokens: 2, APICallCount: 1}); err != nil {
		t.Fatal(err)
	}
	d.tokens.tokenMu.Lock()
	queued := len(d.tokens.tokenQueue)
	d.tokens.tokenMu.Unlock()
	if queued != 0 {
		t.Fatal("a delta queued behind a stopped writer")
	}
	check := usageOpen(t, path)
	if got := usageTotals(t, check, "s-closed"); got.input != 3 || got.apiCalls != 2 {
		t.Fatalf("totals %+v", got)
	}
}

func TestIdleWriterRestartsForLaterDelta(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-idle", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	_ = d.QueueTokenCounts("s-idle", TokenCountDelta{InputTokens: 1, APICallCount: 1})
	d.FlushTokenCounts(5.0)
	saved := tokenWriterIdleSeconds
	t.Cleanup(func() { tokenWriterIdleSeconds = saved })
	tokenWriterIdleSeconds = 0.05
	_ = d.QueueTokenCounts("s-idle", TokenCountDelta{InputTokens: 1, APICallCount: 1})
	d.FlushTokenCounts(5.0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.tokens.tokenMu.Lock()
		gone := !d.tokens.tokenWriter.alive()
		d.tokens.tokenMu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle writer never retired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = d.QueueTokenCounts("s-idle", TokenCountDelta{InputTokens: 5, APICallCount: 1})
	if !d.FlushTokenCounts(5.0) || usageTotals(t, d, "s-idle").input != 7 {
		t.Fatal("a later delta after the writer retired did not land")
	}
}

func TestCloseUnregistersExitHook(t *testing.T) {
	d := usageOpen(t, filepath.Join(t.TempDir(), "hook.db"))
	if err := d.CreateSession(NewSession{ID: "s-hook", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	_ = d.QueueTokenCounts("s-hook", TokenCountDelta{InputTokens: 1, APICallCount: 1})
	d.tokens.tokenMu.Lock()
	hook := d.tokens.tokenAtexitHook
	d.tokens.tokenMu.Unlock()
	registered := func() bool {
		tokenExitHooks.mu.Lock()
		defer tokenExitHooks.mu.Unlock()
		for _, h := range tokenExitHooks.hooks {
			if h == hook {
				return true
			}
		}
		return false
	}
	if hook == nil || !registered() {
		t.Fatal("the first queued delta did not register an exit hook")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if registered() || d.tokens.tokenAtexitHook != nil {
		t.Fatal("close left the exit hook registered")
	}
}

func TestCoalesceFailureFallsBackToRawBatch(t *testing.T) {
	d := usageDB(t)
	if err := d.CreateSession(NewSession{ID: "s-f", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	saved := tokenCoalescer
	t.Cleanup(func() { tokenCoalescer = saved })
	tokenCoalescer = func([]queuedTokenDelta) []queuedTokenDelta { panic("boom") }
	d.applyTokenBatch([]queuedTokenDelta{{"s-f", TokenCountDelta{InputTokens: 2, APICallCount: 1}}, {"s-f", TokenCountDelta{InputTokens: 3, APICallCount: 1}}})
	if got := usageTotals(t, d, "s-f"); got.input != 5 || got.apiCalls != 2 {
		t.Fatalf("raw batch not applied %+v", got)
	}
}

func TestEveryUpdateKeywordIsClassifiedForCoalescing(t *testing.T) {
	classified := map[string]bool{"absolute": true}
	for _, group := range [][]string{tokenDeltaSumFields, tokenDeltaCostFields, tokenDeltaRouteFields} {
		for _, f := range group {
			if classified[f] {
				t.Fatalf("%s classified twice", f)
			}
			classified[f] = true
		}
	}
	for _, kw := range []string{"input_tokens", "output_tokens", "model", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
		"estimated_cost_usd", "actual_cost_usd", "cost_status", "cost_source", "pricing_version", "billing_provider",
		"billing_base_url", "billing_mode", "api_call_count", "absolute", "source", "task"} {
		if !classified[kw] {
			t.Fatalf("%s unclassified", kw)
		}
	}
	var delta TokenCountDelta
	for _, f := range tokenDeltaRouteFields {
		_ = delta.field(f)
	}
	for _, f := range tokenDeltaSumFields {
		delta.addSum(f, &delta)
	}
	for _, f := range tokenDeltaCostFields {
		_ = delta.costSlot(f)
	}
}

func TestTokenGuardInsertCarriesRealSource(t *testing.T) {
	d := usageDB(t)
	if err := d.UpdateTokenCounts("s-src", TokenCountDelta{InputTokens: 1, APICallCount: 1, Source: strPtr("telegram")}); err != nil {
		t.Fatal(err)
	}
	row, err := d.GetSession(context.Background(), "s-src")
	if err != nil || !pyEquals(rowValue(row, "source"), "telegram") || !pyEquals(rowValue(row, "created_source"), "telegram") {
		t.Fatalf("row %v %v", row, err)
	}
	if err := d.UpdateTokenCounts("s-unk", TokenCountDelta{InputTokens: 1, APICallCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateSession(NewSession{ID: "s-unk", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if row, _ := d.GetSession(context.Background(), "s-unk"); !pyEquals(rowValue(row, "source"), "cli") {
		t.Fatalf("placeholder source not replaced %v", row)
	}
}

func TestModelUsageFallsBackToSessionRouteExceptForAux(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	if err := d.CreateSession(NewSession{ID: "s-m", Source: "cli", Model: "main-model"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateTokenCounts("s-m", TokenCountDelta{InputTokens: 10, APICallCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordAuxiliaryUsage(ctx, "s-m", "vision", AuxiliaryUsage{InputTokens: 4}); err != nil {
		t.Fatal(err)
	}
	rows, _ := d.readAll(ctx, "SELECT model, task, api_call_count FROM session_model_usage WHERE session_id = 's-m' ORDER BY task")
	if len(rows) != 2 || !pyEquals(rowValue(rows[0], "model"), "main-model") || !pyEquals(rowValue(rows[1], "model"), "unknown") {
		t.Fatalf("rows %v", rows)
	}
	byTask, err := d.AuxiliaryUsageByTask(ctx, "s-m")
	if err != nil || byTask.Len() != 1 {
		t.Fatalf("aux by task %v %v", byTask, err)
	}
	tokens, _, err := d.UsageTotals(ctx, 0, false)
	if err != nil || tokens != 10 {
		t.Fatalf("usage totals %d %v", tokens, err)
	}
}

func TestIsKanbanOwnedSessionFollowsVerifiedLineage(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	if err := d.CreateSession(NewSession{ID: "root", Source: "kanban"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.writeRowcount(ctx, "UPDATE sessions SET end_reason = 'compression', ended_at = 1 WHERE id = 'root'", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateSession(NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateSession(NewSession{ID: "fork", Source: "cli", ParentSessionID: "root", ModelConfig: map[string]any{"_branched_from": "root"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateSession(NewSession{ID: "plain", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"root": true, "tip": true, "fork": false, "plain": false, "missing": false} {
		got, err := d.IsKanbanOwnedSession(ctx, id)
		if err != nil || got != want {
			t.Fatalf("%s kanban owned %v want %v (%v)", id, got, want, err)
		}
	}
	lineage, _ := d.resumeLineageIDs(ctx, "tip")
	if pyStr(lineage) != pyStr([]string{"root", "tip"}) {
		t.Fatalf("lineage %v", lineage)
	}
	if fork, _ := d.IsExplicitForkChild(ctx, "fork"); !fork {
		t.Fatal("branch child not classified as a fork")
	}
}

func TestFindSessionByOriginPrefersExactSender(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	for _, row := range []struct{ id, user string }{{"a", "u1"}, {"b", "u2"}} {
		if err := d.CreateSession(NewSession{ID: row.id, Source: "telegram", SessionKey: "k-" + row.id, ChatID: "c1", UserID: row.user}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := FindSessionByOrigin(ctx, d, "Telegram", "c1", nil, strPtr("u1")); got != "a" {
		t.Fatalf("exact sender %q", got)
	}
	if got, _ := FindSessionByOrigin(ctx, d, "telegram", "c1", nil, nil); got != "" {
		t.Fatalf("two participants without a user must be ambiguous, got %q", got)
	}
	if got, _ := FindSessionByOrigin(ctx, d, "telegram", "c1", nil, strPtr("u3")); got != "" {
		t.Fatalf("unknown sender among several %q", got)
	}
	if got, _ := FindSessionByOrigin(ctx, d, "", "c1", nil, nil); got != "" {
		t.Fatal("empty platform matched")
	}
}

func dumpRows(rows []*pyjson.Object) string {
	text, err := pyjson.Dumps(rows)
	if err != nil {
		panic(err)
	}
	return text
}
