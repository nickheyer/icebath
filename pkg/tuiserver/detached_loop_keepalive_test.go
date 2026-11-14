package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Detached idle desktop session under a fresh home, with a 20 s orphan grace and no delegations
func detachedLoopSession(t *testing.T) (string, *tuigateway.Session) {
	t.Helper()
	sid := "sid-detached-loop"
	session := orphanRaceSession(t, sid, 600, map[string]any{"session_key": "desktop-loop-session", "running": false,
		"last_active": 0.0, "created_at": 0.0})
	return sid, session
}

// Stores an active half hour loop for the session key, with an optional gateway chat route
func setDesktopLoop(t *testing.T, sessionKey string, route *pyjson.Object) {
	t.Helper()
	interval := int64(1800)
	if _, err := agent.NewLoopManager(context.Background(), sessionKey).Set(context.Background(), "reconcile the spec", &interval, 0, "", route); err != nil {
		t.Fatal(err)
	}
}

// Stores an active half hour heartbeat for the session key
func setDesktopHeartbeat(t *testing.T, sessionKey string) *agent.HeartbeatManager {
	t.Helper()
	heartbeat := agent.NewHeartbeatManager(context.Background(), sessionKey)
	if _, err := heartbeat.Set("check the board", 1800); err != nil {
		t.Fatal(err)
	}
	return heartbeat
}

// Arms the orphan reap and fires it once its grace elapsed awake
func fireOrphanReap(sid string) {
	scheduleWSOrphanReap(context.Background(), sid, nil, nil)
	fireReap(pendingReap(sid), 20, 20)
}

// Fails unless the reap kept the session registered with its timer re-armed
func assertKeptDetached(t *testing.T, sid string, session *tuigateway.Session) {
	t.Helper()
	if tuigateway.SessionRecord(sid) != session {
		t.Fatal("detached session was reaped")
	}
	if pendingReap(sid) == nil {
		t.Fatal("reap was not re-armed")
	}
}

// Fails unless the reap popped the session
func assertCollected(t *testing.T, sid string) {
	t.Helper()
	if tuigateway.SessionRecord(sid) != nil {
		t.Fatal("detached session survived the reap")
	}
}

func TestOrphanReapKeepsDetachedSessionWithActiveDesktopLoop(t *testing.T) {
	sid, session := detachedLoopSession(t)
	setDesktopLoop(t, "desktop-loop-session", nil)
	fireOrphanReap(sid)
	assertKeptDetached(t, sid, session)
}

func TestOrphanReapStillCollectsSessionOnceLoopStops(t *testing.T) {
	sid, _ := detachedLoopSession(t)
	setDesktopLoop(t, "desktop-loop-session", nil)
	agent.NewLoopManager(context.Background(), "desktop-loop-session").Clear(context.Background())
	fireOrphanReap(sid)
	assertCollected(t, sid)
}

func TestGatewayRoutedLoopDoesNotPinDetachedSession(t *testing.T) {
	sid, _ := detachedLoopSession(t)
	route := pyjson.NewObject()
	route.Set("platform", "discord")
	route.Set("chat_id", "123")
	setDesktopLoop(t, "desktop-loop-session", route)
	fireOrphanReap(sid)
	assertCollected(t, sid)
}

func TestTtlAndCapReapersSpareSessionWithActiveDesktopLoop(t *testing.T) {
	ctx := context.Background()
	sid, session := detachedLoopSession(t)
	prevTTL := sessionTTLS
	sessionTTLS = 1
	t.Cleanup(func() { sessionTTLS = prevTTL })
	now := nowSeconds()
	if !sessionIsEvictable(ctx, sid, session, now) {
		t.Fatal("idle detached session past its TTL is not evictable")
	}
	setDesktopLoop(t, "desktop-loop-session", nil)
	if sessionIsEvictable(ctx, sid, session, now) {
		t.Fatal("looping session is evictable")
	}
	if sessionIsReapable(ctx, sid, session) {
		t.Fatal("looping session is reapable")
	}
	if !sessionIsLRUEvictable(ctx, sid, session) {
		t.Fatal("idle looping session blocks a memory trim")
	}
}

func TestSettledClientGoneInterruptIsCollectedDespiteActiveLoop(t *testing.T) {
	sid, session := detachedLoopSession(t)
	setDesktopLoop(t, "desktop-loop-session", nil)
	orphanRaceStale(t, 0)
	session.Set("_run_thread", liveTurnThread(t))
	session.Set("running", true)
	fireOrphanReap(sid)
	if !truthy(session.Get("_client_gone_interrupt_requested")) || tuigateway.SessionRecord(sid) != session {
		t.Fatal("stale detached turn was not interrupted in place")
	}
	session.Set("running", false)
	fireReap(pendingReap(sid), 1, 1)
	assertCollected(t, sid)
}

func TestOrphanReapKeepsDetachedSessionWithActiveLocalHeartbeat(t *testing.T) {
	sid, session := detachedLoopSession(t)
	setDesktopHeartbeat(t, "desktop-loop-session")
	fireOrphanReap(sid)
	if tuigateway.SessionRecord(sid) != session {
		t.Fatal("detached session with a local heartbeat was reaped")
	}
}

func TestPausedOrClearedHeartbeatReleasesDetachedSession(t *testing.T) {
	for _, release := range []string{"pause", "clear"} {
		t.Run(release, func(t *testing.T) {
			sid, _ := detachedLoopSession(t)
			setDesktopHeartbeat(t, "desktop-loop-session")
			heartbeat := agent.NewHeartbeatManager(context.Background(), "desktop-loop-session")
			if release == "pause" {
				heartbeat.Pause()
			} else {
				heartbeat.Clear()
			}
			fireOrphanReap(sid)
			assertCollected(t, sid)
		})
	}
}

func TestGatewayOwnedHeartbeatDoesNotPinDetachedSession(t *testing.T) {
	ctx := context.Background()
	sid, _ := detachedLoopSession(t)
	setDesktopHeartbeat(t, "desktop-loop-session")
	db := ledgerHandle(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "state.db"))
	useSharedDB(t, db)
	entry := `{"session_id": "desktop-loop-session", "origin": {"platform": "discord", "chat_id": "123"}}`
	if err := db.SaveGatewayRoutingEntry(ctx, "agent:main:discord:dm:123", entry, "default"); err != nil {
		t.Fatal(err)
	}
	fireOrphanReap(sid)
	assertCollected(t, sid)
}
