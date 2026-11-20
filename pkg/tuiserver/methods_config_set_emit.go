package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Emits session.info for a session with a live agent
func emitSessionInfo(ctx context.Context, sid string, session *tuihost.Session) {
	if a := sessionAgent(session); a != nil {
		tuihost.Emit(ctx, "session.info", sid, sessionInfo(ctx, a, session))
	}
}

// Emits session.info for every live session
func emitAllSessionInfo(ctx context.Context) {
	for _, live := range tuihost.LiveSessions() {
		emitSessionInfo(ctx, live.SID, live.Session)
	}
}
