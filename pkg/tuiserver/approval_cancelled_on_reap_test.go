package tuiserver

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	loudDenySessionKey = "20260909_120000_aaaaaa"
	loudDenySID        = "live-loud-deny"
	loudDenyRequestID  = "req-loud-deny-1"
)

// Session of the loud deny cases, its stored row gateway owned so the finalize leaves the approval queue to teardown
func loudDenySession(t *testing.T) *tuigateway.Session {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	profile := t.TempDir()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(profile, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: loudDenySessionKey, Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return tuigateway.NewSession(map[string]any{"_sid": loudDenySID, "session_key": loudDenySessionKey, "history": []any{},
		"running": false, "queued_prompt": nil, "agent": nil, "profile_home": profile})
}

// Pending gateway approval queued for the loud deny session, dropped again when the case ends
func loudDenyPending(t *testing.T) *tools.ApprovalEntry {
	t.Helper()
	entry := tools.QueueGatewayApprovalForTests(loudDenySessionKey, map[string]any{"command": "rm -rf /tmp/x", "request_id": loudDenyRequestID})
	t.Cleanup(func() { tools.UnregisterGatewayNotify(loudDenySessionKey) })
	return entry
}

// The approval.cancelled broadcasts among the captured events
func cancelledEvents(events *[][2]any) []map[string]any {
	out := []map[string]any{}
	for _, ev := range *events {
		if ev[0] == "approval.cancelled" {
			out = append(out, ev[1].(map[string]any))
		}
	}
	return out
}

// Whether an entry's waiters were woken
func entryWoken(entry *tools.ApprovalEntry) bool {
	select {
	case <-entry.Done():
		return true
	default:
		return false
	}
}

// Asserts one cancelled broadcast naming the session, the reason and the single pending request
func assertCancelledPayload(t *testing.T, cancelled []map[string]any, reason string) {
	t.Helper()
	if len(cancelled) == 0 {
		t.Fatal("approval.cancelled must be broadcast when a pending prompt is dropped")
	}
	payload := cancelled[0]
	ids, _ := payload["request_ids"].([]any)
	if payload["session_id"] != loudDenySID || payload["stored_session_id"] != loudDenySessionKey || payload["reason"] != reason ||
		payload["cancelled_count"] != 1 || !slices.Contains(ids, any(loudDenyRequestID)) {
		t.Fatalf("payload %v", payload)
	}
}

func TestInterruptWithPendingBroadcastsApprovalCancelled(t *testing.T) {
	session := loudDenySession(t)
	events := captureBroadcasts(t)
	entry := loudDenyPending(t)
	interruptSessionTurn(context.Background(), loudDenySID, session, "client-gone-"+loudDenySID, true)
	assertCancelledPayload(t, cancelledEvents(events), "ws_orphan_reap")
	if entry.Result != "deny" || !entryWoken(entry) || len(tools.ListGatewayApprovals(loudDenySessionKey)) != 0 {
		t.Fatalf("entry result %q woken %v queue %v", entry.Result, entryWoken(entry), tools.ListGatewayApprovals(loudDenySessionKey))
	}
}

func TestInterruptEmptyQueueEmitsNothing(t *testing.T) {
	session := loudDenySession(t)
	events := captureBroadcasts(t)
	tools.UnregisterGatewayNotify(loudDenySessionKey)
	interruptSessionTurn(context.Background(), loudDenySID, session, "", false)
	if got := cancelledEvents(events); len(got) != 0 {
		t.Fatalf("cancelled %v", got)
	}
}

func TestExplicitUserDenyDoesNotEmitCancelled(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	events := captureBroadcasts(t)
	entry := loudDenyPending(t)
	if count := tools.ResolveGatewayApproval(loudDenySessionKey, "deny", false, "", ""); count != 1 {
		t.Fatalf("resolved %d", count)
	}
	if entry.Result != "deny" || len(cancelledEvents(events)) != 0 {
		t.Fatalf("entry result %q cancelled %v", entry.Result, cancelledEvents(events))
	}
}

func TestBroadcastFailureStillDenyResolves(t *testing.T) {
	session := loudDenySession(t)
	prev := broadcastGlobalEvent
	broadcastGlobalEvent = func(string, map[string]any) { panic("transport gone") }
	t.Cleanup(func() { broadcastGlobalEvent = prev })
	entry := loudDenyPending(t)
	interruptSessionTurn(context.Background(), loudDenySID, session, "client-gone-"+loudDenySID, true)
	if entry.Result != "deny" || !entryWoken(entry) {
		t.Fatalf("entry result %q woken %v", entry.Result, entryWoken(entry))
	}
}

func TestUserInterruptReasonIsInterrupt(t *testing.T) {
	session := loudDenySession(t)
	events := captureBroadcasts(t)
	entry := loudDenyPending(t)
	interruptSessionTurn(context.Background(), loudDenySID, session, "client-gone-"+loudDenySID, false)
	cancelled := cancelledEvents(events)
	if len(cancelled) == 0 || cancelled[0]["reason"] != "interrupt" || entry.Result != "deny" {
		t.Fatalf("cancelled %v entry result %q", cancelled, entry.Result)
	}
}

func TestOrphanKwargLabelsWsOrphanReapWithoutClientGonePrefix(t *testing.T) {
	session := loudDenySession(t)
	events := captureBroadcasts(t)
	entry := loudDenyPending(t)
	interruptSessionTurn(context.Background(), loudDenySID, session, "interrupt-manual", true)
	cancelled := cancelledEvents(events)
	if len(cancelled) == 0 || cancelled[0]["reason"] != "ws_orphan_reap" || entry.Result != "deny" {
		t.Fatalf("cancelled %v entry result %q", cancelled, entry.Result)
	}
}

func TestTeardownWithPendingBroadcastsApprovalCancelled(t *testing.T) {
	session := loudDenySession(t)
	events := captureBroadcasts(t)
	loudDenyPending(t)
	teardownSession(context.Background(), session, "ws_orphan_reap")
	assertCancelledPayload(t, cancelledEvents(events), "ws_orphan_reap")
	if queue := tools.ListGatewayApprovals(loudDenySessionKey); len(queue) != 0 {
		t.Fatalf("queue %v", queue)
	}
}

func TestTeardownWithoutPendingStaysSilent(t *testing.T) {
	session := loudDenySession(t)
	events := captureBroadcasts(t)
	teardownSession(context.Background(), session, "ws_orphan_reap")
	if got := cancelledEvents(events); len(got) != 0 {
		t.Fatalf("cancelled %v", got)
	}
}

func TestInterruptMissingSessionKeyDoesNotCrash(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	events := captureBroadcasts(t)
	interruptSessionTurn(context.Background(), loudDenySID, tuigateway.NewSession(map[string]any{"running": false}), "", false)
	if got := cancelledEvents(events); len(got) != 0 {
		t.Fatalf("cancelled %v", got)
	}
}
