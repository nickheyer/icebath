package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Claims the lease of a key as the parent dashboard does, same pid and its own live id
func seedParentLease(t *testing.T, key string) *activesessions.Lease {
	t.Helper()
	lease, refusal, err := activesessions.TryAcquire(context.Background(), key, "desktop", map[string]any{},
		map[string]any{"live_session_id": "parent-sid"}, "", true)
	if err != nil || refusal != nil || lease == nil {
		t.Fatalf("seed %v %v", refusal, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}

// A distinct writer of the key, the same pid with another live id
func foreignAcquire(key string) (*activesessions.Lease, *activesessions.Refusal) {
	lease, refusal, _ := activesessions.TryAcquire(context.Background(), key, "cli", map[string]any{},
		map[string]any{"live_session_id": "other-writer"}, "", false)
	return lease, refusal
}

// Parent record of an isolated turn holding the real lease
func borrowedParentSession(sid, key string, lease *activesessions.Lease) *tuigateway.Session {
	now := nowSeconds()
	return tuigateway.NewSession(map[string]any{"agent": nil, "agent_ready": &pythread.Event{}, "session_key": key, "history": []any{},
		"history_version": int64(0), "history_lock": &sync.Mutex{}, "running": true, "transport": tuigateway.DetachedWSTransport(),
		"attached_images": []any{}, "cols": 80, "source": "desktop", "inflight_turn": nil, "created_at": now, "last_active": now,
		"active_session_lease": lease, "_compute_host_active": true, "_sid": sid})
}

// Registry entry fields of the case home
func registryFields(t *testing.T, fields ...string) [][]any {
	t.Helper()
	var out [][]any
	for _, entry := range registryEntries(t, os.Getenv("ICEBATH_HOME")) {
		row := []any{}
		for _, field := range fields {
			row = append(row, entry[field])
		}
		out = append(out, row)
	}
	return out
}

func TestChildRotationNeverClaimsAndParentReanchorsItsRealLease(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	parent := seedParentLease(t, "A")
	child := tuigateway.NewSession(map[string]any{"session_key": "A", "history_lock": &sync.Mutex{}})
	installBorrowedLease("sid", child, map[string]any{"type": "turn.start", "sid": "sid", "request_id": "turn", "text": "hello", "session_key": "A",
		"source": "desktop", "cols": 80, "history": []any{}, "active_session_lease": map[string]any{"lease_id": parent.LeaseID, "session_id": "A"}})
	if !transferActiveSessionSlot(ctx, "sid", child, "B") {
		t.Fatal("borrowed transfer refused")
	}
	if borrowed, _ := child.Get("active_session_lease").(*activesessions.Lease); borrowed == nil || borrowed.SessionID() != "B" {
		t.Fatalf("borrowed %v", child.Get("active_session_lease"))
	}
	if got := registryFields(t, "session_id", "pid"); !reflect.DeepEqual(got, [][]any{{"A", int64(os.Getpid())}}) {
		t.Fatalf("registry %v", got)
	}
	session := borrowedParentSession("sid", "A", parent)
	session.Set("session_key", "B")
	if vouch := activeSessionLeaseVouch(session); vouch != nil {
		t.Fatalf("stale lease vouched %v", vouch)
	}
	session.Set("session_key", "A")
	unlock := lockHistory(session)
	computeHostAdoptFrameMeta(ctx, session, map[string]any{"sid": "sid", "session_key": "B"})
	unlock()
	if session.Get("session_key") != "B" || parent.SessionID() != "B" {
		t.Fatalf("key %v lease %s", session.Get("session_key"), parent.SessionID())
	}
	if got := registryFields(t, "session_id", "lease_id"); !reflect.DeepEqual(got, [][]any{{"B", parent.LeaseID}}) {
		t.Fatalf("registry %v", got)
	}
	if vouch := activeSessionLeaseVouch(session); !reflect.DeepEqual(vouch, map[string]any{"lease_id": parent.LeaseID, "session_id": "B"}) {
		t.Fatalf("vouch %v", vouch)
	}
}

func TestCloseHoldsLeaseUntilIsolatedTurnSettles(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("dashboard:\n  turn_isolation: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sup, sent := testSupervisor(t)
	swapVar(t, &computeHostSupervisor, sup)
	swapVar(t, &turnSettleBeforeCloseSeconds, 0.2)
	ctx := context.Background()
	parent := seedParentLease(t, "A")
	session := borrowedParentSession("sid", "A", parent)
	session.Set("_compute_host_turn_id", "turn-1")
	session.Set("_closing", true)
	if !teardownPoppedSession(ctx, session, "tui_close") {
		t.Fatal("teardown reported nothing torn down")
	}
	var interrupts []any
	for _, frame := range *sent {
		if frame.GetOr("type", nil) == "interrupt" {
			interrupts = append(interrupts, frame.GetOr("sid", nil))
		}
	}
	if !reflect.DeepEqual(interrupts, []any{"sid"}) {
		t.Fatalf("interrupts %v", interrupts)
	}
	if got := registryFields(t, "lease_id"); !reflect.DeepEqual(got, [][]any{{parent.LeaseID}}) || !ownLiveLeaseIDs(nil)[parent.LeaseID] {
		t.Fatalf("registry %v own %v", got, ownLiveLeaseIDs(nil))
	}
	if lease, refusal := foreignAcquire("A"); lease != nil || refusal == nil || refusal.Reason != "SESSION_NOT_OWNED" {
		t.Fatalf("foreign writer %v %v", lease, refusal)
	}
	onComputeHostTurnDone(ctx, "rid", "sid", session, map[string]any{"type": "turn.end", "sid": "sid", "session_key": "A"})
	if got := registryFields(t, "lease_id"); len(got) != 0 || ownLiveLeaseIDs(nil)[parent.LeaseID] {
		t.Fatalf("registry %v after settle", got)
	}
	lease, refusal := foreignAcquire("A")
	if refusal != nil || lease == nil {
		t.Fatalf("foreign writer refused %v", refusal)
	}
	_ = lease.Release()
}
