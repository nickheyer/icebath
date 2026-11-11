package tuiserver

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func notifLogger() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.session_notifications")
}

// Whether the live gateway routing index of either store owns the due heartbeat tick of this session key
func notifGatewayOwnsHeartbeat(ctx context.Context, session *tuigateway.Session, sessionKey string) (owns bool) {
	defer func() {
		if recover() != nil {
			owns = false
		}
	}()
	db, release := sessionDB(ctx, session)
	defer release()
	seen := map[*state.DB]bool{}
	for _, store := range []*state.DB{db, getDB(ctx)} {
		if store == nil || seen[store] {
			continue
		}
		seen[store] = true
		entry, err := store.GatewayRoutingEntryForSession(ctx, sessionKey)
		if err != nil {
			return false
		}
		if entry != nil {
			origin, _ := entry.Get("origin")
			suspended, _ := entry.Get("suspended")
			return truthy(origin) && !truthy(suspended)
		}
	}
	return false
}

// Whether a loop carries the gateway chat route its own wakeup scanner fires
func loopRouteIsGatewayChat(st *agent.SessionLoopState) bool {
	if st == nil || st.Route == nil {
		return false
	}
	platform, _ := st.Route.Get("platform")
	chatID, _ := st.Route.Get("chat_id")
	return truthy(platform) && truthy(chatID)
}

// Whether this process's poller alone drives an active loop or heartbeat of the session, false on any failure
func sessionOwnsLiveWakeupSchedule(ctx context.Context, session *tuigateway.Session) (owns bool) {
	sidKey := strOr(session.Get("session_key"))
	if sidKey == "" {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			notifLogger().Debug("wakeup-schedule check failed for "+sidKey, "error", r)
			owns = false
		}
	}()
	scoped := sessionProfileRuntimeScope(ctx, session, false)
	loop := agent.NewLoopManager(scoped, sidKey)
	if loop.IsActive() && !loopRouteIsGatewayChat(loop.State()) {
		return true
	}
	heartbeat := agent.NewHeartbeatManager(scoped, sidKey)
	return heartbeat.IsActive() && !notifGatewayOwnsHeartbeat(scoped, session, sidKey)
}

// Whether a background process this session started still has a completion or watch notice on its way
func sessionOwnsPendingProcessNotification(ctx context.Context, session *tuigateway.Session) bool {
	if truthy(session.Get("_turn_cancel_requested")) {
		return false
	}
	return tools.DefaultProcessRegistry().AwaitsNotification(sessionProcessOwners(session))
}

// Raw spawning ids close kills by plus the current session key
func sessionProcessOwners(session *tuigateway.Session) []string {
	owners := map[string]bool{}
	if a := sessionAgent(session); a != nil {
		for _, owner := range a.ProcessOwnerTaskIDs() {
			owners[owner] = true
		}
	}
	owners[strOr(session.Get("session_key"))] = true
	delete(owners, "")
	out := make([]string, 0, len(owners))
	for owner := range owners {
		out = append(out, owner)
	}
	return out
}
