package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Seconds the masked secret prompt waits for a viewer, the CLI secret panel deadline
const secretTimeoutS = 120.0

// Skipped capture result the secret callback returns
func secretSkipped(varName, reason, message string) map[string]any {
	return map[string]any{"success": true, "reason": reason, "stored_as": varName, "validated": false,
		"skipped": true, "message": message}
}

// Binds the viewer secret capture to the turn context for a local viewer turn
func secretPromptScope(ctx context.Context, t *TurnRunner, a *agent.AIAgent) context.Context {
	if !localViewerTurn(t) {
		return ctx
	}
	authority, sessionID := t.approvalOwner.authority, t.approvalOwner.sessionID
	callback := func(varName, prompt string, metadata map[string]any) (map[string]any, error) {
		live, ok := authority.Live(sessionID)
		if !ok || live == nil {
			return secretSkipped(varName, "cancelled", "Secret setup was skipped."), nil
		}
		params := pyjson.ObjectOf("env_var", varName, "prompt", prompt)
		if len(metadata) > 0 {
			params.Set("metadata", secretMetadataObject(metadata))
		}
		timeout := secretTimeoutS
		value := gateway.AskViewer(live.EventStream, sessionID, "secret", params, a.InterruptRequested, &timeout, true)
		if value == "" {
			return secretSkipped(varName, "cancelled", "Secret setup was skipped."), nil
		}
		saved, err := config.SaveEnvValueSecure(ctx, home.Dir(ctx), varName, value)
		if err != nil {
			return nil, err
		}
		return map[string]any{"success": saved.Success, "stored_as": saved.StoredAs, "validated": saved.Validated,
			"skipped": false, "message": "Secret stored securely. The secret value was not exposed to the model."}, nil
	}
	return skills.WithTurnSecretCapture(ctx, callback)
}

// Capture metadata in the order the skill setup builds it, skill name then help then required for
func secretMetadataObject(metadata map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, key := range []string{"skill_name", "help", "required_for"} {
		if v, ok := metadata[key]; ok {
			out.Set(key, v)
		}
	}
	for _, key := range sortedKeys(metadata) {
		if _, seen := out.Get(key); !seen {
			out.Set(key, metadata[key])
		}
	}
	return out
}
