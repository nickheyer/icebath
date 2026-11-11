package agent

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestManagedWorkerTurnSurvivesLeaseRefreshTick(t *testing.T) {
	type sent struct {
		method, operation string
		payload           any
	}
	var calls []sent
	rpc := cannedCall(func(method string, p *pyjson.Object) (any, error) {
		calls = append(calls, sent{method, p.GetOr("operation", "").(string), p.GetOr("payload", nil)})
		return pyjson.ObjectOf("value", true), nil
	})
	store := mustStore(t, rpc, pyjson.ObjectOf("session_id", "s"), filepath.Join(t.TempDir(), "private"), 0)
	defer store.Close()
	a := &AIAgent{sessionID: "s"}
	expires := float64(time.Now().UnixNano())/1e9 + leaseTTLSeconds
	lease := newDurableTurnLease(a, store, "s", "holder", &expires)
	lease.turnActive = true
	before := lease.authorityDeadline
	if !lease.RefreshTick() {
		t.Fatal("refresh tick stopped the timer")
	}
	if lease.interruptMessage != "" || a.interruptMessage != "" {
		t.Fatalf("interrupted %q %q", lease.interruptMessage, a.interruptMessage)
	}
	if len(calls) != 1 || calls[0].method != "worker.persist" || calls[0].operation != "turn.renew" ||
		!pyjson.Equal(calls[0].payload, pyjson.ObjectOf("holder", "holder", "ttl_seconds", leaseTTLSeconds)) {
		t.Fatalf("sent %v", calls)
	}
	if lease.authorityDeadline < before {
		t.Fatalf("deadline moved back %v < %v", lease.authorityDeadline, before)
	}
}

func TestReopenReconcilesPendingReceiptBeforeNewWrite(t *testing.T) {
	dir := t.TempDir()
	scope := pyjson.ObjectOf("session_id", "s", "execution_id", "w", "epoch", int64(1))
	var calls []*pyjson.Object
	lost := cannedCall(func(_ string, p *pyjson.Object) (any, error) {
		calls = append(calls, p)
		return nil, &workerStoreError{"TimeoutError", "lost reply"}
	})
	store := mustStore(t, lost, scope, dir, 0)
	if err := store.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 10}); err == nil {
		t.Fatal("lost reply accepted")
	}
	store.closeOwner()
	var restored []*pyjson.Object
	owner := cannedCall(func(_ string, p *pyjson.Object) (any, error) {
		restored = append(restored, p)
		return pyjson.ObjectOf("ok", true), nil
	})
	reopened := mustStore(t, owner, scope, dir, 0)
	defer reopened.Close()
	if err := reopened.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(restored[0], calls[0]) {
		t.Fatalf("replay %v != %v", restored[0], calls[0])
	}
	if len(restored) != 2 || !pyjson.Equal(restored[0].GetOr("sequence", nil), int64(1)) || !pyjson.Equal(restored[1].GetOr("sequence", nil), int64(2)) {
		t.Fatalf("sequences %v", restored)
	}
	for i, want := range []int64{10, 20} {
		if got := restored[i].GetOr("payload", nil).(*pyjson.Object).GetOr("input_tokens", nil); !pyjson.Equal(got, want) {
			t.Fatalf("input_tokens %d = %v", i, got)
		}
	}
}

// Owner call that commits for real and then loses its reply
func losingAck(db *state.SessionDB) WorkerCall {
	commit := dbWorkerCall(db)
	return func(ctx context.Context, method string, p *pyjson.Object) (any, error) {
		if _, err := commit(ctx, method, p); err != nil {
			return nil, err
		}
		return nil, &workerStoreError{"TimeoutError", "lost_ack"}
	}
}

func receiptCount(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int64
	if err := raw.QueryRow("SELECT COUNT(*) FROM worker_receipts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func openOwnerDB(t *testing.T, path string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLostAckReceiptReconcilesUnderTheAdoptedEpochOnce(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	path, outbox := filepath.Join(tmp, "state.db"), filepath.Join(tmp, "outbox")
	db := openOwnerDB(t, path)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	first, _ := db.BeginRuntimeEpoch(ctx, "first")
	scope := pyjson.ObjectOf("epoch", first, "execution_id", "worker", "session_id", "s", "generation", int64(0))
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: first, ExecutionID: "worker", SessionID: "s", Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	store := mustStore(t, dbWorkerCall(db), scope, outbox, 0)
	store.rpc = losingAck(db)
	if err := store.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 10}); err == nil {
		t.Fatal("lost ack accepted")
	}
	store.closeOwner()
	db.Close()
	db = openOwnerDB(t, path)
	defer db.Close()
	second, _ := db.BeginRuntimeEpoch(ctx, "second")
	successor := scope.Copy()
	successor.Set("epoch", second)
	adopted, err := db.AdoptWorkerExecution(ctx, second, "worker", "s", 0, "private")
	if err != nil {
		t.Fatal(err)
	}
	staleEpoch, otherExecution := adopted.Copy(), adopted.Copy()
	staleEpoch.Set("owner_epoch", first)
	otherExecution.Set("execution_id", "other")
	for _, proof := range []any{nil, staleEpoch, otherExecution} {
		if _, err := NewRuntimeSessionStore(ctx, dbWorkerCall(db), successor, outbox, proof, 0); persistenceReason(err) != "outbox_scope_mismatch" {
			t.Fatalf("proof %v accepted: %v", proof, err)
		}
	}
	regenerated := successor.Copy()
	regenerated.Set("generation", int64(1))
	if _, err := NewRuntimeSessionStore(ctx, dbWorkerCall(db), regenerated, outbox, adopted, 0); persistenceReason(err) != "outbox_scope_mismatch" {
		t.Fatalf("generation drift accepted: %v", err)
	}
	if n := receiptCount(t, path); n != 1 {
		t.Fatalf("receipts %d", n)
	}
	reopened, err := NewRuntimeSessionStore(ctx, dbWorkerCall(db), successor, outbox, adopted, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if pending, _ := journalPending(reopened.journal); len(pending) != 0 || !pyjson.Equal(reopened.Scope(), successor) {
		t.Fatalf("journal %v scope %v", pending, reopened.Scope())
	}
	if err := reopened.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if n := receiptCount(t, path); n != 2 {
		t.Fatalf("receipts %d", n)
	}
	row, _ := db.GetSession(ctx, "s")
	if !pyjson.Equal(row.GetOr("input_tokens", nil), int64(15)) {
		t.Fatalf("input_tokens %v", row.GetOr("input_tokens", nil))
	}
}

func TestPendingPublicationFollowsAdoptionOntoTheRotatedTarget(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	path, outbox := filepath.Join(tmp, "state.db"), filepath.Join(tmp, "outbox")
	db := openOwnerDB(t, path)
	if err := db.CreateSession(state.NewSession{ID: "root", Source: "cli", SystemPrompt: "PREFIX"}); err != nil {
		t.Fatal(err)
	}
	first, _ := db.BeginRuntimeEpoch(ctx, "first")
	scope := pyjson.ObjectOf("epoch", first, "execution_id", "worker", "session_id", "root", "generation", int64(0))
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: first, ExecutionID: "worker", SessionID: "root", Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	store := mustStore(t, dbWorkerCall(db), scope, outbox, 0)
	if ok, err := store.TryAcquireCompressionLockChecked("root", "holder", 300); !ok || err != nil {
		t.Fatalf("lock %v %v", ok, err)
	}
	store.rpc = losingAck(db)
	prompt, holder := "PREFIX", "holder"
	err := store.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: "root", ChildSessionID: "child", Source: "cli",
		Messages: []map[string]any{{"role": "assistant", "content": "SUMMARY"}}, SystemPrompt: &prompt, CompressionLockHolder: &holder,
		RequireCompressionLease: true, LeaseTTLSeconds: 300})
	if err == nil {
		t.Fatal("lost publication ack accepted")
	}
	store.closeOwner()
	db.Close()
	db = openOwnerDB(t, path)
	defer db.Close()
	second, _ := db.BeginRuntimeEpoch(ctx, "second")
	adopted, err := db.AdoptWorkerExecution(ctx, second, "worker", "child", 0, "private")
	if err != nil {
		t.Fatal(err)
	}
	receipts := receiptCount(t, path)
	rotated := scope.Copy()
	rotated.Set("epoch", second)
	rotated.Set("session_id", "child")
	reopened, err := NewRuntimeSessionStore(ctx, dbWorkerCall(db), rotated, outbox, adopted, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if pending, _ := journalPending(reopened.journal); len(pending) != 0 || !pyjson.Equal(reopened.Scope(), rotated) {
		t.Fatalf("journal %v scope %v", pending, reopened.Scope())
	}
	if n := receiptCount(t, path); n != receipts {
		t.Fatalf("receipts %d != %d", n, receipts)
	}
	if child, _ := db.GetSession(ctx, "child"); child == nil {
		t.Fatal("child row missing")
	}
	if _, err := reopened.AppendMessagesBatch(ctx, "child", []map[string]any{{"role": "user", "content": "next"}}, state.AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStoreOffersEveryCoreSessionDbCallWithItsKeywords(t *testing.T) {
	core := reflect.TypeOf((*SessionStore)(nil)).Elem()
	owner, worker := reflect.TypeOf((*state.SessionDB)(nil)), reflect.TypeOf((*RuntimeSessionStore)(nil))
	for i := range core.NumMethod() {
		name := core.Method(i).Name
		w, ok := worker.MethodByName(name)
		if !ok {
			t.Fatalf("RuntimeSessionStore lacks %s", name)
		}
		o, ok := owner.MethodByName(name)
		if !ok {
			t.Fatalf("SessionDB lacks %s", name)
		}
		if w.Type.NumIn() != o.Type.NumIn() || w.Type.NumOut() != o.Type.NumOut() {
			t.Fatalf("RuntimeSessionStore.%s %v rejects the SessionDB arguments %v", name, w.Type, o.Type)
		}
		for j := 1; j < o.Type.NumIn(); j++ {
			if w.Type.In(j) != o.Type.In(j) {
				t.Fatalf("RuntimeSessionStore.%s argument %d is %v, SessionDB takes %v", name, j, w.Type.In(j), o.Type.In(j))
			}
		}
	}
}

func TestTailReadsAnswerLikeTheOwner(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "state.db")
	db := openOwnerDB(t, path)
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(ctx, "s", []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"},
		{"role": "user", "content": "failed turn"}}, state.AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "other", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(ctx, "other", []map[string]any{{"role": "assistant", "content": "x"}}, state.AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []int64
	q, err := raw.Query("SELECT id FROM messages ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for q.Next() {
		var id int64
		q.Scan(&id)
		rows = append(rows, id)
	}
	q.Close()
	raw.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "fixture")
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: epoch, ExecutionID: "w", SessionID: "s", Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	scope := pyjson.ObjectOf("epoch", epoch, "execution_id", "w", "session_id", "s", "generation", int64(0))
	store := mustStore(t, dbWorkerCall(db), scope, filepath.Join(tmp, "outbox"), 0)
	defer store.Close()
	viaStore, err := store.LatestConversationRole(ctx, "s")
	direct, err2 := db.LatestConversationRole(ctx, "s")
	if err != nil || err2 != nil || viaStore != direct || direct != "user" {
		t.Fatalf("tail role %q %q %v %v", viaStore, direct, err, err2)
	}
	for _, rowID := range append(rows, 1_000_000) {
		sr, sok, err := store.GetMessageRole(ctx, "s", rowID)
		dr, dok, err2 := db.GetMessageRole(ctx, "s", rowID)
		if err != nil || err2 != nil || sr != dr || sok != dok {
			t.Fatalf("row %d store %q %v direct %q %v (%v %v)", rowID, sr, sok, dr, dok, err, err2)
		}
	}
}
