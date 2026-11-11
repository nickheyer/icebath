package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Liveness tracked desktop lease of a stored session in a temp registry
func deferredTestLease(t *testing.T) *activesessions.Lease {
	t.Helper()
	lease, refusal, err := activesessions.TryAcquire(context.Background(), "stored-session", "desktop", map[string]any{}, nil, t.TempDir(), true)
	if err != nil || refusal != nil || lease == nil {
		t.Fatalf("acquire %v %v %v", lease, refusal, err)
	}
	return lease
}

// Parks a lease as deferred with the given age and drops it from the table after the case
func deferLease(t *testing.T, lease *activesessions.Lease, ageSeconds float64) {
	t.Helper()
	deferredActiveSessionLeases.Lock()
	deferredActiveSessionLeases.leases[lease.LeaseID] = lease
	deferredActiveSessionLeases.ages[lease.LeaseID] = nowSeconds() - ageSeconds
	deferredActiveSessionLeases.Unlock()
	t.Cleanup(func() {
		deferredActiveSessionLeases.Lock()
		delete(deferredActiveSessionLeases.leases, lease.LeaseID)
		delete(deferredActiveSessionLeases.ages, lease.LeaseID)
		deferredActiveSessionLeases.Unlock()
		_ = lease.Release()
	})
}

func deferredHolds(leaseID string) (bool, bool) {
	deferredActiveSessionLeases.Lock()
	defer deferredActiveSessionLeases.Unlock()
	_, leased := deferredActiveSessionLeases.leases[leaseID]
	_, aged := deferredActiveSessionLeases.ages[leaseID]
	return leased, aged
}

func TestStaleDeferredLeaseIsForceReleased(t *testing.T) {
	lease := deferredTestLease(t)
	deferLease(t, lease, deferredActiveSessionLeaseTTLSeconds+60)
	if reaped := reapStaleDeferredLeases(nowSeconds()); reaped != 1 {
		t.Fatalf("reaped %d", reaped)
	}
	leased, aged := deferredHolds(lease.LeaseID)
	if !lease.Released() || leased || aged {
		t.Fatal("stale lease kept")
	}
}

func TestFreshDeferredLeaseIsKept(t *testing.T) {
	lease := deferredTestLease(t)
	deferLease(t, lease, 10)
	if reaped := reapStaleDeferredLeases(nowSeconds()); reaped != 0 {
		t.Fatalf("reaped %d", reaped)
	}
	if leased, _ := deferredHolds(lease.LeaseID); lease.Released() || !leased {
		t.Fatal("fresh lease released")
	}
	releaseDeferredActiveSessionLease(tuigateway.NewSession(map[string]any{"_deferred_active_session_lease": lease}))
	if !lease.Released() {
		t.Fatal("settlement did not release")
	}
}

func TestSettlementClearsTheAgeEntry(t *testing.T) {
	lease := deferredTestLease(t)
	deferLease(t, lease, 0)
	releaseDeferredActiveSessionLease(tuigateway.NewSession(map[string]any{"_deferred_active_session_lease": lease}))
	leased, aged := deferredHolds(lease.LeaseID)
	if !lease.Released() || leased || aged {
		t.Fatal("settlement left the lease deferred")
	}
}

func TestReapedLeaseNoLongerVouchedByOwnLiveLeaseIds(t *testing.T) {
	lease := deferredTestLease(t)
	deferLease(t, lease, deferredActiveSessionLeaseTTLSeconds+60)
	if !ownLiveLeaseIDs(nil)[lease.LeaseID] {
		t.Fatal("deferred lease not vouched")
	}
	reapStaleDeferredLeases(nowSeconds())
	if ownLiveLeaseIDs(nil)[lease.LeaseID] {
		t.Fatal("reaped lease still vouched")
	}
}
