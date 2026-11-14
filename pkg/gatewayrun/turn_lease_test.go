package gatewayrun

import (
	"context"
	"errors"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"slices"
	"sync"
	"testing"
	"time"
)

// Seconds as an optional timeout
func secs(v float64) *float64 { return &v }

// Waits until sid has n pending acquires registered, the moment a waiter blocks on the lock
func waitPending(t *testing.T, r *SessionTurnLeaseRegistry, sid string, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		lease := r.leases[sid]
		got := lease != nil && lease.pendingAcquires == n
		r.mu.Unlock()
		if got {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %d pending acquires on %s", n, sid)
}

// Acquire outcome delivered by a goroutine
type acquireResult struct {
	token *TurnLeaseToken
	err   error
}

// Starts an acquire on its own goroutine
func acquireAsync(ctx context.Context, r *SessionTurnLeaseRegistry, sid, owner string, gen int) chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		token, err := r.Acquire(ctx, sid, owner, gen, secs(1))
		out <- acquireResult{token, err}
	}()
	return out
}

// Session ids the registry tracks
func leaseIDs(r *SessionTurnLeaseRegistry) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for sid := range r.leases {
		ids = append(ids, sid)
	}
	slices.Sort(ids)
	return ids
}

func TestAliasKeyTurnWaitsAndOrderIsPreserved(t *testing.T) {
	registry := NewSessionTurnLeaseRegistry(defaultMaxLeases)
	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	turn := func(owner string, hold time.Duration, wg *sync.WaitGroup) {
		defer wg.Done()
		token, err := registry.Acquire(context.Background(), "sess-1", owner, 1, secs(5))
		if err != nil || token == nil {
			t.Errorf("acquire %v %v", token, err)
			return
		}
		record("load:" + owner)
		time.Sleep(hold)
		record("flush:" + owner)
		registry.Release(token)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go turn("key-a", 50*time.Millisecond, &wg)
	time.Sleep(10 * time.Millisecond)
	go turn("key-b", 0, &wg)
	wg.Wait()
	want := []string{"load:key-a", "flush:key-a", "load:key-b", "flush:key-b"}
	if !slices.Equal(events, want) {
		t.Fatalf("events %v", events)
	}
}

func TestDistinctSessionsDoNotContend(t *testing.T) {
	registry := NewSessionTurnLeaseRegistry(defaultMaxLeases)
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, pair := range [][2]string{{"sess-a", "key-a"}, {"sess-b", "key-b"}} {
		wg.Add(1)
		go func(sid, owner string) {
			defer wg.Done()
			token, _ := registry.Acquire(context.Background(), sid, owner, 1, secs(5))
			mu.Lock()
			order = append(order, "start:"+sid)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			order = append(order, "end:"+sid)
			mu.Unlock()
			registry.Release(token)
		}(pair[0], pair[1])
	}
	wg.Wait()
	for _, e := range order[:2] {
		if e[:6] != "start:" {
			t.Fatalf("order %v", order)
		}
	}
}

func TestTimeoutFailsClosedInsteadOfAuthorizingAnUnserializedTurn(t *testing.T) {
	ctx := context.Background()
	registry := NewSessionTurnLeaseRegistry(defaultMaxLeases)
	holder, err := registry.Acquire(ctx, "sess-timeout", "key-a", 1, secs(1))
	if err != nil || holder == nil {
		t.Fatal(err)
	}
	_, err = registry.Acquire(ctx, "sess-timeout", "key-b", 1, secs(0.02))
	var timeout *TurnLeaseTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err %v", err)
	}
	if registry.leases["sess-timeout"].holder != holder || !registry.Release(holder) {
		t.Fatal("holder lost its lease")
	}
	successor, err := registry.Acquire(ctx, "sess-timeout", "key-b", 2, secs(1))
	if err != nil || successor == nil || !registry.Release(successor) {
		t.Fatal(err)
	}
}

func TestRegistryDoesNotEvictLeaseDuringWaiterHandoff(t *testing.T) {
	ctx := context.Background()
	registry := NewSessionTurnLeaseRegistry(1)
	first, _ := registry.Acquire(ctx, "shared", "first", 1, secs(1))
	lease := registry.leases["shared"]
	waking := acquireAsync(ctx, registry, "shared", "waking", 1)
	waitPending(t, registry, "shared", 1)
	if !registry.Release(first) {
		t.Fatal("release first")
	}
	other, err := registry.Acquire(ctx, "other", "other", 1, secs(1))
	if err != nil {
		t.Fatal(err)
	}
	woken := <-waking
	if woken.err != nil || registry.leases["shared"] != lease {
		t.Fatalf("shared lease replaced %v", woken.err)
	}
	successor := acquireAsync(ctx, registry, "shared", "successor", 2)
	waitPending(t, registry, "shared", 1)
	select {
	case <-successor:
		t.Fatal("successor ran beside the woken waiter")
	default:
	}
	if !registry.Release(woken.token) {
		t.Fatal("release waking")
	}
	next := <-successor
	if next.err != nil || !registry.Release(next.token) || !registry.Release(other) {
		t.Fatal("successor release")
	}
}

func TestRegistryDoesNotEvictAnUncontendedAcquireBeforeItLocks(t *testing.T) {
	ctx := context.Background()
	registry := NewSessionTurnLeaseRegistry(1)
	seed, _ := registry.Acquire(ctx, "shared", "seed", 1, secs(1))
	if !registry.Release(seed) {
		t.Fatal("release seed")
	}
	lease := registry.leases["shared"]
	registry.mu.Lock()
	lease.pendingAcquires++
	registry.mu.Unlock()
	other, err := registry.Acquire(ctx, "other", "other", 1, secs(1))
	if err != nil || registry.leases["shared"] != lease {
		t.Fatal("pending lease evicted")
	}
	registry.mu.Lock()
	lease.pendingAcquires--
	registry.mu.Unlock()
	pending, err := registry.Acquire(ctx, "shared", "pending", 2, secs(1))
	if err != nil || !registry.Release(pending) || !registry.Release(other) {
		t.Fatal("pending release")
	}
}

func TestTimedOutAcquireDoesNotPinIdleRegistryEntry(t *testing.T) {
	ctx := context.Background()
	registry := NewSessionTurnLeaseRegistry(1)
	holder, _ := registry.Acquire(ctx, "shared", "holder", 1, secs(1))
	var timeout *TurnLeaseTimeoutError
	if _, err := registry.Acquire(ctx, "shared", "timeout", 2, secs(0.02)); !errors.As(err, &timeout) {
		t.Fatalf("err %v", err)
	}
	if !registry.Release(holder) {
		t.Fatal("release holder")
	}
	other, _ := registry.Acquire(ctx, "other", "other", 1, secs(1))
	if ids := leaseIDs(registry); !slices.Equal(ids, []string{"other"}) {
		t.Fatalf("leases %v", ids)
	}
	if !registry.Release(other) {
		t.Fatal("release other")
	}
}

func TestCancelledAcquireDoesNotPinIdleRegistryEntry(t *testing.T) {
	registry := NewSessionTurnLeaseRegistry(1)
	holder, _ := registry.Acquire(context.Background(), "shared", "holder", 1, secs(1))
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := acquireAsync(ctx, registry, "shared", "cancelled", 2)
	waitPending(t, registry, "shared", 1)
	cancel()
	if result := <-cancelled; !errors.Is(result.err, context.Canceled) {
		t.Fatalf("err %v", result.err)
	}
	if !registry.Release(holder) {
		t.Fatal("release holder")
	}
	other, _ := registry.Acquire(context.Background(), "other", "other", 1, secs(1))
	if ids := leaseIDs(registry); !slices.Equal(ids, []string{"other"}) {
		t.Fatalf("leases %v", ids)
	}
	if !registry.Release(other) {
		t.Fatal("release other")
	}
}

func TestRebindMovesSerializationToNewSessionID(t *testing.T) {
	ctx := context.Background()
	registry := NewSessionTurnLeaseRegistry(defaultMaxLeases)
	token, _ := registry.Acquire(ctx, "parent", "key-a", 1, secs(5))
	if !registry.Rebind(token, "child") || token.sessionID != "child" {
		t.Fatal("rebind")
	}
	waiter := acquireAsync(ctx, registry, "child", "key-b", 1)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-waiter:
		t.Fatal("waiter ran beside the holder")
	default:
	}
	if !registry.Release(token) {
		t.Fatal("release")
	}
	next := <-waiter
	if next.err != nil || next.token == nil {
		t.Fatal(next.err)
	}
	registry.Release(next.token)
}

func TestRebindDoesNotReplaceTargetDuringWaiterHandoff(t *testing.T) {
	ctx := context.Background()
	registry := NewSessionTurnLeaseRegistry(defaultMaxLeases)
	targetHolder, _ := registry.Acquire(ctx, "target", "target-holder", 1, secs(1))
	targetLease := registry.leases["target"]
	targetWaiter := acquireAsync(ctx, registry, "target", "target-waiter", 2)
	waitPending(t, registry, "target", 1)
	sourceHolder, _ := registry.Acquire(ctx, "source", "source-holder", 1, secs(1))
	if !registry.Release(targetHolder) {
		t.Fatal("release target holder")
	}
	if registry.Rebind(sourceHolder, "target") {
		t.Fatal("rebind replaced a live target")
	}
	woken := <-targetWaiter
	if woken.err != nil || registry.leases["target"] != targetLease {
		t.Fatal("target lease replaced")
	}
	if !registry.Release(woken.token) || !registry.Release(sourceHolder) {
		t.Fatal("release")
	}
}

func TestDefaultTurnLeaseTimeoutMatchesTheRuntimeFallback(t *testing.T) {
	var agentDefaults map[string]any
	switch block := config.Defaults()["agent"].(type) {
	case config.Tree:
		agentDefaults = block
	case map[string]any:
		agentDefaults = block
	}
	value, err := pyint.Float(agentDefaults["gateway_turn_lease_timeout"])
	if err != nil || value != defaultLeaseWait {
		t.Fatalf("default %v %v, runtime %v", value, err, defaultLeaseWait)
	}
}

func TestRunnerReleaseTurnLeaseIsTokenScopedAndBareSafe(t *testing.T) {
	r := allocGatewayRunner()
	if r.releaseTurnLease("key-a", 1) {
		t.Fatal("bare runner released a lease")
	}
	r.turnLeases = NewSessionTurnLeaseRegistry(defaultMaxLeases)
	r.sessions = map[string]*SessionState{}
	token, err := r.turnLeases.Acquire(context.Background(), "sess-r", "key-a", 1, secs(5))
	if err != nil {
		t.Fatal(err)
	}
	r.turnLeaseTokens().Set(leaseTokenKey{sessionKey: "key-a", generation: 1}, token)
	if r.releaseTurnLease("key-a", 2) {
		t.Fatal("wrong generation released")
	}
	if r.turnLeases.leases["sess-r"].holder != token {
		t.Fatal("holder changed")
	}
	if !r.releaseTurnLease("key-a", 1) {
		t.Fatal("right generation did not release")
	}
	if r.releaseTurnLease("key-a", 1) {
		t.Fatal("release not idempotent")
	}
	if r.releaseTurnLease("", 1) {
		t.Fatal("empty key released")
	}
}
