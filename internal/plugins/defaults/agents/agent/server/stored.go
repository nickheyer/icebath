package server

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nickheyer/icebath/internal/config"
	agentprompt "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Keys of the model settings of a session that record what its stored prompt was built for and under which refresh count
const (
	promptKey   = "_prompt_key"
	promptEpoch = "_prompt_epoch"
	// Length of the start of the stored prompt that stays the same from session to session
	promptStatic = "_prompt_static"
)

// Prompt the session stored on an earlier run, so the provider's prompt cache still holds it; nil when it has none, it was built for
// something else, or a refresh asked for a new one since
func (a *Agent) storedPrompt(ctx context.Context, store sessions.Store, settings *config.Config, session *sessionsv1.Session, key string, epoch int64,
	client *tools.Client) *system {
	var recorded struct {
		Key    string `json:"_prompt_key"`
		Epoch  int64  `json:"_prompt_epoch"`
		Static int    `json:"_prompt_static"`
	}
	if session.GetModelConfig() == "" || json.Unmarshal([]byte(session.GetModelConfig()), &recorded) != nil || recorded.Key != key || recorded.Epoch != epoch {
		return nil
	}
	text, err := store.SystemPrompt(ctx, session.GetId())
	if err != nil || text == "" {
		return nil
	}
	prefill, err := agentprompt.Prefill(settings, client)
	if err != nil {
		return nil
	}
	built := &system{key: key, text: text, prefill: prefill, epoch: epoch}
	if recorded.Static > 0 && recorded.Static <= len(text) {
		built.static = text[:recorded.Static]
	}
	return built
}

// Stores the prompt built for the session with the tools it offers, what it was built for and the refresh count it answers
func (a *Agent) keepPrompt(ctx context.Context, store sessions.Store, id string, built *system, tools []string) {
	// The prompt built is kept though the turn it was built for was stopped since
	ctx = context.WithoutCancel(ctx)
	if err := store.SetSystemPrompt(ctx, id, built.text, tools); err != nil {
		slog.Warn("Could not store the session's system prompt", "session", id, "error", err)
		return
	}
	if err := store.Update(ctx, id, sessions.Change{ModelConfig: map[string]any{promptKey: built.key, promptEpoch: built.epoch,
		promptStatic: len(built.static)}}); err != nil {
		slog.Warn("Could not record what the session's system prompt was built for", "session", id, "error", err)
	}
}
