package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner over a real store whose telegram group route points at routeTo, rows seeded before and after the switch
func pinningRunner(t *testing.T, routeTo, routeParent string, before, after func(db *state.SessionDB)) (*GatewayRunner, *gateway.SessionStore, *gateway.SessionEntry) {
	t.Helper()
	ctx := context.Background()
	store := sessionDBStore(t, sessionDBHome(t))
	r := runnerWithStore(store)
	db := storeHandle(t, store)
	thread := "4"
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "-100", ChatType: "group", ThreadID: &thread}
	entry, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		t.Fatal(err)
	}
	before(db)
	if err := db.CreateSession(state.NewSession{ID: routeTo, Source: "telegram", ParentSessionID: routeParent}); err != nil {
		t.Fatal(err)
	}
	current, err := store.SwitchSession(ctx, entry.SessionKey, routeTo, nil, true)
	if err != nil || current == nil {
		t.Fatalf("route setup %v %v", current, err)
	}
	after(db)
	return r, store, current
}

// Seeds nothing
func noRows(*state.SessionDB) {}

// Seeds one session row, ended with reason when it is not empty
func seedRow(t *testing.T, db *state.SessionDB, id, parent, endReason string) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: id, Source: "telegram", ParentSessionID: parent}); err != nil {
		t.Fatal(err)
	}
	if endReason != "" {
		if err := db.EndSession(id, endReason); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayPinningFailsClosedLiveSpawningSessionRebindsFromDifferentRoute(t *testing.T) {
	ctx := context.Background()
	r, store, current := pinningRunner(t, "sess_current", "", noRows, func(db *state.SessionDB) {
		seedRow(t, db, "sess_live", "", "")
	})
	resolved, err := r.resolveAsyncDelegationSession(ctx, current, "sess_live", "", nil)
	if err != nil || resolved == nil || resolved.SessionID != "sess_live" {
		t.Fatalf("resolved %v %v", resolved, err)
	}
	if got := store.LookupBySessionKey(ctx, current.SessionKey); got == nil || got.SessionID != "sess_live" {
		t.Fatalf("route %v", got)
	}
}

func TestGatewayPinningFailsClosedNonCompressionEndedParentDrops(t *testing.T) {
	ctx := context.Background()
	r, store, current := pinningRunner(t, "sess_route", "", noRows, func(db *state.SessionDB) {
		seedRow(t, db, "sess_old", "", "session_reset")
	})
	resolved, err := r.resolveAsyncDelegationSession(ctx, current, "sess_old", "", nil)
	if err != nil || resolved != nil {
		t.Fatalf("resolved %v %v", resolved, err)
	}
	if got := store.LookupBySessionKey(ctx, current.SessionKey); got == nil || got.SessionID != "sess_route" {
		t.Fatalf("route changed %v", got)
	}
}

func TestGatewayPinningFailsClosedIntermediateCompressionRouteAdvancesToSameLiveTip(t *testing.T) {
	ctx := context.Background()
	r, store, current := pinningRunner(t, "sess_middle", "sess_parent", func(db *state.SessionDB) {
		seedRow(t, db, "sess_parent", "", "compression")
	}, func(db *state.SessionDB) {
		if err := db.EndSession("sess_middle", "compression"); err != nil {
			t.Fatal(err)
		}
		seedRow(t, db, "sess_tip", "sess_middle", "")
	})
	resolved, err := r.resolveAsyncDelegationSession(ctx, current, "sess_parent", "", nil)
	if err != nil || resolved == nil || resolved.SessionID != "sess_tip" {
		t.Fatalf("resolved %v %v", resolved, err)
	}
	if got := store.LookupBySessionKey(ctx, current.SessionKey); got == nil || got.SessionID != "sess_tip" {
		t.Fatalf("route %v", got)
	}
}

func TestGatewayPinningFailsClosedCompressionParentFollowsRealSessiondbLineage(t *testing.T) {
	ctx := context.Background()
	r, store, current := pinningRunner(t, "sess_parent", "", noRows, func(db *state.SessionDB) {
		if err := db.EndSession("sess_parent", "compression"); err != nil {
			t.Fatal(err)
		}
		seedRow(t, db, "sess_tip", "sess_parent", "")
	})
	resolved, err := r.resolveAsyncDelegationSession(ctx, current, "sess_parent", "", nil)
	if err != nil || resolved == nil || resolved.SessionID != "sess_tip" {
		t.Fatalf("resolved %v %v", resolved, err)
	}
	if got := store.LookupBySessionKey(ctx, current.SessionKey); got == nil || got.SessionID != "sess_tip" {
		t.Fatalf("route %v", got)
	}
}
