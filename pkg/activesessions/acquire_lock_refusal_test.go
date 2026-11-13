package activesessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestTryAcquireLocksRegistryAndRefusalsSurviveCopy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	ctx := context.Background()

	inert, refusal, err := TryAcquire(ctx, "", "cli", map[string]any{}, nil, "", false)
	if err != nil || refusal != nil || inert == nil || inert.Enabled || inert.StatePath != "" {
		t.Fatalf("empty session id gave lease %+v refusal %v err %v", inert, refusal, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "runtime")); !os.IsNotExist(err) {
		t.Fatalf("empty session id touched the registry: %v", err)
	}

	lease, refusal, err := TryAcquire(ctx, "s1", "cli", map[string]any{}, map[string]any{"live_session_id": "a"}, "", false)
	if err != nil || refusal != nil || lease == nil || !lease.Enabled {
		t.Fatalf("first claim gave lease %+v refusal %v err %v", lease, refusal, err)
	}
	if want := filepath.Join(tmp, "runtime", "active_sessions.lock"); lease.LockPath != want {
		t.Fatalf("lock path %q, want %q", lease.LockPath, want)
	}

	unlock, err := FileLock(lease.LockPath, NoLockTimeout)
	if err != nil {
		t.Fatal(err)
	}
	_, err = FileLock(lease.LockPath, 100*time.Millisecond)
	var unavailable *LockUnavailableError
	if !errors.As(err, &unavailable) || err.Error() != "file lock unavailable: "+lease.LockPath {
		t.Fatalf("contended lock gave %v", err)
	}
	if cause := errors.Unwrap(err); cause == nil || cause.Error() != "file lock still held after 0.1s" {
		t.Fatalf("contended lock cause %v", cause)
	}
	claimed := make(chan *Refusal, 1)
	go func() {
		_, r, _ := TryAcquire(ctx, "s1", "desktop", map[string]any{}, map[string]any{"live_session_id": "b"}, "", false)
		claimed <- r
	}()
	select {
	case r := <-claimed:
		t.Fatalf("claim finished while the registry lock was held: %v", r)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	unlock()
	var owned *Refusal
	select {
	case owned = <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("claim never finished after the lock was released")
	}
	if owned == nil || owned.Reason != SessionNotOwned || !strings.HasPrefix(owned.Message, "This chat is open in another Icebath window/terminal.") {
		t.Fatalf("second owner refusal %+v", owned)
	}

	same, refusal, err := TryAcquire(ctx, "s1", "cli", map[string]any{}, map[string]any{"live_session_id": "a"}, "", false)
	if err != nil || refusal != nil || same == nil {
		t.Fatalf("same writer reclaim gave lease %+v refusal %v err %v", same, refusal, err)
	}

	_, full, err := TryAcquire(ctx, "s2", "desktop", map[string]any{"max_concurrent_sessions": 1}, nil, "", false)
	if err != nil || full == nil || full.Reason != MaxConcurrentSessions ||
		full.Message != "Icebath is at the active session limit (1/1). Held by: cli, oldest 0m ago. Try again when another session finishes." {
		t.Fatalf("capacity refusal %+v err %v", full, err)
	}

	full.LedgerStatus = "pending"
	payload := map[string]any{"text": full, "status": "refused"}
	copied, ok := pyjson.Clone(payload).(map[string]any)["text"].(*Refusal)
	if !ok || copied == full || *copied != *full {
		t.Fatalf("copied refusal %+v of %+v", copied, full)
	}
	if text, err := pyjson.Dumps(pyjson.Clone(payload)); err != nil || !strings.Contains(text, `"text": "Icebath is at the active session limit (1/1).`) {
		t.Fatalf("copied payload encodes as %s err %v", text, err)
	}

	if err := same.Release(); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadEntries(lease.StatePath, true)
	if err != nil || len(entries) != 0 {
		t.Fatalf("registry after release %v err %v", entries, err)
	}

	if err := os.WriteFile(lease.StatePath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, unreadable, err := TryAcquire(ctx, "s3", "cli", map[string]any{}, nil, "", false)
	if err != nil || unreadable == nil || unreadable.Reason != SessionCoordinationUnavailable ||
		!strings.HasPrefix(unreadable.Message, "Icebath could not read the active-session registry at "+lease.StatePath) {
		t.Fatalf("unreadable registry refusal %+v err %v", unreadable, err)
	}
	if text, _ := os.ReadFile(lease.StatePath); string(text) != "{not json" {
		t.Fatalf("unreadable registry was rewritten to %q", text)
	}
}
