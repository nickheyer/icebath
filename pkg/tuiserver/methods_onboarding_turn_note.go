package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Appends the onboarding chat note before a user turn, naming a setup card still waiting on the client
func stageOnboardingTurnNote(ctx context.Context, sid string, a *agent.AIAgent, historyEmpty bool) {
	tools.AppendTurnNote(a, tools.OnboardingSessionTurnNote(a, historyEmpty, sessionPendingKind(sid) == "setup_beat"))
}
