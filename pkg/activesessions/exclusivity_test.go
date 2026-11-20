package activesessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

var ownerSeq atomic.Int64

// Isolated registry home for one exclusivity case
func isolatedRegistry(t *testing.T) {
	t.Helper()
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), ".icebath"))
}

// Acquires as a distinct owner unless a live id is given
func acquire(t *testing.T, sessionID string, cfg map[string]any, liveID string) (*Lease, *Refusal) {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	if liveID == "" {
		liveID = fmt.Sprintf("live-%d", ownerSeq.Add(1)-1)
	}
	lease, refusal, err := TryAcquire(context.Background(), sessionID, "tui", cfg, map[string]any{"live_session_id": liveID}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return lease, refusal
}

func TestNoCapConfiguredStillFencesOneSession(t *testing.T) {
	isolatedRegistry(t)
	leaseA, refused := acquire(t, "S", nil, "")
	if leaseA == nil || refused != nil {
		t.Fatalf("first owner refused %v", refused)
	}
	leaseB, refusal := acquire(t, "S", nil, "")
	if leaseB != nil {
		t.Fatal("a second live owner of one session must be refused")
	}
	if refusal.Reason != SessionNotOwned || !strings.Contains(refusal.String(), "S") {
		t.Fatalf("refusal %+v", refusal)
	}
	if got := mustSnapshot(t, ""); len(got) != 1 {
		t.Fatalf("holders %d", len(got))
	}
}

func TestDifferentSessionsStillRunConcurrently(t *testing.T) {
	isolatedRegistry(t)
	lease1, refused1 := acquire(t, "S1", nil, "")
	lease2, refused2 := acquire(t, "S2", nil, "")
	if lease1 == nil || refused1 != nil || lease2 == nil || refused2 != nil {
		t.Fatalf("refused %v %v", refused1, refused2)
	}
	if got := mustSnapshot(t, ""); len(got) != 2 {
		t.Fatalf("holders %d", len(got))
	}
}

func TestGlobalCapacityStillAppliesIndependently(t *testing.T) {
	isolatedRegistry(t)
	cfg := map[string]any{"max_concurrent_sessions": 2}
	if l, _ := acquire(t, "S1", cfg, ""); l == nil {
		t.Fatal("S1 refused")
	}
	if l, _ := acquire(t, "S2", cfg, ""); l == nil {
		t.Fatal("S2 refused")
	}
	lease, refusal := acquire(t, "S3", cfg, "")
	if lease != nil || refusal.Reason != MaxConcurrentSessions {
		t.Fatalf("a capacity refusal must not be reported as an ownership refusal: %+v", refusal)
	}
}

func TestCapacityAndExclusivityAreNotTheSameSwitch(t *testing.T) {
	isolatedRegistry(t)
	cfg := map[string]any{"max_concurrent_sessions": 4}
	if l, _ := acquire(t, "S", cfg, ""); l == nil {
		t.Fatal("S refused")
	}
	lease, refusal := acquire(t, "S", cfg, "")
	if lease != nil || refusal.Reason != SessionNotOwned {
		t.Fatalf("with capacity to spare, the refusal can only be about ownership: %+v", refusal)
	}
}

func TestADeadOwnerIsPrunedAndASuccessorMayAcquire(t *testing.T) {
	isolatedRegistry(t)
	if l, _ := acquire(t, "S", nil, ""); l == nil {
		t.Fatal("S refused")
	}
	entries, err := readEntries(statePathOf(""), false)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %d %v", len(entries), err)
	}
	entries[0].Set("pid", int64(0x7FFFFFFE))
	entries[0].Set("process_start_time", 1.0)
	if err := writeEntries(statePathOf(""), entries); err != nil {
		t.Fatal(err)
	}
	successor, refusal := acquire(t, "S", nil, "")
	if successor == nil {
		t.Fatalf("a dead owner must not block a successor: %v", refusal)
	}
	if got := mustSnapshot(t, ""); len(got) != 1 {
		t.Fatalf("holders %d", len(got))
	}
}

func TestARecycledPidDoesNotKeepALeaseAlive(t *testing.T) {
	isolatedRegistry(t)
	if l, _ := acquire(t, "S", nil, ""); l == nil {
		t.Fatal("S refused")
	}
	entries, err := readEntries(statePathOf(""), false)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].Set("pid", int64(os.Getpid()))
	entries[0].Set("process_start_time", 1.0)
	if err := writeEntries(statePathOf(""), entries); err != nil {
		t.Fatal(err)
	}
	if successor, refusal := acquire(t, "S", nil, ""); successor == nil {
		t.Fatalf("a recycled pid must not hold a session: %v", refusal)
	}
}

func TestReleaseLetsTheNextOwnerIn(t *testing.T) {
	isolatedRegistry(t)
	leaseA, _ := acquire(t, "S", nil, "")
	if leaseA == nil {
		t.Fatal("S refused")
	}
	if l, _ := acquire(t, "S", nil, ""); l != nil {
		t.Fatal("second owner admitted")
	}
	if err := ReleaseActiveSession(leaseA); err != nil {
		t.Fatal(err)
	}
	if got := mustSnapshot(t, ""); len(got) != 0 {
		t.Fatalf("holders %d", len(got))
	}
	if leaseB, refusal := acquire(t, "S", nil, ""); leaseB == nil {
		t.Fatalf("after release the session must be acquirable: %v", refusal)
	}
}

func TestReleaseIsIdempotentAndOnlyDropsItsOwnLease(t *testing.T) {
	isolatedRegistry(t)
	leaseA, _ := acquire(t, "S1", nil, "")
	leaseB, _ := acquire(t, "S2", nil, "")
	for range 2 {
		if err := ReleaseActiveSession(leaseA); err != nil {
			t.Fatal(err)
		}
	}
	held := sessionIDs(mustSnapshot(t, ""))
	if len(held) != 1 || held[0] != "S2" {
		t.Fatalf("held %v", held)
	}
	if leaseB == nil {
		t.Fatal("S2 refused")
	}
}

func TestASessionWithNoStoredIdIsExempt(t *testing.T) {
	isolatedRegistry(t)
	if l, _ := acquire(t, "", nil, ""); l == nil {
		t.Fatal("first draft refused")
	}
	if l, _ := acquire(t, "", nil, ""); l == nil {
		t.Fatal("empty ids do not collide with each other")
	}
}

func TestTheSameLiveSessionMayReAcquireItsOwnLease(t *testing.T) {
	isolatedRegistry(t)
	if l, _ := acquire(t, "S", nil, "tab-1"); l == nil {
		t.Fatal("S refused")
	}
	if again, refusal := acquire(t, "S", nil, "tab-1"); again == nil {
		t.Fatalf("a writer must not be fenced out by its own leak: %v", refusal)
	}
	if got := mustSnapshot(t, ""); len(got) != 1 {
		t.Fatalf("and it must not double-book: %d", len(got))
	}
	other, refusal := acquire(t, "S", nil, "tab-2")
	if other != nil || refusal.Reason != SessionNotOwned {
		t.Fatalf("other %v refusal %+v", other, refusal)
	}
}
