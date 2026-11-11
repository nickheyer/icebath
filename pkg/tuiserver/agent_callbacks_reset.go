package tuiserver

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Rebuilds a session agent at a conversation boundary, dropping runtime overrides and history, and pushes its info
func resetSessionAgent(ctx context.Context, sid string, session *tuigateway.Session) (map[string]any, error) {
	rawGeneration, present := session.Lookup("_queued_prompt_generation")
	if !present {
		rawGeneration = 0
	}
	generation, err := pyint.Int64(rawGeneration)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{"attached_images": []any{}, "queued_prompt": nil, "_queued_prompt_generation": generation + 1,
		"edit_snapshots": map[string]any{}, "image_counter": 0, "running": false, "show_reasoning": loadShowReasoning(ctx),
		"tool_progress_mode": loadToolProgressMode(ctx), "tool_started_at": map[string]any{}}
	sessionKey, hasKey := session.Lookup("session_key")
	if !hasKey {
		return nil, fmt.Errorf("'session_key'")
	}
	scoped, env := setSessionContext(ctx, strOr(sessionKey), nil, "")
	newAgent, err := func() (*agent.AIAgent, error) {
		defer clearSessionContext(env)
		for _, k := range []string{"model_override", "create_reasoning_override", "create_service_tier_override", "one_turn_model_restore"} {
			session.Pop(k)
		}
		launch := contextCwdIsLaunchArtifact(session)
		return rebuildSessionAgent(scoped, sid, session, makeAgentOptions{sessionID: strOr(sessionKey),
			platformOverride: sessionSource(session), contextCwdIsLaunchArtifact: &launch}, false)
	}()
	if err != nil {
		return nil, err
	}
	session.Update(updates)
	session.Pop("queued_prompts")
	func() {
		unlock := lockHistory(session)
		defer unlock()
		session.Set("history", []any{})
		session.Set("history_version", intOr(session.Get("history_version"))+1)
	}()
	info := sessionInfo(ctx, newAgent, session)
	tuigateway.Emit(ctx, "session.info", sid, info)
	restartSlashWorker(ctx, sid, session)
	return info, nil
}
