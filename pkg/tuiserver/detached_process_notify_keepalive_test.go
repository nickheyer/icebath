package tuiserver

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Detached idle desktop session with an isolated process registry and a twenty second orphan grace
func detachedProcSession(t *testing.T) (string, *tuigateway.Session, *tools.ProcessRegistry) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	registry := tools.DefaultProcessRegistry()
	t.Cleanup(tools.IsolateProcessRegistryForTests(registry))
	sid := "sid-detached-proc"
	session := tuigateway.NewSession(map[string]any{"session_key": "desktop-proc-session", "transport": tuigateway.DetachedWSTransport(),
		"running": false, "history_lock": &sync.Mutex{}, "last_active": 0.0, "created_at": 0.0})
	swapSessions(t, map[string]*tuigateway.Session{sid: session})
	swapVar(t, &wsOrphanReapGraceS, func() float64 { return 20 })
	t.Cleanup(func() { cancelWSOrphanReap(sid) })
	return sid, session, registry
}

// Background process a session turn spawned, owned by the session key
func spawnOwnedProcess(registry *tools.ProcessRegistry, owner string, notify bool) *tools.ProcessSession {
	proc := tools.NewProcessSession(fmt.Sprintf("proc_%d", registry.CountRunning()), "icebath -p reviewer chat")
	proc.SessionKey, proc.OwnerTaskID, proc.StartedAt, proc.NotifyOnComplete = owner, owner, nowSeconds(), notify
	registry.InsertRunning(proc)
	return proc
}

func TestOrphanReapRearmsWhileProcessWillNotify(t *testing.T) {
	sid, session, registry := detachedProcSession(t)
	spawnOwnedProcess(registry, "desktop-proc-session", true)
	scheduleWSOrphanReap(context.Background(), sid, nil, nil)
	first := pendingReap(sid)
	fireReap(first, 20, 20)
	if tuigateway.SessionRecord(sid) != session || truthy(session.Get("_finalized")) {
		t.Fatal("session with a pending process notice was reaped")
	}
	if next := pendingReap(sid); next == nil || next == first {
		t.Fatal("orphan reap was not rearmed")
	}
}

func TestForeignNoticeInAnotherSessionsPostTurnDrainKeepsItsOwner(t *testing.T) {
	sid, session, registry := detachedProcSession(t)
	ctx := context.Background()
	proc := spawnOwnedProcess(registry, "desktop-proc-session", true)
	tools.FinishProcessForTests(ctx, registry, proc, 0)
	otherSID := "other-sid"
	other := tuigateway.NewSession(map[string]any{"session_key": "other-owner", "transport": tuigateway.DetachedWSTransport(),
		"running": false, "history_lock": &sync.Mutex{}, "last_active": 0.0, "created_at": 0.0})
	tuigateway.SetSessionRecord(otherSID, other)
	t.Cleanup(func() { tuigateway.SetSessionRecord(otherSID, nil) })
	realOwns := sessionOwnsNotificationEvent
	type observation struct{ queueEmpty, reapable bool }
	var observed []observation
	swapVar(t, &sessionOwnsNotificationEvent, func(ctx context.Context, drainingSID string, draining *tuigateway.Session, evt map[string]any) bool {
		if drainingSID == otherSID && evt["owner_task_id"] == "desktop-proc-session" {
			observed = append(observed, observation{registry.CompletionQueue.Empty(), sessionIsReapable(ctx, sid, session)})
			scheduleWSOrphanReap(ctx, sid, nil, nil)
			fireReap(pendingReap(sid), 20, 20)
		}
		return realOwns(ctx, drainingSID, draining, evt)
	})
	var routed [][]map[string]any
	swapVar(t, &notifHandleReady, func(_ context.Context, _ string, _ *tuigateway.Session, events []map[string]any, _ map[string]bool,
		_ *tools.ProcessRegistry, _ func(map[string]any) string, _ *[]map[string]any, _ bool) {
		routed = append(routed, events)
	})
	runPostTurnFollowups(ctx, nil, otherSID, other, map[string]any{}, "")
	if len(observed) != 1 || observed[0] != (observation{true, false}) {
		t.Fatalf("observed %v, the notice was in the drainer's hands so its owner stays protected", observed)
	}
	if tuigateway.SessionRecord(sid) != session || truthy(session.Get("_finalized")) {
		t.Fatal("owner of the notice was reaped")
	}
	queued := registry.CompletionQueue.Snapshot()
	if len(queued) != 1 || queued[0]["owner_task_id"] != "desktop-proc-session" {
		t.Fatalf("queue %v", queued)
	}
	if len(routed) != 1 || len(routed[0]) != 0 {
		t.Fatalf("routed %v", routed)
	}
}
