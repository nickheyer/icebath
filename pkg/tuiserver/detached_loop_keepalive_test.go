package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Stores an active half hour loop for the session key, with an optional gateway chat route
func setSessionLoop(t *testing.T, sessionKey string, route *pyjson.Object) {
	t.Helper()
	interval := int64(1800)
	if _, err := agent.NewLoopManager(context.Background(), sessionKey).Set(context.Background(), "reconcile the spec", &interval, 0, "", route); err != nil {
		t.Fatal(err)
	}
}

// Stores an active half hour heartbeat for the session key
func setSessionHeartbeat(t *testing.T, sessionKey string) *agent.HeartbeatManager {
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
func assertKeptDetached(t *testing.T, sid string, session *tuihost.Session) {
	t.Helper()
	if tuihost.SessionRecord(sid) != session {
		t.Fatal("detached session was reaped")
	}
	if pendingReap(sid) == nil {
		t.Fatal("reap was not re-armed")
	}
}

// Fails unless the reap popped the session
func assertCollected(t *testing.T, sid string) {
	t.Helper()
	if tuihost.SessionRecord(sid) != nil {
		t.Fatal("detached session survived the reap")
	}
}
