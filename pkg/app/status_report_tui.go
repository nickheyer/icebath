package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Status facts of a TUI session, the live agent winning route and tokens, the profile home naming the path
func TUIStatusFields(ctx context.Context, sessionID string, a *agent.AIAgent, meta map[string]any, model, provider string, tokens any, agentRunning bool, profileHome string) map[string]any {
	scoped := ctx
	if profileHome != "" {
		scoped = home.WithOverride(ctx, profileHome)
	}
	in := statusFieldsInput{ctx: scoped, sessionID: sessionID, meta: meta, model: model, provider: provider, agentRunning: agentRunning}
	if a != nil {
		in.agentModel, in.agentProvider, in.agentTokens = a.Model(), a.Provider(), a.SessionTotalTokens()
	}
	if tokens != nil {
		if n, err := pyint.Int64(tokens); err == nil {
			in.agentTokens = int(n)
		}
	}
	return buildStatusFields(in)
}

// Label value lines of the status facts for the keys in order
func TUIStatusLines(fields map[string]any, keys ...string) []string {
	return statusLines(fields, keys...)
}
