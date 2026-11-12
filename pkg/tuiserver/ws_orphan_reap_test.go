package tuiserver

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Agent double whose plain interrupt runs a case body
type interruptOnlyAgent struct{ fn func() }

func (a interruptOnlyAgent) Interrupt(string) { a.fn() }

// Session record of the server cases with their default fields and the overrides
func serverCaseSession(t *testing.T, sid string, extra map[string]any) *tuigateway.Session {
	t.Helper()
	fields := map[string]any{"agent": &struct{}{}, "session_key": "session-key", "history": []any{}, "history_version": 0,
		"running": false, "attached_images": []any{}, "image_counter": 0, "cols": 80, "slash_worker": nil,
		"show_reasoning": false, "tool_progress_mode": "all"}
	transport, hasTransport := extra["transport"]
	for key, value := range extra {
		fields[key] = value
	}
	session := orphanRaceSession(t, sid, 600, fields)
	if hasTransport {
		session.Set("transport", transport)
	}
	return session
}

// Live async delegation of a UI session, removed when the case ends
func liveDelegation(t *testing.T, delegationID, sid string) func() {
	t.Helper()
	remove := tools.SetAsyncDelegationRecordForTests(delegationID, map[string]any{"delegation_id": delegationID, "status": "running",
		"origin_ui_session_id": sid})
	t.Cleanup(remove)
	return remove
}

func TestWsOrphanReapSparesTurnReattachedWithinGrace(t *testing.T) {
	var interrupted []string
	disconnecting := &orphanRacePeer{id: 30}
	session := serverCaseSession(t, "reattached-sid", map[string]any{"transport": disconnecting, "running": true,
		"agent": interruptOnlyAgent{func() { interrupted = append(interrupted, "interrupted") }}, "_run_thread": liveTurnThread(t)})
	closeSessionsForTransport(context.Background(), disconnecting, "ws_disconnect")
	if session.Get("transport") != tuigateway.DetachedWSTransport() {
		t.Fatalf("transport %v", session.Get("transport"))
	}
	session.Set("transport", &orphanRacePeer{id: 31})
	fireReap(pendingReap("reattached-sid"), 20, 20)
	if len(interrupted) != 0 || tuigateway.SessionRecord("reattached-sid") != session || pendingReap("reattached-sid") != nil {
		t.Fatalf("interrupted %v pending %v", interrupted, pendingReap("reattached-sid"))
	}
}

func TestWsOrphanReapDefersRunningTurnForActiveDelegation(t *testing.T) {
	var interrupted []string
	var session *tuigateway.Session
	session = serverCaseSession(t, "delegating-turn", map[string]any{"running": true, "_run_thread": liveTurnThread(t),
		"agent": interruptOnlyAgent{func() { interrupted = append(interrupted, "interrupted"); session.Set("running", false) }}})
	orphanRaceStale(t, 0)
	remove := liveDelegation(t, "deleg_defer_running", "delegating-turn")
	scheduleWSOrphanReap(context.Background(), "delegating-turn", nil, nil)
	first := pendingReap("delegating-turn")
	fireReap(first, 20, 20)
	if second := pendingReap("delegating-turn"); len(interrupted) != 0 || second == nil || second == first {
		t.Fatalf("interrupted %v while a delegation ran", interrupted)
	}
	remove()
	fireReap(pendingReap("delegating-turn"), 20, 20)
	if len(interrupted) != 1 || pendingReap("delegating-turn") == nil {
		t.Fatalf("interrupted %v", interrupted)
	}
	fireReap(pendingReap("delegating-turn"), 1, 1)
	if tuigateway.SessionRecord("delegating-turn") != nil {
		t.Fatal("settled turn was not reaped")
	}
}

func TestWsOrphanReapInterruptsInProcessTurn(t *testing.T) {
	var interrupted []string
	var session *tuigateway.Session
	session = serverCaseSession(t, "inline-sid", map[string]any{"running": true, "_run_thread": liveTurnThread(t),
		"agent": interruptOnlyAgent{func() { interrupted = append(interrupted, "interrupted"); session.Set("running", false) }}})
	orphanRaceStale(t, 0)
	scheduleWSOrphanReap(context.Background(), "inline-sid", nil, nil)
	first := pendingReap("inline-sid")
	fireReap(first, 20, 20)
	if len(interrupted) != 1 || session.Get("_turn_cancel_requested") != true {
		t.Fatalf("interrupted %v cancel %v", interrupted, session.Get("_turn_cancel_requested"))
	}
	if next := pendingReap("inline-sid"); next == nil || next == first {
		t.Fatal("interrupt poll was not armed")
	}
}

func TestWsDisconnectRunningSidecarStillClosesWithoutOrphanTimer(t *testing.T) {
	transport := &orphanRacePeer{id: 32}
	session := serverCaseSession(t, "sidecar-sid", map[string]any{"transport": transport, "running": true, "close_on_disconnect": true})
	reaped, detached := closeSessionsForTransport(context.Background(), transport, "ws_disconnect")
	if reaped != 1 || detached != 0 {
		t.Fatalf("close returned (%d, %d)", reaped, detached)
	}
	if tuigateway.SessionRecord("sidecar-sid") != nil || !truthy(session.Get("_finalized")) {
		t.Fatal("sidecar session was not torn down")
	}
	if pendingReap("sidecar-sid") != nil {
		t.Fatal("orphan timer armed for a sidecar")
	}
}

func TestWsOrphanReapReschedulesWhileMidTurnThenReaps(t *testing.T) {
	session := serverCaseSession(t, "midturn-sid", map[string]any{"running": true})
	scheduleWSOrphanReap(context.Background(), "midturn-sid", nil, nil)
	first := pendingReap("midturn-sid")
	fireReap(first, 20, 20)
	if tuigateway.SessionRecord("midturn-sid") != session {
		t.Fatal("mid turn session was reaped")
	}
	next := pendingReap("midturn-sid")
	if next == nil || next == first || truthy(session.Get("_finalized")) {
		t.Fatal("mid turn reap was not rescheduled")
	}
	session.Set("running", false)
	fireReap(next, 1, 1)
	if tuigateway.SessionRecord("midturn-sid") != nil || !truthy(session.Get("_finalized")) {
		t.Fatal("settled session was not torn down")
	}
}

func TestWsOrphanReapWaitsForActiveDelegationThenReaps(t *testing.T) {
	session := serverCaseSession(t, "delegating-sid", nil)
	remove := liveDelegation(t, "deleg_ws_orphan_reap_test", "delegating-sid")
	scheduleWSOrphanReap(context.Background(), "delegating-sid", nil, nil)
	first := pendingReap("delegating-sid")
	fireReap(first, 20, 20)
	next := pendingReap("delegating-sid")
	if tuigateway.SessionRecord("delegating-sid") != session || next == nil || next == first || truthy(session.Get("_finalized")) {
		t.Fatal("session with a running delegation was reaped")
	}
	tools.SetAsyncDelegationRecordForTests("deleg_ws_orphan_reap_test", map[string]any{"delegation_id": "deleg_ws_orphan_reap_test",
		"status": "completed", "origin_ui_session_id": "delegating-sid"})
	fireReap(next, 20, 20)
	remove()
	if tuigateway.SessionRecord("delegating-sid") != nil || !truthy(session.Get("_finalized")) {
		t.Fatal("session was not reaped after its delegation completed")
	}
}

// Slash worker double counting its closes
type countingWorker struct{ closes int }

func (w *countingWorker) Close() { w.closes++ }

func TestFinalizeSessionClosesSlashWorker(t *testing.T) {
	ctx := context.Background()
	worker := &countingWorker{}
	session := serverCaseSession(t, "slash-worker-sid", map[string]any{"slash_worker": worker})
	finalizeSession(ctx, session, "tui_close")
	if worker.closes != 1 || session.Get("_finalized") != true {
		t.Fatalf("closes %d finalized %v", worker.closes, session.Get("_finalized"))
	}
	finalizeSession(ctx, session, "tui_close")
	teardownSession(ctx, session, "tui_close")
	if worker.closes != 1 {
		t.Fatalf("worker closed %d times", worker.closes)
	}
}

// Live window whose socket can close under the session
type closablePeer struct {
	recordingTransport
	closed atomic.Bool
}

func (p *closablePeer) Closed() bool { return p.closed.Load() }

// Waits for the frames a window received
func receivedFrames(peer *closablePeer, want int) []map[string]any {
	deadline := time.Now().Add(5 * time.Second)
	for {
		peer.mu.Lock()
		frames := append([]map[string]any(nil), peer.frames...)
		peer.mu.Unlock()
		if len(frames) >= want || time.Now().After(deadline) {
			return frames
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseTransportRebindsSessionToRemainingViewer(t *testing.T) {
	main, popout := &closablePeer{}, &closablePeer{}
	session := serverCaseSession(t, "multi-sid", map[string]any{"transport": nil})
	tuigateway.AttachSessionTransport(session, main)
	tuigateway.AttachSessionTransport(session, popout)
	tuigateway.SetSessionViewer(session, main, 100.0)
	tuigateway.SetSessionViewer(session, popout, 200.0)
	if _, fanout := session.Get("transport").(*tuigateway.FanoutTransport); !fanout {
		t.Fatalf("transport %T", session.Get("transport"))
	}
	reaped, detached := closeSessionsForTransport(context.Background(), popout, "ws_disconnect")
	if reaped != 0 || detached != 0 {
		t.Fatalf("close returned (%d, %d)", reaped, detached)
	}
	fanout, _ := session.Get("transport").(*tuigateway.FanoutTransport)
	if fanout == nil || !fanout.Contains(main) || fanout.Contains(popout) {
		t.Fatalf("transport after close %v", session.Get("transport"))
	}
	if pendingReap("multi-sid") != nil || wsSessionIsOrphaned(session) {
		t.Fatal("watched session was handed to the orphan reaper")
	}
	tuigateway.Emit(context.Background(), "message.delta", "multi-sid", map[string]any{"text": "still here"})
	frames := receivedFrames(main, 1)
	if len(frames) != 1 || frames[0]["params"].(map[string]any)["type"] != "message.delta" {
		t.Fatalf("main frames %v", frames)
	}
	if left := receivedFrames(popout, 0); len(left) != 0 {
		t.Fatalf("departed window frames %v", left)
	}
}

func TestCloseTransportDetachesWhenNoViewersRemain(t *testing.T) {
	only := &closablePeer{}
	session := serverCaseSession(t, "solo-sid", map[string]any{"transport": only})
	tuigateway.SetSessionViewer(session, only, 100.0)
	reaped, detached := closeSessionsForTransport(context.Background(), only, "ws_disconnect")
	if reaped != 0 || detached != 1 || session.Get("transport") != tuigateway.DetachedWSTransport() {
		t.Fatalf("close returned (%d, %d) transport %v", reaped, detached, session.Get("transport"))
	}
	if pendingReap("solo-sid") == nil {
		t.Fatal("orphan reap was not armed")
	}
}

func TestCloseTransportSkipsDeadRemainingViewers(t *testing.T) {
	dead, popout := &closablePeer{}, &closablePeer{}
	session := serverCaseSession(t, "dead-viewer-sid", map[string]any{"transport": nil})
	tuigateway.AttachSessionTransport(session, dead)
	tuigateway.AttachSessionTransport(session, popout)
	tuigateway.SetSessionViewer(session, dead, 100.0)
	tuigateway.SetSessionViewer(session, popout, 200.0)
	if _, fanout := session.Get("transport").(*tuigateway.FanoutTransport); !fanout {
		t.Fatalf("transport %T", session.Get("transport"))
	}
	dead.closed.Store(true)
	reaped, detached := closeSessionsForTransport(context.Background(), popout, "ws_disconnect")
	if reaped != 0 || detached != 1 || session.Get("transport") != tuigateway.DetachedWSTransport() {
		t.Fatalf("close returned (%d, %d) transport %v", reaped, detached, session.Get("transport"))
	}
	if pendingReap("dead-viewer-sid") == nil {
		t.Fatal("orphan reap was not armed")
	}
}

func TestWsOrphanReapSparesReattachedSession(t *testing.T) {
	cases := map[string]map[string]any{
		"reattached": {"transport": &orphanRacePeer{id: 40}, "running": false},
		"mid_turn":   {"transport": tuigateway.DetachedWSTransport(), "running": true},
		"done":       {"transport": tuigateway.DetachedWSTransport(), "running": false, "_finalized": true},
	}
	for name, fields := range cases {
		if wsSessionIsOrphaned(tuigateway.NewSession(fields)) {
			t.Fatalf("%s session reads as orphaned", name)
		}
	}
}

func TestWsOrphanReapStillFiresWhenNeverResumed(t *testing.T) {
	session := serverCaseSession(t, "lonely-sid", nil)
	scheduleWSOrphanReap(context.Background(), "lonely-sid", nil, nil)
	pending := pendingReap("lonely-sid")
	if pending == nil {
		t.Fatal("reap was not armed")
	}
	fireReap(pending, 20, 20)
	if tuigateway.SessionRecord("lonely-sid") != nil || pendingReap("lonely-sid") != nil || !truthy(session.Get("_finalized")) {
		t.Fatal("never resumed session was not reaped")
	}
}

func TestWsOrphanReapSparesDetachedSessionWithRunningAsyncDelegation(t *testing.T) {
	h := t.TempDir()
	session := serverCaseSession(t, "bg-sid", map[string]any{"session_key": "sess_bg"})
	db := ledgerHandle(t, filepath.Join(h, "state.db"))
	useSharedDB(t, db)
	if err := db.CreateSession(state.NewSession{ID: "sess_bg", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	record := func(status string) {
		tools.SetAsyncDelegationRecordForTests("deleg_bg", map[string]any{"delegation_id": "deleg_bg", "status": status,
			"session_key": "sess_bg", "origin_ui_session_id": "bg-sid"})
	}
	record("running")
	t.Cleanup(func() { tools.SetAsyncDelegationRecordForTests("deleg_bg", nil)() })
	scheduleWSOrphanReap(context.Background(), "bg-sid", nil, nil)
	for _, status := range []string{"finalizing", "completed"} {
		before := pendingReap("bg-sid")
		fireReap(before, 20, 20)
		after := pendingReap("bg-sid")
		if tuigateway.SessionRecord("bg-sid") != session || after == nil || after == before || truthy(session.Get("_finalized")) {
			t.Fatalf("session reaped while its delegation was live (next status %s)", status)
		}
		record(status)
	}
	fireReap(pendingReap("bg-sid"), 20, 20)
	if tuigateway.SessionRecord("bg-sid") != nil || !truthy(session.Get("_finalized")) {
		t.Fatal("session kept after its delegation completed")
	}
}

func TestWsOrphanReapDisabledWhenGraceZero(t *testing.T) {
	prev := wsOrphanReapGraceS
	wsOrphanReapGraceS = func() float64 { return 0 }
	t.Cleanup(func() { wsOrphanReapGraceS = prev })
	scheduleWSOrphanReap(context.Background(), "any-sid", nil, nil)
	if pendingReap("any-sid") != nil {
		t.Fatal("timer armed with a zero grace")
	}
}

// Agent double with a settable activity clock and a recorded interrupt
type activityAgent struct {
	mu          sync.Mutex
	summary     map[string]any
	fail        bool
	interrupted []string
}

func (a *activityAgent) GetActivitySummary() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		panic("summary unavailable")
	}
	return maps.Clone(a.summary)
}

func (a *activityAgent) Interrupt(string) {
	a.mu.Lock()
	a.interrupted = append(a.interrupted, "interrupted")
	a.mu.Unlock()
}

func (a *activityAgent) set(key string, value any) {
	a.mu.Lock()
	a.summary[key] = value
	a.mu.Unlock()
}

func (a *activityAgent) interrupts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.interrupted)
}

func TestWsOrphanReapDefersRunningTurnWithFreshActivity(t *testing.T) {
	fake := &activityAgent{summary: map[string]any{"seconds_since_activity": 1.0}}
	session := serverCaseSession(t, "fresh-sid", map[string]any{"agent": fake, "running": true})
	orphanRaceStale(t, 300)
	scheduleWSOrphanReap(context.Background(), "fresh-sid", nil, nil)
	for range 2 {
		fireReap(pendingReap("fresh-sid"), 20, 20)
		next := pendingReap("fresh-sid")
		if len(fake.interrupts()) != 0 || truthy(session.Get("_client_gone_interrupt_requested")) || next == nil || next.interval != 20 ||
			tuigateway.SessionRecord("fresh-sid") != session {
			t.Fatalf("fresh turn was not deferred: interrupts %v", fake.interrupts())
		}
	}
	fake.set("seconds_since_activity", 301.0)
	fireReap(pendingReap("fresh-sid"), 20, 20)
	if !slices.Equal(fake.interrupts(), []string{"interrupted"}) || session.Get("_client_gone_interrupt_requested") != true {
		t.Fatalf("stale turn interrupts %v", fake.interrupts())
	}
	session.Set("running", false)
	fireReap(pendingReap("fresh-sid"), 1, 1)
	if tuigateway.SessionRecord("fresh-sid") != nil || !truthy(session.Get("_finalized")) {
		t.Fatal("settled turn was not reaped")
	}
}

func TestWsOrphanActivityGateZeroRestoresInterruptAtGrace(t *testing.T) {
	fake := &activityAgent{summary: map[string]any{"seconds_since_activity": 0.5}}
	session := serverCaseSession(t, "optout-sid", map[string]any{"agent": fake, "running": true, "_run_thread": liveTurnThread(t)})
	orphanRaceStale(t, 0)
	scheduleWSOrphanReap(context.Background(), "optout-sid", nil, nil)
	fireReap(pendingReap("optout-sid"), 20, 20)
	if !slices.Equal(fake.interrupts(), []string{"interrupted"}) || session.Get("_client_gone_interrupt_requested") != true {
		t.Fatalf("interrupts %v", fake.interrupts())
	}
}

func TestWsOrphanActivityGateUnreadableSummaryStaysEligible(t *testing.T) {
	orphanRaceStale(t, 300)
	t.Cleanup(func() { wsOrphanActivityStaleS = func() float64 { return 600 } })
	for name, held := range map[string]any{
		"raises":   &activityAgent{fail: true},
		"none":     nil,
		"no_clock": &struct{}{},
		"no_value": &activityAgent{summary: map[string]any{"seconds_since_activity": nil}},
	} {
		if wsOrphanTurnActivityIsFresh(tuigateway.NewSession(map[string]any{"agent": held})) {
			t.Fatalf("%s summary reads as fresh", name)
		}
	}
}

// Agent double counting its closes
type closingAgent struct{ closes int }

func (a *closingAgent) CloseContext(context.Context) error {
	a.closes++
	return nil
}

func TestCloseSessionByIdIsIdempotentAndFull(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	worker, closer := &countingWorker{}, &closingAgent{}
	session := tuigateway.NewSession(map[string]any{"session_key": "k1", "agent": closer, "slash_worker": worker})
	swapSessions(t, map[string]*tuigateway.Session{"sid-1": session})
	tools.QueueGatewayApprovalForTests("k1", map[string]any{"command": "danger"})
	t.Cleanup(func() { tools.UnregisterGatewayNotify("k1") })
	if !closeSessionByID(ctx, "sid-1", "ws_disconnect", nil) {
		t.Fatal("first close reported nothing closed")
	}
	if closeSessionByID(ctx, "sid-1", "ws_disconnect", nil) {
		t.Fatal("second close reported a close")
	}
	if worker.closes != 1 || closer.closes != 1 || session.Get("_finalized") != true {
		t.Fatalf("worker %d agent %d finalized %v", worker.closes, closer.closes, session.Get("_finalized"))
	}
	if tools.GetPendingGatewayApproval("k1") != nil {
		t.Fatal("approval route was not unregistered")
	}
	if tuigateway.SessionRecord("sid-1") != nil {
		t.Fatal("session still registered")
	}
}

func TestAttachWorkerClosesOrphanWhenSessionAlreadyTornDown(t *testing.T) {
	swapSessions(t, map[string]*tuigateway.Session{})
	worker := &countingWorker{}
	detached := tuigateway.NewSession(map[string]any{"session_key": "k"})
	attachWorker("gone", detached, worker)
	if _, stored := detached.Lookup("slash_worker"); worker.closes != 1 || stored || tuigateway.SessionRecord("gone") != nil {
		t.Fatalf("closes %d stored %v", worker.closes, stored)
	}
}

// Worker that fails the case when closed
type mustStayOpenWorker struct{ t *testing.T }

func (w mustStayOpenWorker) Close() { w.t.Error("must not close a worker for a live session") }

func TestAttachWorkerStoresWorkerOnLiveSession(t *testing.T) {
	live := tuigateway.NewSession(map[string]any{"session_key": "k"})
	swapSessions(t, map[string]*tuigateway.Session{"live": live})
	worker := mustStayOpenWorker{t}
	attachWorker("live", live, worker)
	if live.Get("slash_worker") != any(worker) {
		t.Fatalf("worker %v", live.Get("slash_worker"))
	}
}

func TestCloseSessionsForTransportClosesFlaggedRepointsRest(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prev := wsOrphanReapGraceS
	wsOrphanReapGraceS = func() float64 { return 0 }
	t.Cleanup(func() { wsOrphanReapGraceS = prev })
	transport := &orphanRacePeer{id: 50}
	flagged := tuigateway.NewSession(map[string]any{"transport": transport, "close_on_disconnect": true})
	rest := tuigateway.NewSession(map[string]any{"transport": transport, "close_on_disconnect": false})
	swapSessions(t, map[string]*tuigateway.Session{"a": flagged, "b": rest})
	closeSessionsForTransport(context.Background(), transport, "ws_disconnect")
	if tuigateway.SessionRecord("a") != nil || !truthy(flagged.Get("_finalized")) {
		t.Fatal("flagged session was not closed")
	}
	if tuigateway.SessionRecord("b") != rest || truthy(rest.Get("_finalized")) || rest.Get("transport") != tuigateway.DetachedWSTransport() {
		t.Fatalf("rest session transport %v", rest.Get("transport"))
	}
}
