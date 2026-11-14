package agent

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type leaseFakeDB struct {
	mu         sync.Mutex
	exists     bool
	acquired   bool
	events     [][3]string
	onAcquire  func(o state.TurnLeaseWait)
	refreshErr error
}

func newLeaseFakeDB(exists, acquired bool) *leaseFakeDB {
	return &leaseFakeDB{exists: exists, acquired: acquired}
}

func (d *leaseFakeDB) record(kind, sid, holder string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, [3]string{kind, sid, holder})
}

func (d *leaseFakeDB) recorded() [][3]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][3]string(nil), d.events...)
}

func (d *leaseFakeDB) GetSession(_ context.Context, sessionID string) (*pyjson.Object, error) {
	if d.exists {
		return pyjson.ObjectOf("id", sessionID), nil
	}
	return nil, nil
}

func (d *leaseFakeDB) AcquireSessionTurnLease(sessionID, holder string, o state.TurnLeaseWait) (bool, error) {
	d.record("acquire", sessionID, holder)
	if d.onAcquire != nil {
		d.onAcquire(o)
	}
	return d.acquired, nil
}

func (d *leaseFakeDB) RefreshSessionTurnLease(string, string, float64, *float64) (bool, error) {
	if d.refreshErr != nil {
		return false, d.refreshErr
	}
	return true, nil
}

func (d *leaseFakeDB) ReleaseSessionTurnLease(sessionID, holder string) error {
	d.record("release", sessionID, holder)
	return nil
}

func (d *leaseFakeDB) ResolveResumeSessionID(sessionID string) (string, error) { return sessionID, nil }

func (d *leaseFakeDB) GetMessagesAsConversation(context.Context, string, state.ConversationOptions) ([]map[string]any, error) {
	return nil, nil
}

// Routes each test agent's admission to its own store and restores the seam after the test
func useLeaseStores(t *testing.T) map[*AIAgent]turnLeaseAdmissionStore {
	t.Helper()
	stores := map[*AIAgent]turnLeaseAdmissionStore{}
	prev := turnLeaseAdmissionStoreOf
	turnLeaseAdmissionStoreOf = func(a *AIAgent) turnLeaseAdmissionStore { return stores[a] }
	t.Cleanup(func() { turnLeaseAdmissionStoreOf = prev })
	return stores
}

// Disables the liveness watchdog as the hermes tests do by resolving a None timeout
func disableLivenessWatchdog(t *testing.T) {
	t.Helper()
	prev := turnLivenessConfig
	turnLivenessConfig = func(context.Context) map[string]any {
		return map[string]any{"agent": map[string]any{"turn_liveness": map[string]any{"timeout_s": 0, "poll_s": 1.0}}}
	}
	t.Cleanup(func() { turnLivenessConfig = prev })
}

type leaseStatuses struct {
	mu   sync.Mutex
	msgs []string
}

func (s *leaseStatuses) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func leaseAgent(stores map[*AIAgent]turnLeaseAdmissionStore, db turnLeaseAdmissionStore) (*AIAgent, *leaseStatuses) {
	statuses := &leaseStatuses{}
	interval := 60.0
	a := &AIAgent{sessionID: "s1", suppressStatusOutput: true}
	a.sessionTurnLeaseRefreshInterval = &interval
	a.statusCallback = func(_ string, msg any) {
		statuses.mu.Lock()
		defer statuses.mu.Unlock()
		statuses.msgs = append(statuses.msgs, fmt.Sprint(msg))
	}
	if stores != nil {
		stores[a] = db
	}
	return a, statuses
}

func admitLease(t *testing.T, a *AIAgent, history []map[string]any) *durableTurnAdmission {
	t.Helper()
	admission, err := admitDurableTurnLease(context.Background(), a, "s1", "s1:t:abcd",
		map[string]string{"session_id": "s1", "task_id": "t", "platform": "cli"}, history)
	if err != nil {
		t.Fatal(err)
	}
	return admission
}

// Records every interrupt the lease publishes instead of reaching the agent
func recordLeaseInterrupts(t *testing.T, now func() float64) func() []leaseInterruptCall {
	t.Helper()
	var mu sync.Mutex
	var calls []leaseInterruptCall
	prev := turnLeaseInterrupt
	turnLeaseInterrupt = func(_ *AIAgent, message string, _ InterruptOptions) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, leaseInterruptCall{at: now(), message: message})
		return true, nil
	}
	t.Cleanup(func() { turnLeaseInterrupt = prev })
	return func() []leaseInterruptCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]leaseInterruptCall(nil), calls...)
	}
}

type leaseInterruptCall struct {
	at      float64
	message string
}

func TestFreshRowLeasesWithSeedKeptAndPersistDisabledSkips(t *testing.T) {
	disableLivenessWatchdog(t)
	stores := useLeaseStores(t)
	seed := []map[string]any{{"role": "user", "content": "hi"}}
	a, _ := leaseAgent(stores, newLeaseFakeDB(false, true))
	admission := admitLease(t, a, seed)
	if admission.lease == nil || admission.earlyResult != nil {
		t.Fatalf("admission %+v", admission)
	}
	if len(admission.conversationHistory) != 1 || &admission.conversationHistory[0] != &seed[0] {
		t.Fatal("the seed history was not kept")
	}
	if a.sessionDBCreated {
		t.Fatal("a fresh row marked the session created")
	}
	admission.lease.Release()

	db := newLeaseFakeDB(true, true)
	disabled, _ := leaseAgent(stores, db)
	disabled.persistDisabled = true
	if admission := admitLease(t, disabled, seed); admission.lease != nil || len(db.recorded()) != 0 {
		t.Fatalf("persist disabled admission %+v events %v", admission, db.recorded())
	}
}

func TestAdmissionSetsHolderAttrsAndReleaseClearsThem(t *testing.T) {
	disableLivenessWatchdog(t)
	stores := useLeaseStores(t)
	db := newLeaseFakeDB(true, true)
	a, _ := leaseAgent(stores, db)
	lease, ok := admitLease(t, a, nil).lease.(*durableTurnLease)
	if !ok {
		t.Fatal("admission carries no durable lease")
	}
	if !a.sessionDBCreated || a.activeSessionTurnLeaseHolder == nil || *a.activeSessionTurnLeaseHolder != lease.holder || a.activeSessionTurnLeaseTTLSeconds != leaseTTLSeconds {
		t.Fatalf("holder attrs created %v holder %v ttl %v", a.sessionDBCreated, a.activeSessionTurnLeaseHolder, a.activeSessionTurnLeaseTTLSeconds)
	}
	if !strings.HasPrefix(lease.holder, "pid=") || !strings.Contains(lease.holder, ":platform=cli") {
		t.Fatalf("holder %q", lease.holder)
	}
	if lease.watchdog != nil || len(lease.timerHandles) != 0 || lease.IsTurnActive() {
		t.Fatal("an admitted lease started its timers before the turn")
	}
	lease.StopRefresher()
	lease.JoinThreads()
	lease.ClearInterrupt()
	lease.Release()
	want := [][3]string{{"acquire", "s1", lease.holder}, {"release", "s1", lease.holder}}
	if got := db.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %v", got)
	}
	if a.activeSessionTurnLeaseHolder != nil || a.activeSessionTurnLeaseTTLSeconds != 0 {
		t.Fatal("release left the holder attrs set")
	}
}

func TestReservedChildRowKeepsFirstTurnPersistence(t *testing.T) {
	disableLivenessWatchdog(t)
	stores := useLeaseStores(t)
	a, _ := leaseAgent(stores, newLeaseFakeDB(true, true))
	a.sessionDBReserved = true
	admission := admitLease(t, a, nil)
	defer admission.lease.Release()
	if a.sessionDBCreated {
		t.Fatal("a reserved identity suppressed the first turn metadata write")
	}
}

func TestTimeoutAndInterruptEarlyResults(t *testing.T) {
	stores := useLeaseStores(t)
	a, statuses := leaseAgent(stores, newLeaseFakeDB(true, false))
	admission := admitLease(t, a, []map[string]any{{"role": "user", "content": "x"}})
	early := admission.earlyResult
	if admission.lease != nil || early["failed"] != true || early["error"] != "session_turn_lease_timeout:s1" {
		t.Fatalf("early result %v", early)
	}
	if early["failure_reason"] != "session_busy" || early["failure_retryable"] != true {
		t.Fatalf("early result %v", early)
	}
	if !reflect.DeepEqual(early["messages"], []map[string]any{{"role": "user", "content": "x"}}) {
		t.Fatalf("messages %v", early["messages"])
	}
	if got := statuses.all(); len(got) != 1 || got[0] != early["final_response"] {
		t.Fatalf("timeout warning %v", got)
	}

	b, _ := leaseAgent(stores, newLeaseFakeDB(true, false))
	b.interruptRequested.Store(true)
	b.interruptMessage = "stop"
	early = admitLease(t, b, nil).earlyResult
	if early["interrupted"] != true || early["interrupt_message"] != "stop" {
		t.Fatalf("interrupt early result %v", early)
	}
	if b.interruptRequested.Load() || b.interruptMessage != "" {
		t.Fatal("the early return left the interrupt set for the next turn")
	}
}

func TestInterruptBeforeAnyWaitDoesNotBlameAnotherProcess(t *testing.T) {
	stores := useLeaseStores(t)
	a, _ := leaseAgent(stores, newLeaseFakeDB(true, false))
	a.interruptRequested.Store(true)
	reply, _ := admitLease(t, a, nil).earlyResult["final_response"].(string)
	if strings.Contains(reply, "another Icebath process") || !strings.Contains(reply, "not processed") {
		t.Fatalf("reply %q", reply)
	}
	held := newLeaseFakeDB(true, false)
	held.onAcquire = func(o state.TurnLeaseWait) { _ = o.OnWait(0.5) }
	b, statuses := leaseAgent(stores, held)
	b.interruptRequested.Store(true)
	reply, _ = admitLease(t, b, nil).earlyResult["final_response"].(string)
	if !strings.Contains(reply, "another Icebath process") {
		t.Fatalf("reply %q", reply)
	}
	if got := statuses.all(); len(got) != 1 || !strings.Contains(got[0], "waiting for it to finish") {
		t.Fatalf("wait notice %v", got)
	}
}

func TestCarryUnadmittedUserMessage(t *testing.T) {
	ts := 1234.5
	result := map[string]any{"interrupted": true, "messages": []map[string]any{{"role": "user", "content": "old"}}}
	carryUnadmittedUserMessage(result, "typed", "durable", &ts, "voice", map[string]any{"k": "v"}, "pm-1")
	msgs := result["messages"].([]map[string]any)
	want := map[string]any{"role": "user", "content": "durable", state.PersistAfterAdmissionInterrupt: true, "api_content": "typed",
		"display_kind": "voice", "display_metadata": map[string]any{"k": "v"}, "platform_message_id": "pm-1", "timestamp": 1234.5}
	if len(msgs) != 2 || !reflect.DeepEqual(msgs[1], want) {
		t.Fatalf("carried %v", msgs)
	}
	hard := map[string]any{"interrupted": true, "_hard_interrupted": true, "messages": []map[string]any{}}
	carryUnadmittedUserMessage(hard, "typed", nil, nil, "", nil, "")
	if _, left := hard["_hard_interrupted"]; left || len(hard["messages"].([]map[string]any)) != 0 {
		t.Fatalf("hard stop carried the input %v", hard)
	}
	timedOut := map[string]any{"failed": true, "messages": []map[string]any{}}
	carryUnadmittedUserMessage(timedOut, "typed", nil, nil, "", nil, "")
	if len(timedOut["messages"].([]map[string]any)) != 0 {
		t.Fatal("a timeout carried the input")
	}
}

func activeLease(t *testing.T, db turnLeaseStore) (*durableTurnLease, func() []leaseInterruptCall) {
	t.Helper()
	a, _ := leaseAgent(nil, nil)
	calls := recordLeaseInterrupts(t, leaseWallNow)
	lease := newDurableTurnLease(a, db, "s1", "h", nil)
	lease.turnActive = true
	return lease, calls
}

func TestRefreshTickSqliteLockKeepsTheTurn(t *testing.T) {
	db := newLeaseFakeDB(true, true)
	db.refreshErr = &state.SQLiteError{Class: state.SQLiteOperationalError, Msg: "database is locked"}
	lease, calls := activeLease(t, db)
	if !lease.RefreshTick() {
		t.Fatal("a lock cancelled the refresher")
	}
	if len(calls()) != 0 || lease.interruptMessage != "" || lease.stop.IsSet() {
		t.Fatalf("a lock interrupted the turn %v", calls())
	}
}

type movingClock struct {
	mu     sync.Mutex
	at     float64
	anchor time.Time
}

func (c *movingClock) time() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at + time.Since(c.anchor).Seconds()
}

func (c *movingClock) set(at float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.anchor = at, time.Now()
}

func TestLockToleranceNeverOutlivesTheCommittedRowExpiry(t *testing.T) {
	clock := &movingClock{at: 1000.0, anchor: time.Now()}
	prevNow := leaseWallNow
	leaseWallNow = clock.time
	restoreStore := state.SetWallClock(clock.time)
	t.Cleanup(func() { leaseWallNow = prevNow; restoreStore() })

	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.TryAcquireSessionTurnLease("s1", "h", leaseTTLSeconds, nil); err != nil || !ok {
		t.Fatalf("acquire %v %v", ok, err)
	}
	calls := recordLeaseInterrupts(t, clock.time)
	expires, err := db.SessionTurnLeaseExpiresAt("s1", "h")
	if err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{sessionID: "s1"}
	lease := newDurableTurnLease(a, db, "s1", "h", expires)
	lease.turnActive = true
	rowExpiry := func() float64 {
		committed, err := db.SessionTurnLeaseExpiresAt("s1", "h")
		if err != nil || committed == nil {
			t.Fatalf("the turn lost its lease row %v", err)
		}
		return *committed
	}
	writeLock := func() (*sql.DB, *sql.Conn) {
		raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := raw.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		return raw, conn
	}
	unlock := func(raw *sql.DB, conn *sql.Conn) {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
		_ = raw.Close()
	}

	clock.set(1060.0)
	admittedExpiry := rowExpiry()
	raw, conn := writeLock()
	refused := make(chan struct{})
	var once sync.Once
	db.SetWriteRetrySleep(func(deadline, patienceS float64) bool {
		once.Do(func() { close(refused) })
		return db.SleepBeforeWriteRetryJitter(deadline, patienceS)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		lease.RefreshTick()
	}()
	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		t.Fatal("renewal never reached the locked write")
	}
	unlock(raw, conn)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the delayed renewal never finished")
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("a delayed renewal interrupted the turn %v", got)
	}
	if rowExpiry() <= admittedExpiry {
		t.Fatal("the delayed renewal must have renewed the row")
	}
	if lease.authorityDeadline > rowExpiry() {
		t.Fatalf("deadline %v outlives the row %v", lease.authorityDeadline, rowExpiry())
	}

	clock.set(lease.authorityDeadline - 3.0)
	raw, conn = writeLock()
	keep := lease.RefreshTick()
	unlock(raw, conn)
	if keep {
		t.Fatal("a renewal blocked through the authority kept the refresher")
	}
	got := calls()
	if len(got) != 1 || got[0].message != "Session turn lease could not be refreshed; stopping to protect the transcript." {
		t.Fatalf("interrupts %v", got)
	}
	if got[0].at >= rowExpiry() {
		t.Fatalf("interrupt at %v after the row expiry %v", got[0].at, rowExpiry())
	}
}

func TestInterruptTurnOnlyWhileActive(t *testing.T) {
	a, _ := leaseAgent(nil, nil)
	calls := recordLeaseInterrupts(t, leaseWallNow)
	lease := newDurableTurnLease(a, newLeaseFakeDB(true, true), "s1", "h", nil)
	lease.interruptTurn("lost")
	if len(calls()) != 0 || lease.interruptMessage != "" {
		t.Fatal("an inactive turn was interrupted")
	}
	lease.turnActive = true
	lease.interruptTurn("lost")
	if got := calls(); len(got) != 1 || got[0].message != "lost" || lease.interruptMessage != "lost" {
		t.Fatalf("interrupts %v message %q", got, lease.interruptMessage)
	}
	lease.DeactivateAfterLivenessAbort()
	if !lease.stop.IsSet() || lease.IsTurnActive() {
		t.Fatal("deactivation left the turn active")
	}
}

func TestFinishedTurnLeaseDoesNotKeepTheAgentAliveWithoutGc(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ref := func() weak.Pointer[AIAgent] {
		a, _ := leaseAgent(nil, nil)
		lease := newDurableTurnLease(a, newLeaseFakeDB(true, true), "s1", "h", nil)
		if err := lease.BuildThreads(context.Background()); err != nil {
			t.Fatal(err)
		}
		if lease.watchdog == nil {
			t.Fatal("the liveness watchdog is on by default")
		}
		lease.Start(context.Background())
		lease.StopRefresher()
		lease.JoinThreads()
		lease.ClearInterrupt()
		lease.Release()
		return weak.Make(a)
	}()
	runtime.GC()
	runtime.GC()
	if ref.Value() != nil {
		t.Fatal("the finished turn's timers kept the agent alive")
	}
}

type tickOwner struct{ handle *cron.ScheduledHandle }

func (o *tickOwner) tick(context.Context) bool { return true }

func TestCancelledHandleReleasesItsOwnerWithoutGc(t *testing.T) {
	ref := func() weak.Pointer[tickOwner] {
		owner := &tickOwner{}
		owner.handle = cron.SchedulePeriodic(context.Background(), owner.tick, time.Hour)
		owner.handle.Cancel(0)
		return weak.Make(owner)
	}()
	runtime.GC()
	runtime.GC()
	if ref.Value() != nil {
		t.Fatal("the cancelled handle kept its owner alive")
	}
}
