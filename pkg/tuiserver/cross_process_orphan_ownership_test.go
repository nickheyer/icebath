package tuiserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
