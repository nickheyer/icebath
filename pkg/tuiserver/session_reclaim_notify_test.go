package tuiserver

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Records every global broadcast for the length of a case
func captureBroadcasts(t *testing.T) *[][2]any {
	t.Helper()
	events := &[][2]any{}
	prev := broadcastGlobalEvent
	broadcastGlobalEvent = func(event string, payload map[string]any) { *events = append(*events, [2]any{event, payload}) }
	t.Cleanup(func() { broadcastGlobalEvent = prev })
	return events
}

// Session record of the reclaim cases with its profile home holding the stored row
func reclaimSession(t *testing.T) *tuigateway.Session {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	profile := t.TempDir()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(profile, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "20260731_120000_aaaaaa", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return tuigateway.NewSession(map[string]any{"_sid": "live-abc", "session_key": "20260731_120000_aaaaaa", "profile_home": profile})
}

// End reason the stored row of a reclaim session carries after teardown
func reclaimEndReason(t *testing.T, session *tuigateway.Session) any {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(strOr(session.Get("profile_home")), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := db.GetSession(context.Background(), "20260731_120000_aaaaaa")
	if err != nil || row == nil {
		t.Fatalf("row %v %v", row, err)
	}
	return row.GetOr("end_reason", nil)
}

func TestReclaimReasonsAnnounceToClients(t *testing.T) {
	for _, reason := range []string{"idle_timeout", "lru_evict", "ws_orphan_reap"} {
		t.Run(reason, func(t *testing.T) {
			events := captureBroadcasts(t)
			teardownSession(context.Background(), reclaimSession(t), reason)
			want := [][2]any{{"session.reclaimed", map[string]any{"session_id": "live-abc", "stored_session_id": "20260731_120000_aaaaaa", "reason": reason}}}
			if !reflect.DeepEqual(*events, want) {
				t.Fatalf("broadcasts %v", *events)
			}
		})
	}
}

func TestClientInitiatedClosesStaySilent(t *testing.T) {
	for _, reason := range []string{"tui_close", "tui_shutdown", "ws_disconnect", "branched"} {
		t.Run(reason, func(t *testing.T) {
			events := captureBroadcasts(t)
			teardownSession(context.Background(), reclaimSession(t), reason)
			if len(*events) != 0 {
				t.Fatalf("broadcasts %v", *events)
			}
		})
	}
}

func TestBroadcastFailureDoesNotBreakTeardown(t *testing.T) {
	prev := broadcastGlobalEvent
	broadcastGlobalEvent = func(string, map[string]any) { panic("transport gone") }
	t.Cleanup(func() { broadcastGlobalEvent = prev })
	session := reclaimSession(t)
	teardownSession(context.Background(), session, "ws_orphan_reap")
	if !truthy(session.Get("_finalized")) || reclaimEndReason(t, session) != "ws_orphan_reap" {
		t.Fatalf("finalize did not run with ws_orphan_reap, end reason %v", reclaimEndReason(t, session))
	}
}

func TestReapPathsStampTheRuntimeIdTheClientHolds(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tuigateway.SetSessionRecord("live-xyz", tuigateway.NewSession(map[string]any{"session_key": "k"}))
	popped := popSessionByID(context.Background(), "live-xyz")
	tuigateway.SetSessionRecord("live-xyz", nil)
	if popped == nil || popped.Get("_sid") != "live-xyz" {
		t.Fatalf("popped %v", popped)
	}
}
