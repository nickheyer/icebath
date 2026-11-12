package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const sessionEpochKey = "prompt_refresh_epoch"

func promptRefreshLog() *slog.Logger { return slog.Default().With("logger", "agent.prompt_refresh") }

// Store recording operator prompt refresh requests, a managed worker facade keeps none
type promptRefreshEpochReader interface {
	PromptRefreshEpoch(ctx context.Context, sessionID string) (int, error)
}

var _ promptRefreshEpochReader = (*state.SessionDB)(nil)

// Newest refresh epoch recorded for the agent's session, zero without a request or a store
func requestedEpoch(ctx context.Context, a *AIAgent) int {
	reader, ok := a.sessionDB.(promptRefreshEpochReader)
	if !ok || a.sessionID == "" {
		return 0
	}
	epoch, err := reader.PromptRefreshEpoch(ctx, a.sessionID)
	if err != nil {
		promptRefreshLog().Warn(fmt.Sprintf("Prompt refresh epoch read failed (session=%s); reusing the stored prompt.", a.sessionID), "error", err)
		return 0
	}
	return epoch
}

// Refresh epoch the stored session prompt already satisfies, from its model config
func appliedEpoch(sessionRow *pyjson.Object) int {
	var raw any
	if sessionRow != nil {
		raw, _ = sessionRow.Get("model_config")
	}
	return intEpoch(state.ParseModelConfig(raw)[sessionEpochKey])
}

// Epoch held by a decoded value when it is a Python int, zero otherwise, clamped to the Go int range
func intEpoch(v any) int {
	n, ok := pyint.Instance(v)
	if !ok {
		return 0
	}
	if !n.IsInt64() || n.Int64() > math.MaxInt || n.Int64() < math.MinInt {
		if n.Sign() < 0 {
			return math.MinInt
		}
		return math.MaxInt
	}
	return int(n.Int64())
}

// Epoch to rebuild the stored prompt at, zero when the stored prompt is current
func storedPromptRefreshEpoch(ctx context.Context, a *AIAgent, sessionRow *pyjson.Object) int {
	epoch := requestedEpoch(ctx, a)
	if epoch != 0 && appliedEpoch(sessionRow) < epoch {
		return epoch
	}
	a.promptRefreshEpoch = &epoch
	return 0
}

// Records that the freshly built prompt satisfies epoch, read before the build so a mid build request still fires
func stampPromptEpoch(a *AIAgent, epoch int) {
	a.promptRefreshEpoch = &epoch
	if epoch != 0 {
		persistPromptEpoch(context.Background(), a, epoch)
	}
}

// Writes the epoch into the session model config, false when the write failed
func persistPromptEpoch(ctx context.Context, a *AIAgent, epoch int) bool {
	if a.sessionDB == nil {
		return true
	}
	if err := a.sessionDB.PatchSessionModelConfig(ctx, a.sessionID, map[string]any{sessionEpochKey: epoch}); err != nil {
		promptRefreshLog().Warn(fmt.Sprintf("Could not record the prompt refresh epoch (session=%s); the refresh re-fires next turn.", a.sessionID), "error", err)
		return false
	}
	return true
}

// Rebuilds the session prompt from current sources and persists it with epoch
func rebuildForRefresh(ctx context.Context, a *AIAgent, systemMessage *string, conversationHistory []map[string]any, epoch int) error {
	promptRefreshLog().Info(fmt.Sprintf("Operator requested a system prompt refresh (epoch %d) for session %s; rebuilding it "+
		"from current SOUL.md, skills and context files (one-time prefix-cache break).", epoch, a.sessionID))
	if err := InvalidateSystemPrompt(tools.WithEnvHome(ctx, a.env), a); err != nil {
		return err
	}
	a.autoLoadSkillsResolved = false
	skills.ClearSkillsSystemPromptCache(agentHome(a.env), true)
	prompt, err := BuildSystemPrompt(ctx, a, systemMessage)
	if err != nil {
		return err
	}
	a.cachedSystemPrompt = prompt
	observability.RecordCacheBreak(ctx, a, "system_prompt_rebuild")
	StageSurfaceSwitchNote(ctx, a, a.cachedSystemPrompt, conversationHistory)
	if a.sessionDB != nil {
		if err := a.sessionDB.UpdateSystemPrompt(ctx, a.sessionID, &prompt); err != nil {
			promptRefreshLog().Warn(fmt.Sprintf("Could not persist the refreshed system prompt (session=%s); the refresh re-fires next turn.", a.sessionID), "error", err)
			return nil
		}
	}
	if persistPromptEpoch(ctx, a, epoch) {
		a.promptRefreshEpoch = &epoch
	}
	return nil
}

// RefreshWarmPrompt adopts a refresh request recorded after a warm agent's prompt was set, leaving epochless agents alone
func RefreshWarmPrompt(ctx context.Context, a *AIAgent, systemMessage *string, conversationHistory []map[string]any) error {
	seen := a.promptRefreshEpoch
	if a.cachedSystemPrompt == "" || seen == nil || len(conversationHistory) == 0 {
		return nil
	}
	if epoch := requestedEpoch(ctx, a); epoch > *seen {
		return rebuildForRefresh(ctx, a, systemMessage, conversationHistory, epoch)
	}
	return nil
}
