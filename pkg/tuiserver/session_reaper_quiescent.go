package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Hard exemptions of both reapers, never mid turn, awaiting input, building, delegating or on a live transport
func sessionIsLRUEvictable(ctx context.Context, sid string, session *tuigateway.Session) bool {
	if truthy(session.Get("running")) || sessionPendingKind(sid) != "" || sessionHasActiveDelegations(ctx, sid, session) {
		return false
	}
	if ready, isEvent := session.Get("agent_ready").(*pythread.Event); isEvent && ready != nil && !ready.IsSet() && !truthy(session.Get("lazy")) {
		return false
	}
	transport, _ := session.Get("transport").(tuigateway.Transport)
	return tuigateway.TransportIsDead(transport)
}

// Whether no session but the excluded one is busy, building, awaiting input, delegating or on a live transport
func sessionsQuiescent(exclude string) bool {
	ctx := context.Background()
	for _, live := range tuigateway.LiveSessions() {
		if live.SID == exclude {
			continue
		}
		if !sessionIsLRUEvictable(ctx, live.SID, live.Session) {
			return false
		}
	}
	return true
}

// Reaper eligibility, the LRU exemptions and not the sole receiver of a pending loop, heartbeat or process notice
func sessionIsReapable(ctx context.Context, sid string, session *tuigateway.Session) bool {
	return sessionIsLRUEvictable(ctx, sid, session) && !sessionOwnsLiveWakeupSchedule(ctx, session) &&
		!sessionOwnsPendingProcessNotification(ctx, session)
}
