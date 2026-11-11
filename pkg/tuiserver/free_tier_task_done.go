package tuiserver

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Whether a turn completed on its own with a reply on the guest identity of the free tier
func isCleanTask(result map[string]any, a *agent.AIAgent, userInput bool, displayKind string) bool {
	if displayKind == "auto_continue" || userInput {
		return false
	}
	if result == nil || !pyrepr.Truthy(result["completed"]) {
		return false
	}
	if pyrepr.Truthy(result["pending_steer"]) || pyrepr.Truthy(result["user_intervened"]) {
		return false
	}
	if pystr.Strip(pyrepr.Str(orEmpty(result["final_response"]))) == "" {
		return false
	}
	return a != nil && provider.IsAnonymousRequest(a.Provider(), a.APIKey())
}

// Records a clean free tier task for the sign in offer without ever failing the turn
func noteTaskDone(ctx context.Context, session *tuigateway.Session, result map[string]any, a *agent.AIAgent, displayKind string) {
	if !isCleanTask(result, a, pyrepr.Truthy(session.Get("_turn_user_input")), displayKind) {
		return
	}
	if err := provider.RecordTaskDone(scopedPoolEnv(ctx), provider.FreeTierOfferTrial); err != nil {
		slog.Default().With("logger", "tui_gateway.free_tier_task_done").Debug("sign-in offer task record failed", "error", err)
	}
}
