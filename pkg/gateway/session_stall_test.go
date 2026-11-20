package gateway_test

import (
	"math"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Pointer to a float literal
func stallF(v float64) *float64 { return &v }

// test_session_stall_watchdog.py test_should_emit_requires_pending_and_idle
func TestShouldEmitRequiresPendingAndIdle(t *testing.T) {
	if !gateway.ShouldEmitSessionStallNotification(300, stallF(400), true, false) {
		t.Fatal("idle past the timeout with pending inbound must emit")
	}
	for _, c := range []struct {
		timeout, idle    float64
		pending, already bool
	}{{300, 400, false, false}, {300, 100, true, false}, {0, 9999, true, false}, {300, 400, true, true}} {
		if gateway.ShouldEmitSessionStallNotification(c.timeout, stallF(c.idle), c.pending, c.already) {
			t.Fatalf("must not emit for %+v", c)
		}
	}
}

// test_session_stall_watchdog.py test_should_clear_when_pending_gone_or_activity_resumes
func TestShouldClearWhenPendingGoneOrActivityResumes(t *testing.T) {
	if !gateway.ShouldClearSessionStallNotification(300, stallF(400), false) || !gateway.ShouldClearSessionStallNotification(300, stallF(10), true) {
		t.Fatal("latch must clear when pending drained or activity resumed")
	}
	if gateway.ShouldClearSessionStallNotification(300, stallF(400), true) {
		t.Fatal("latch must hold while still stalled")
	}
}

// test_session_stall_watchdog.py test_should_clear_holds_latch_when_idle_unknown
func TestShouldClearHoldsLatchWhenIdleUnknown(t *testing.T) {
	if gateway.ShouldClearSessionStallNotification(300, nil, true) {
		t.Fatal("unknown progress must hold the latch")
	}
}

// test_session_stall_watchdog.py test_resolve_idle_uses_shared_activity_snapshot_only
func TestResolveIdleUsesSharedActivitySnapshotOnly(t *testing.T) {
	now := 1_000_000.0
	at := now - 120
	snap := agent.BuildActivitySnapshot(&at, "tool: terminal", string(state.ActivityProvenanceUnknown), &now, nil)
	if got := gateway.ResolveSessionIdleSecondsFromActivity(snap.Vals, &now); got == nil || *got != 120.0 {
		t.Fatalf("idle = %v, want 120", got)
	}
	if gateway.ResolveSessionIdleSecondsFromActivity(nil, &now) != nil || gateway.ResolveSessionIdleSecondsFromActivity(map[string]any{}, &now) != nil {
		t.Fatal("an empty summary has no idle time")
	}
}

// test_session_stall_watchdog.py test_resolve_idle_prefers_seconds_since_activity_field
func TestResolveIdlePrefersSecondsSinceActivityField(t *testing.T) {
	now := 999.0
	if got := gateway.ResolveSessionIdleSecondsFromActivity(map[string]any{"seconds_since_activity": 42.5, "last_activity_at": 1.0}, &now); got == nil || *got != 42.5 {
		t.Fatalf("idle = %v, want 42.5", got)
	}
}

// test_session_stall_watchdog.py test_resolve_idle_rejects_nonfinite_seconds_since_activity
func TestResolveIdleRejectsNonfiniteSecondsSinceActivity(t *testing.T) {
	now := 1_000_000.0
	if got := gateway.ResolveSessionIdleSecondsFromActivity(map[string]any{"seconds_since_activity": math.NaN(), "last_activity_at": now - 15}, &now); got == nil || *got != 15.0 {
		t.Fatalf("idle = %v, want 15", got)
	}
}

// test_session_stall_watchdog.py test_resolve_idle_rejects_boolean_seconds_and_uses_timestamp
func TestResolveIdleRejectsBooleanSecondsAndUsesTimestamp(t *testing.T) {
	now := 1_000_000.0
	if got := gateway.ResolveSessionIdleSecondsFromActivity(map[string]any{"seconds_since_activity": true, "last_activity_ts": now - 3}, &now); got == nil || *got != 3.0 {
		t.Fatalf("idle = %v, want 3", got)
	}
}
