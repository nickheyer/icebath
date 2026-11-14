package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Launch store of the case home holding a tui row for each key
func reapLedger(t *testing.T, keys ...string) *state.DB {
	t.Helper()
	db := reaperTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	useSharedDB(t, db)
	for _, key := range keys {
		if err := db.CreateSession(state.NewSession{ID: key, Source: "tui"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// End reason the store recorded for a session, empty while it is open
func storedEndReason(t *testing.T, db *state.DB, sid string) string {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	return strOr(row.GetOr("end_reason", nil))
}

// Whether a reap timer still counts down, false once it was cancelled
func reapArmed(timer *wsReapTimer) bool { return timer.timer.Stop() }

// Plain write peer of a resumed client, the id keeps two peers distinct
type liveWritePeer struct{ id int }

func (*liveWritePeer) Write(*pyjson.Object) bool { return true }
func (*liveWritePeer) Close()                    {}

// Agent double whose close holds the teardown until released, the slow teardown of the reap
type slowClosingAgent struct{ started, release chan struct{} }

func (a *slowClosingAgent) CloseContext(context.Context) error {
	close(a.started)
	<-a.release
	return nil
}

// Delegation field whose truth test raises, a registry lookup that fails
type raisingTruth struct{}

func (raisingTruth) PyBool() bool { panic("delegation registry unavailable") }

func TestWsOrphanReapInterruptsIsolatedTurnThenReaps(t *testing.T) {
	session := serverCaseSession(t, "isolated-sid", map[string]any{"agent": &struct{}{}, "agent_ready": &pythread.Event{}, "running": true,
		"_compute_host_active": true, "history": []any{map[string]any{"role": "assistant", "content": "partial"}},
		"queued_prompt": map[string]any{"text": "must not run"}, "session_key": "isolated-key"})
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("dashboard:\n  turn_isolation: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := reapLedger(t, "isolated-key")
	sup, sent := testSupervisor(t)
	swapVar(t, &computeHostSupervisor, sup)
	interrupts := func() []string {
		var out []string
		for _, frame := range *sent {
			if frame.GetOr("type", nil) == "interrupt" {
				out = append(out, strOr(frame.GetOr("sid", nil))+"/"+strOr(frame.GetOr("request_id", nil)))
			}
		}
		return out
	}
	scheduleWSOrphanReap(context.Background(), "isolated-sid", nil, nil)
	first := pendingReap("isolated-sid")
	fireReap(first, 20, 20)
	if got := interrupts(); len(got) != 1 || got[0] != "isolated-sid/client-gone-isolated-sid" {
		t.Fatalf("interrupts %v", got)
	}
	history, _ := session.Get("history").([]any)
	if session.Get("_turn_cancel_requested") != true || session.Get("queued_prompt") != nil || len(history) != 1 {
		t.Fatalf("cancel %v queued %v history %v", session.Get("_turn_cancel_requested"), session.Get("queued_prompt"), history)
	}
	second := pendingReap("isolated-sid")
	if second == nil || second == first {
		t.Fatal("isolated turn reap was not rescheduled")
	}
	fireReap(second, 20, 20)
	third := pendingReap("isolated-sid")
	if got := interrupts(); len(got) != 1 || third == nil || third == second {
		t.Fatalf("interrupts %v rescheduled %v", got, third != nil)
	}
	session.Set("running", false)
	fireReap(third, 20, 20)
	if tuigateway.SessionRecord("isolated-sid") != nil || session.Get("_finalized") != true {
		t.Fatal("settled isolated session was not reaped")
	}
	if reason := storedEndReason(t, db, "isolated-key"); reason != "ws_orphan_reap" {
		t.Fatalf("end reason %q", reason)
	}
}

func TestWsOrphanReapReleasesResumeLockBeforeSlowTeardown(t *testing.T) {
	closer := &slowClosingAgent{started: make(chan struct{}), release: make(chan struct{})}
	serverCaseSession(t, "slow-orphan", map[string]any{"agent": closer, "session_key": "slow-key"})
	db := reapLedger(t, "slow-key")
	scheduleWSOrphanReap(context.Background(), "slow-orphan", nil, nil)
	timer := pendingReap("slow-orphan")
	done := make(chan struct{})
	go func() {
		defer close(done)
		fireReap(timer, 20, 20)
	}()
	released := false
	defer func() {
		if !released {
			close(closer.release)
		}
		<-done
	}()
	select {
	case <-closer.started:
	case <-time.After(10 * time.Second):
		t.Fatal("teardown never started")
	}
	if tuigateway.SessionRecord("slow-orphan") != nil {
		t.Fatal("reaped session still registered")
	}
	acquired := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if acquired = sessionResumeLock.TryLock(); acquired {
			sessionResumeLock.Unlock()
			break
		}
	}
	if !acquired {
		t.Fatal("orphan teardown kept the global resume lock held")
	}
	close(closer.release)
	released = true
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reap thread never finished")
	}
	if reason := storedEndReason(t, db, "slow-key"); reason != "ws_orphan_reap" {
		t.Fatalf("end reason %q", reason)
	}
}

func TestWsOrphanReapRetriesWhenDelegationLookupFails(t *testing.T) {
	session := serverCaseSession(t, "lookup-error-sid", map[string]any{})
	t.Cleanup(tools.SwapAsyncRecords([]string{"deleg_broken"}, map[string]map[string]any{
		"deleg_broken": {"delegation_id": "deleg_broken", "status": "running", "origin_ui_session_id": raisingTruth{}, "session_key": raisingTruth{}}}))
	scheduleWSOrphanReap(context.Background(), "lookup-error-sid", nil, nil)
	first := pendingReap("lookup-error-sid")
	fireReap(first, 20, 20)
	if tuigateway.SessionRecord("lookup-error-sid") != session || truthy(session.Get("_finalized")) {
		t.Fatal("session reaped while the delegation lookup failed")
	}
	if next := pendingReap("lookup-error-sid"); next == nil || next == first {
		t.Fatal("reap was not retried")
	}
}

func TestResumeRebindCancelsPendingWsOrphanReap(t *testing.T) {
	session := serverCaseSession(t, "cancel-sid", map[string]any{})
	scheduleWSOrphanReap(context.Background(), "cancel-sid", nil, nil)
	timer := pendingReap("cancel-sid")
	if timer == nil {
		t.Fatal("no pending reap")
	}
	liveSessionPayload(context.Background(), "cancel-sid", session, livePayloadOptions{transport: &liveWritePeer{}, omitMessages: true})
	if pendingReap("cancel-sid") != nil || reapArmed(timer) {
		t.Fatal("rebind left the reap armed")
	}
}

func TestClaimOrReuseLiveWinnerCancelsPendingReap(t *testing.T) {
	winner := serverCaseSession(t, "winner-sid", map[string]any{"session_key": "stored-claim"})
	t.Cleanup(func() { tuigateway.SetSessionRecord("fresh-sid", nil) })
	scheduleWSOrphanReap(context.Background(), "winner-sid", nil, nil)
	timer := pendingReap("winner-sid")
	if timer == nil {
		t.Fatal("no pending reap")
	}
	fresh := tuigateway.NewSession(map[string]any{"agent": &struct{}{}, "session_key": "session-key", "history": []any{}})
	liveSID, live, reused := claimOrReuseLive(context.Background(), "fresh-sid", "stored-claim", fresh, nil)
	if !reused || liveSID != "winner-sid" || live != winner {
		t.Fatalf("claim %s %v", liveSID, reused)
	}
	if pendingReap("winner-sid") != timer || winner.Get("transport") != tuigateway.DetachedWSTransport() {
		t.Fatal("unguarded claim touched the winner")
	}
	transport := &liveWritePeer{}
	ctx := tuigateway.BindTransport(context.Background(), transport)
	response := resumeReuseLive(newResumeCtx(ctx, 1, map[string]any{"omit_messages": true}, "stored-claim"), liveSID, live)
	if resultOf(t, response)["session_id"] != "winner-sid" || winner.Get("transport") != transport {
		t.Fatalf("reuse %v", response)
	}
	if pendingReap("winner-sid") != nil || reapArmed(timer) {
		t.Fatal("reuse left the reap armed")
	}
}

func TestSupersededRuntimeFinalizedWithoutReclaimedBroadcast(t *testing.T) {
	old := serverCaseSession(t, "old-sid", map[string]any{"session_key": "stored-super"})
	db := reapLedger(t, "stored-super")
	t.Cleanup(func() { tuigateway.SetSessionRecord("new-sid", nil); cancelWSOrphanReap("new-sid") })
	var broadcastsMu sync.Mutex
	var broadcasts []string
	swapVar(t, &broadcastGlobalEvent, func(event string, _ map[string]any) {
		broadcastsMu.Lock()
		broadcasts = append(broadcasts, event)
		broadcastsMu.Unlock()
	})
	scheduleWSOrphanReap(context.Background(), "old-sid", nil, nil)
	timer := pendingReap("old-sid")
	if timer == nil {
		t.Fatal("no pending reap")
	}
	tuigateway.SetSessionRecord("old-sid", nil)
	fresh := tuigateway.NewSession(map[string]any{"agent": &struct{}{}, "session_key": "stored-super", "history": []any{}})
	sessionsLock.Lock()
	type claimed struct {
		sid    string
		reused bool
	}
	result := make(chan claimed, 1)
	go func() {
		sid, _, reused := claimOrReuseLive(context.Background(), "new-sid", "stored-super", fresh, nil)
		result <- claimed{sid, reused}
	}()
	waitBlockedIn(t, "claimOrReuseLive")
	tuigateway.SetSessionRecord("old-sid", old)
	sessionsLock.Unlock()
	got := <-result
	if got.reused || got.sid != "" {
		t.Fatalf("claim %v", got)
	}
	if tuigateway.SessionRecord("new-sid") != fresh || tuigateway.SessionRecord("old-sid") != nil {
		t.Fatal("fresh runtime not registered or the parked one kept")
	}
	if pendingReap("old-sid") != nil || reapArmed(timer) {
		t.Fatal("superseded runtime kept its reap armed")
	}
	if old.Get("_finalized") != true || storedEndReason(t, db, "stored-super") != "superseded_by_resume" {
		t.Fatalf("finalized %v reason %q", old.Get("_finalized"), storedEndReason(t, db, "stored-super"))
	}
	broadcastsMu.Lock()
	defer broadcastsMu.Unlock()
	if len(broadcasts) != 0 {
		t.Fatalf("broadcasts %v", broadcasts)
	}
}

func TestLazyUnpersistedResumeRebindsTransportAndCancelsReap(t *testing.T) {
	session := serverCaseSession(t, "lazy-sid", map[string]any{"session_key": "stored-lazy", "profile_home": nil})
	reapLedger(t)
	scheduleWSOrphanReap(context.Background(), "lazy-sid", nil, nil)
	timer := pendingReap("lazy-sid")
	if timer == nil {
		t.Fatal("no pending reap")
	}
	transport := &liveWritePeer{}
	resp := handleRequest(tuigateway.BindTransport(context.Background(), transport), map[string]any{"id": "lz1", "method": "session.resume",
		"params": map[string]any{"session_id": "stored-lazy", "omit_messages": true}})
	if resultOf(t, resp)["session_id"] != "lazy-sid" || session.Get("transport") != transport {
		t.Fatalf("resume %v", resp)
	}
	if pendingReap("lazy-sid") != nil || reapArmed(timer) {
		t.Fatal("unpersisted resume left the reap armed")
	}
}
