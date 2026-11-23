package tuiserver

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Status report facts and their display lines, assigned from the CLI status report at startup
var StatusReport struct {
	Fields func(ctx context.Context, sessionID string, a *agent.AIAgent, meta map[string]any, model, provider string, tokens any, agentRunning bool, profileHome string) map[string]any
	Lines  func(fields map[string]any, keys ...string) []string
}

// Rewinds a session's history to just before its last user turn, the kept history, the undone message and the count removed; busy while a turn runs
func rewindLastExchange(ctx context.Context, session *tuihost.Session) (kept []map[string]any, undone map[string]any, removed int, busy bool, err error) {
	if truthy(session.Get("running")) {
		return nil, nil, 0, true, nil
	}
	unlock := lockHistory(session)
	defer unlock()
	if truthy(session.Get("running")) {
		return nil, nil, 0, true, nil
	}
	history := historyWithoutEphemeralScaffolding(historyList(session.Get("history")))
	userTurns := 0
	for _, message := range history {
		if agent.UserOriginatedTurnView(message) != nil {
			userTurns++
		}
	}
	if userTurns == 0 {
		return nil, nil, 0, false, nil
	}
	if failure := catchPanic(func() {
		kept, undone, removed, err = rewindActiveSessionHistory(ctx, session, userTurns-1, false)
	}); failure != nil {
		err = fmt.Errorf("%s", pyrepr.Str(failure))
	}
	return kept, undone, removed, false, err
}
