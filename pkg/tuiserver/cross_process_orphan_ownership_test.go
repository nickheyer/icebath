package tuiserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

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

// Runtime record of a TUI chat on a transport
func runtimeRecord(sessionKey string, transport tuihost.Transport, running bool) *tuihost.Session {
	now := nowSeconds()
	return tuihost.NewSession(map[string]any{"agent": &takeoverAgent{}, "agent_ready": nil, "transport": transport,
		"running": running, "last_active": now, "created_at": now, "source": "tui", "session_key": sessionKey,
		"history": []any{}, "active_session_lease": nil, "profile_home": nil})
}

// Lease a record holds
func recordLease(session *tuihost.Session) *activesessions.Lease {
	lease, _ := session.Get("active_session_lease").(*activesessions.Lease)
	return lease
}

// Interrupt double of a record
func recordAgent(session *tuihost.Session) *takeoverAgent {
	return session.Get("agent").(*takeoverAgent)
}

// Makes the registry of a home unusable by putting a file where its runtime directory belongs
func breakRegistry(t *testing.T, h string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "runtime"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaimFailsClosedWhenRegistrySetupFails(t *testing.T) {
	h, _ := ownershipHome(t)
	breakRegistry(t, h)
	ctx := context.Background()
	tuiLease, tuiRefusal := tuihost.ClaimActiveSessionSlotRefusal(ctx, "tui-session", "tui-runtime", "tui", "")
	if tuiLease != nil || tuiRefusal == nil || tuiRefusal.Message != tuihost.SessionOwnershipUnavailable {
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
	session := tuihost.NewSession(map[string]any{"active_session_lease": lease})
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
