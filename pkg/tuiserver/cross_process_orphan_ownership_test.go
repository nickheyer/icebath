package tuiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Child side of a lease holder process, holding a desktop lease from the ready file until the release file
func sessionLeaseHolderChild() {
	fail := func(msg string) { os.Stderr.WriteString(msg + "\n"); os.Exit(1) }
	lease, refusal, err := activesessions.TryAcquire(context.Background(), os.Getenv("SESSION_ID"), "desktop", map[string]any{}, nil, "", true)
	if err != nil || lease == nil || refusal != nil {
		fail(fmt.Sprintf("acquire failed %v %v", err, refusal))
	}
	if err := os.WriteFile(os.Getenv("READY_FILE"), []byte("ready"), 0o644); err != nil {
		fail(err.Error())
	}
	deadline := time.Now().Add(120 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("RELEASE_FILE")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fail("timed out waiting for release signal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := lease.Release(); err != nil {
		fail(err.Error())
	}
}

// Another process holding the desktop lease of one session in a home
type leaseHolder struct {
	cmd     *exec.Cmd
	out     bytes.Buffer
	release string
	done    chan struct{}
	err     error
}

// Starts a lease holder on home and waits until it owns the lease
func spawnLeaseHolder(t *testing.T, h, sessionID string) *leaseHolder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "child-ready")
	holder := &leaseHolder{release: filepath.Join(dir, "child-release"), done: make(chan struct{})}
	holder.cmd = exec.Command(exe)
	holder.cmd.Env = append(os.Environ(), "ICEBATH_HOME="+h, "ICEBATH_TEST_CHILD=session-lease-holder", "SESSION_ID="+sessionID,
		"READY_FILE="+ready, "RELEASE_FILE="+holder.release)
	holder.cmd.Stdout, holder.cmd.Stderr = &holder.out, &holder.out
	if err := holder.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { holder.err = holder.cmd.Wait(); close(holder.done) }()
	t.Cleanup(holder.stop)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return holder
		}
		select {
		case <-holder.done:
			t.Fatalf("lease holder exited before ready: %v\n%s", holder.err, holder.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for lease holder")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Signals release and waits for the child, killing it when it hangs
func (h *leaseHolder) stop() {
	_ = os.WriteFile(h.release, []byte("release"), 0o644)
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.done
	}
}

// Releases the child lease and fails unless the child exited cleanly
func (h *leaseHolder) finish(t *testing.T) {
	t.Helper()
	h.stop()
	if h.err != nil {
		t.Fatalf("lease holder: %v\n%s", h.err, h.out.String())
	}
}

// Fresh launch home with its own session store installed as the shared store
func ownershipHome(t *testing.T) (string, *state.DB) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db := ledgerHandle(t, filepath.Join(h, "state.db"))
	useSharedDB(t, db)
	return h, db
}

// Registry entries of a home
func registryEntries(t *testing.T, registryHome string) []map[string]any {
	t.Helper()
	entries, err := activesessions.RegistrySnapshot(context.Background(), registryHome, false)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, len(entries))
	for i, entry := range entries {
		out[i], _ = pyjson.Plain(entry).(map[string]any)
	}
	return out
}

// Stored session row with a source in the store of a home, the handle kept open for the case
func storedRow(t *testing.T, h, sessionID, source string) string {
	t.Helper()
	dbPath := filepath.Join(h, "state.db")
	if err := ledgerHandle(t, dbPath).CreateSession(state.NewSession{ID: sessionID, Source: source}); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

// Fails when the stored row was ended
func assertRowOpen(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	if endedAt, endReason := readRealRow(t, dbPath, sessionID); endedAt != nil || endReason != nil {
		t.Fatalf("row ended: %v %v", endedAt, endReason)
	}
}

// Desktop record of a profile session with no lease and no agent, as the automatic cleanup cases build it
func desktopCleanupRecord(profileHome, sessionID string, lease any) *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"active_session_lease": lease, "agent": nil, "history": []any{},
		"profile_home": profileHome, "session_key": sessionID, "slash_worker": nil, "source": "desktop"})
}

// Agent double recording an interrupt request
type takeoverAgent struct {
	mu          sync.Mutex
	interrupted bool
}

func (a *takeoverAgent) Interrupt(string) {
	a.mu.Lock()
	a.interrupted = true
	a.mu.Unlock()
}

func (a *takeoverAgent) wasInterrupted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interrupted
}

// Runtime record of a desktop chat on a transport
func runtimeRecord(sessionKey string, transport tuigateway.Transport, running bool) *tuigateway.Session {
	now := nowSeconds()
	return tuigateway.NewSession(map[string]any{"agent": &takeoverAgent{}, "agent_ready": nil, "transport": transport,
		"running": running, "last_active": now, "created_at": now, "source": "desktop", "session_key": sessionKey,
		"history": []any{}, "active_session_lease": nil, "profile_home": nil})
}

// Lease a record holds
func recordLease(session *tuigateway.Session) *activesessions.Lease {
	lease, _ := session.Get("active_session_lease").(*activesessions.Lease)
	return lease
}

// Interrupt double of a record
func recordAgent(session *tuigateway.Session) *takeoverAgent {
	return session.Get("agent").(*takeoverAgent)
}

// Makes the registry of a home unusable by putting a file where its runtime directory belongs
func breakRegistry(t *testing.T, h string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "runtime"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanGuardFailsClosedWhenRegistryIsUnavailable(t *testing.T) {
	h, _ := ownershipHome(t)
	breakRegistry(t, h)
	var seen []bool
	otherRuntimeLeaseGuard(context.Background(), "preserved-session", tuigateway.NewSession(map[string]any{"profile_home": nil}),
		func(otherOwns bool) { seen = append(seen, otherOwns) })
	if !slices.Equal(seen, []bool{true}) {
		t.Fatalf("guard yielded %v", seen)
	}
}

func TestDesktopClaimFailsClosedWhenRegistrySetupFails(t *testing.T) {
	h, _ := ownershipHome(t)
	breakRegistry(t, h)
	ctx := context.Background()
	desktopLease, desktopRefusal := tuigateway.ClaimActiveSessionSlotRefusal(ctx, "desktop-session", "desktop-runtime", "desktop", "")
	tuiLease, tuiRefusal := tuigateway.ClaimActiveSessionSlotRefusal(ctx, "tui-session", "tui-runtime", "tui", "")
	if desktopLease != nil || desktopRefusal == nil || desktopRefusal.Message != tuigateway.SessionOwnershipUnavailable {
		t.Fatalf("desktop claim %v %v", desktopLease, desktopRefusal)
	}
	if tuiLease != nil || tuiRefusal == nil || tuiRefusal.Message != tuigateway.SessionOwnershipUnavailable {
		t.Fatalf("tui claim %v %v", tuiLease, tuiRefusal)
	}
}

// Liveness tracked lease whose first registry write fails
type flakyLease struct {
	mu       sync.Mutex
	calls    int
	released bool
}

func (l *flakyLease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls == 1 {
		return errors.New("replace failed once")
	}
	l.released = true
	return nil
}

func (l *flakyLease) Released() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

func (*flakyLease) IsEnabled() bool      { return true }
func (*flakyLease) TracksLiveness() bool { return true }

func TestServerReleaseRetriesLivenessLeaseBeforeDroppingReference(t *testing.T) {
	lease := &flakyLease{}
	session := tuigateway.NewSession(map[string]any{"active_session_lease": lease})
	if !releaseActiveSessionSlot(session) {
		t.Fatal("release reported failure")
	}
	if lease.calls != 2 {
		t.Fatalf("release calls %d", lease.calls)
	}
	if _, held := session.Lookup("active_session_lease"); held {
		t.Fatal("lease reference kept")
	}
}

func TestAutomaticCleanupPreservesCorruptRegistryWithoutOverwrite(t *testing.T) {
	ownershipHome(t)
	profileHome := filepath.Join(t.TempDir(), "profile-home")
	statePath := filepath.Join(profileHome, "runtime", "active_sessions.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := "{not-json"
	if err := os.WriteFile(statePath, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := storedRow(t, profileHome, "preserved-session", "desktop")
	finalizeSession(context.Background(), desktopCleanupRecord(profileHome, "preserved-session", nil), "idle_timeout")
	assertRowOpen(t, dbPath, "preserved-session")
	if raw, err := os.ReadFile(statePath); err != nil || string(raw) != corrupt {
		t.Fatalf("registry rewritten: %q %v", raw, err)
	}
}

func TestOwnLiveLeaseIdsReportsLiveOwnersAndSkipsTheExcluded(t *testing.T) {
	first, second := &activesessions.Lease{LeaseID: "first"}, &activesessions.Lease{LeaseID: "second"}
	swapSessions(t, map[string]*tuigateway.Session{
		"one":   tuigateway.NewSession(map[string]any{"active_session_lease": first}),
		"two":   tuigateway.NewSession(map[string]any{"active_session_lease": second}),
		"three": tuigateway.NewSession(map[string]any{"active_session_lease": nil}),
	})
	if got := ownLiveLeaseIDs(nil); len(got) != 2 || !got["first"] || !got["second"] {
		t.Fatalf("live ids %v", got)
	}
	if got := ownLiveLeaseIDs(first); len(got) != 1 || !got["second"] {
		t.Fatalf("live ids excluding first %v", got)
	}
}

// Moves every registry entry of a home back past the self orphan grace, an owner that vanished minutes ago
func backdateRegistry(t *testing.T, registryHome string, seconds float64) {
	t.Helper()
	statePath := filepath.Join(registryHome, "runtime", "active_sessions.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	entries, isList := data.([]any)
	if wrapped, isDict := data.(map[string]any); isDict {
		entries, isList = wrapped["entries"].([]any)
	}
	if !isList || len(entries) == 0 {
		t.Fatalf("registry %s", raw)
	}
	for _, entry := range entries {
		e := entry.(map[string]any)
		e["started_at"] = e["started_at"].(float64) - seconds
	}
	if raw, err = json.Marshal(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticCleanupReclaimsOwnOrphanLeaseNotTreatedAsSibling(t *testing.T) {
	ownershipHome(t)
	profileHome := filepath.Join(t.TempDir(), "profile-home")
	sessionID := "own-orphan-session"
	ownerLease, refusal := tuigateway.ClaimActiveSessionSlotRefusal(context.Background(), sessionID, "vanished-runtime", "desktop", profileHome)
	if ownerLease == nil || refusal != nil {
		t.Fatalf("claim %v %v", ownerLease, refusal)
	}
	backdateRegistry(t, profileHome, 60)
	dbPath := storedRow(t, profileHome, sessionID, "desktop")
	swapSessions(t, map[string]*tuigateway.Session{})
	finalizeSession(context.Background(), desktopCleanupRecord(profileHome, sessionID, nil), "ws_orphan_reap")
	assertRowOpen(t, dbPath, sessionID)
	if entries := registryEntries(t, profileHome); len(entries) != 0 {
		t.Fatalf("orphan lease kept %v", entries)
	}
}

func TestAutomaticDesktopCleanupPreservesSiblingAndReleasesSoleOwnerLease(t *testing.T) {
	ctx := context.Background()
	ownershipHome(t)
	profileHome := filepath.Join(t.TempDir(), "profile-home")
	sessionID := "shared-profile-session"
	dbPath := storedRow(t, profileHome, sessionID, "desktop")
	child := spawnLeaseHolder(t, profileHome, sessionID)
	reasons := []string{"ws_orphan_reap", "ws_disconnect", "idle_timeout", "lru_evict", "tui_shutdown"}
	if len(automaticSessionEndReasons) != len(reasons) {
		t.Fatalf("automatic reasons %v", automaticSessionEndReasons)
	}
	for _, reason := range reasons {
		if !automaticSessionEndReasons[reason] {
			t.Fatalf("%s is not an automatic reason", reason)
		}
	}
	refused, refusal := tuigateway.ClaimActiveSessionSlotRefusal(ctx, sessionID, "local-runtime", "desktop", profileHome)
	if refused != nil || refusal == nil || refusal.Reason != activesessions.SessionNotOwned {
		t.Fatalf("second claim %v %v", refused, refusal)
	}
	if entries := registryEntries(t, profileHome); len(entries) != 1 {
		t.Fatalf("registry %v", entries)
	}
	for _, reason := range reasons {
		finalizeSession(ctx, desktopCleanupRecord(profileHome, sessionID, nil), reason)
		assertRowOpen(t, dbPath, sessionID)
		remaining := registryEntries(t, profileHome)
		if len(remaining) != 1 || remaining[0]["session_id"] != sessionID {
			t.Fatalf("%s left registry %v", reason, remaining)
		}
	}
	finalizeSession(ctx, desktopCleanupRecord(profileHome, sessionID, nil), "tui_close")
	if endedAt, endReason := readRealRow(t, dbPath, sessionID); endedAt == nil || endReason != "tui_close" {
		t.Fatalf("explicit close did not end the row: %v %v", endedAt, endReason)
	}
	reaperExec(t, ledgerHandle(t, dbPath), "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", sessionID)
	child.finish(t)
	if entries := registryEntries(t, profileHome); len(entries) != 0 {
		t.Fatalf("child lease left %v", entries)
	}
	for index, reason := range reasons {
		soleLease, message := tuigateway.ClaimActiveSessionSlotRefusal(ctx, sessionID, fmt.Sprintf("sole-runtime-%d", index), "desktop", profileHome)
		if soleLease == nil || message != nil {
			t.Fatalf("sole claim %v %v", soleLease, message)
		}
		finalizeSession(ctx, desktopCleanupRecord(profileHome, sessionID, soleLease), reason)
		if entries := registryEntries(t, profileHome); len(entries) != 0 {
			t.Fatalf("%s kept the sole lease %v", reason, entries)
		}
	}
	assertRowOpen(t, dbPath, sessionID)
}

func TestNewRuntimeTakesOverDetachedSiblingLeaseInSameProcess(t *testing.T) {
	ctx := context.Background()
	ownershipHome(t)
	key := "restored-chat"
	old := runtimeRecord(key, tuigateway.DetachedWSTransport(), true)
	fresh := runtimeRecord(key, &orphanRacePeer{id: 10}, false)
	swapSessions(t, map[string]*tuigateway.Session{"old": old, "new": fresh})
	if refusal := ensureActiveSessionSlot(ctx, "old", old); refusal != nil {
		t.Fatalf("old claim %v", refusal)
	}
	lease := recordLease(old)
	if refusal := ensureActiveSessionSlot(ctx, "new", fresh); refusal != nil {
		t.Fatalf("new claim %v", refusal)
	}
	if _, held := old.Lookup("active_session_lease"); recordLease(fresh) != lease || held {
		t.Fatal("lease did not move to the new runtime")
	}
	if !recordAgent(old).wasInterrupted() || old.Get("_turn_cancel_requested") != true {
		t.Fatal("old turn was not interrupted")
	}
	entries := registryEntries(t, "")
	if len(entries) != 1 || entries[0]["lease_id"] != lease.LeaseID {
		t.Fatalf("registry %v", entries)
	}
	if metadata, _ := entries[0]["metadata"].(map[string]any); metadata["live_session_id"] != "new" {
		t.Fatalf("metadata %v", entries[0]["metadata"])
	}
	fresh.Set("transport", tuigateway.DetachedWSTransport())
	if refusal := ensureActiveSessionSlot(ctx, "old", old); refusal != nil {
		t.Fatalf("old reclaim %v", refusal)
	}
	if _, marked := old.Lookup("_lease_taken_over"); recordLease(old) != lease || marked {
		t.Fatal("old runtime did not own the lease again")
	}
	if _, held := fresh.Lookup("active_session_lease"); fresh.Get("_lease_taken_over") != true || held {
		t.Fatal("new runtime was not marked taken over")
	}
	_ = lease.Release()
}

func TestNewRuntimeNeverTakesALiveForeignOrAttachedLease(t *testing.T) {
	ctx := context.Background()
	h, _ := ownershipHome(t)
	child := spawnLeaseHolder(t, h, "foreign-chat")
	foreign := runtimeRecord("foreign-chat", &orphanRacePeer{id: 11}, false)
	attached := runtimeRecord("two-windows", &orphanRacePeer{id: 12}, false)
	second := runtimeRecord("two-windows", &orphanRacePeer{id: 13}, false)
	swapSessions(t, map[string]*tuigateway.Session{"f": foreign, "w1": attached, "w2": second})
	if refusal := ensureActiveSessionSlot(ctx, "w1", attached); refusal != nil {
		t.Fatalf("first window claim %v", refusal)
	}
	for _, sid := range []string{"f", "w2"} {
		record := map[string]*tuigateway.Session{"f": foreign, "w2": second}[sid]
		refusal := ensureActiveSessionSlot(ctx, sid, record)
		if refusal == nil || refusal.Reason != activesessions.SessionNotOwned {
			t.Fatalf("%s claim %v", sid, refusal)
		}
		if recordLease(record) != nil {
			t.Fatalf("%s took a lease", sid)
		}
	}
	if recordLease(attached) == nil || recordAgent(attached).wasInterrupted() {
		t.Fatal("attached window lost its lease or turn")
	}
	_ = recordLease(attached).Release()
	child.finish(t)
}

func TestTakeoverStaysWithinTheProfile(t *testing.T) {
	ctx := context.Background()
	ownershipHome(t)
	homeA, homeB := filepath.Join(t.TempDir(), "A"), filepath.Join(t.TempDir(), "B")
	key := "20260916_175430_a9e77f"
	sibling := runtimeRecord(key, tuigateway.DetachedWSTransport(), true)
	sibling.Set("profile_home", homeA)
	fresh := runtimeRecord(key, &orphanRacePeer{id: 14}, false)
	fresh.Set("profile_home", homeB)
	swapSessions(t, map[string]*tuigateway.Session{"a": sibling, "b": fresh})
	if refusal := ensureActiveSessionSlot(ctx, "a", sibling); refusal != nil {
		t.Fatalf("profile A claim %v", refusal)
	}
	holderB, message, err := activesessions.TryAcquire(ctx, key, "desktop", map[string]any{}, map[string]any{"live_session_id": "other-window"}, homeB, true)
	if err != nil || holderB == nil || message != nil {
		t.Fatalf("profile B holder %v %v %v", holderB, message, err)
	}
	defer func() {
		_ = holderB.Release()
		_ = recordLease(sibling).Release()
	}()
	refusal := ensureActiveSessionSlot(ctx, "b", fresh)
	if refusal == nil || refusal.Reason != activesessions.SessionNotOwned {
		t.Fatalf("profile B claim %v", refusal)
	}
	if recordLease(fresh) != nil {
		t.Fatal("profile B took a lease")
	}
	if recordLease(sibling) == nil || recordAgent(sibling).wasInterrupted() {
		t.Fatal("profile A runtime lost its lease or turn")
	}
}

func TestTakenOverRuntimeFinalizeSparesTheNewOwnersRow(t *testing.T) {
	ctx := context.Background()
	h, _ := ownershipHome(t)
	key := "restored-tui-chat"
	old := runtimeRecord(key, tuigateway.DetachedWSTransport(), true)
	old.Update(map[string]any{"source": "tui", "slash_worker": nil})
	fresh := runtimeRecord(key, &orphanRacePeer{id: 15}, false)
	fresh.Set("source", "tui")
	swapSessions(t, map[string]*tuigateway.Session{"old": old, "new": fresh})
	if refusal := ensureActiveSessionSlot(ctx, "old", old); refusal != nil {
		t.Fatalf("old claim %v", refusal)
	}
	if refusal := ensureActiveSessionSlot(ctx, "new", fresh); refusal != nil {
		t.Fatalf("new claim %v", refusal)
	}
	lease := recordLease(fresh)
	defer func() { _ = lease.Release() }()
	dbPath := storedRow(t, h, key, "tui")
	var interruptMu sync.Mutex
	interrupted := map[string]int{}
	for id, selector := range map[string][2]string{"deleg_key": {"session_key", key}, "deleg_ui": {"origin_ui_session_id", "old"}} {
		t.Cleanup(tools.SetAsyncDelegationRecordForTests(id, map[string]any{"delegation_id": id, "status": "running", selector[0]: selector[1]}))
		tools.SetAsyncDelegationInterruptForTests(id, func() error {
			interruptMu.Lock()
			interrupted[id]++
			interruptMu.Unlock()
			return nil
		})
	}
	tools.QueueGatewayApprovalForTests(key, map[string]any{"command": "rm -rf build"})
	t.Cleanup(func() { tools.UnregisterGatewayNotify(key) })
	teardownSession(ctx, old, "ws_orphan_reap")
	assertRowOpen(t, dbPath, key)
	interruptMu.Lock()
	if interrupted["deleg_key"] != 0 || interrupted["deleg_ui"] != 1 {
		t.Fatalf("interrupts %v", interrupted)
	}
	interruptMu.Unlock()
	if tools.GetPendingGatewayApproval(key) == nil {
		t.Fatal("approval route of the new owner was unregistered")
	}
	if lease.Released() || recordLease(fresh) != lease {
		t.Fatal("new owner lost its lease")
	}
}
