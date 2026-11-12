package tuiserver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Detached isolated desktop turn of the activity cases
func isolatedActivitySession(t *testing.T) *tuigateway.Session {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	session := tuigateway.NewSession(map[string]any{"agent": nil, "agent_ready": &pythread.Event{}, "session_key": "session", "history": []any{},
		"history_version": 0, "history_lock": &sync.Mutex{}, "running": true, "transport": tuigateway.DetachedWSTransport(),
		"attached_images": []any{}, "cols": 80, "source": "desktop", "inflight_turn": nil,
		"_compute_host_active": true, "_compute_host_turn_id": "new-turn"})
	swapSessions(t, map[string]*tuigateway.Session{"session": session})
	swapVar(t, &wsOrphanActivityStaleS, func() float64 { return 30 })
	return session
}

// Relays one activity frame on a context whose client must receive nothing
func relayActivity(t *testing.T, params map[string]any) {
	t.Helper()
	client := &recordingTransport{}
	relayComputeHostRPC(tuigateway.BindTransport(context.Background(), client), pyjson.ObjectOf("jsonrpc", "2.0", "method", "compute_host.activity", "params", objectOf(params)))
	if len(client.frames) != 0 {
		t.Fatalf("internal activity leaked to client %v", client.frames)
	}
}

func TestActivityRelayIsFencedAndAges(t *testing.T) {
	for _, change := range []string{"none", "other-session", "old-turn", "not-running", "stale", "missing"} {
		t.Run(change, func(t *testing.T) {
			session := isolatedActivitySession(t)
			params := map[string]any{"session_id": "session", "turn_id": "new-turn", "activity_ns": monotonicNS()}
			switch change {
			case "other-session":
				params["session_id"] = "other"
			case "old-turn":
				params["turn_id"] = "old-turn"
			case "not-running":
				session.Set("running", false)
			case "stale":
				params["activity_ns"] = params["activity_ns"].(int64) - 31_000_000_000
			case "missing":
				params["activity_ns"] = nil
			}
			relayActivity(t, params)
			if fresh := wsOrphanTurnActivityIsFresh(session); fresh != (change == "none") {
				t.Fatalf("fresh %v", fresh)
			}
			if change != "none" {
				return
			}
			params["activity_ns"] = monotonicNS() - 29_800_000_000
			relayActivity(t, params)
			if !wsOrphanTurnActivityIsFresh(session) {
				t.Fatal("a 29.8 s old activity is stale")
			}
			time.Sleep(300 * time.Millisecond)
			relayActivity(t, params)
			if wsOrphanTurnActivityIsFresh(session) {
				t.Fatal("repeated delivery of the same clock refreshed the activity")
			}
		})
	}
}
