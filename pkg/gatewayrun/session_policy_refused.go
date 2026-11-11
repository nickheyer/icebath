package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Failed never run turn for a scope entry refusal an unchanged resend repeats, nil for any other refusal
func refusedTurnResult(ctx context.Context, policy *gateway.LocalSessionPolicy, exc *state.RuntimeStoreError) map[string]any {
	var result map[string]any
	switch exc.Reason {
	case "workspace_unavailable":
		result, _ = pyjson.Plain(gateway.WorkspaceUnavailableResult(ctx, policy)).(map[string]any)
	case "launch_credentials_unavailable":
		text := agent.LaunchCredentialsCopy(ctx, exc)
		result = agent.StampFailure(map[string]any{"final_response": text, "messages": []any{}, "api_calls": 0, "tools": []any{},
			"failed": true, "completed": false, "error": text}, "launch_credentials_unavailable", false)
	default:
		return nil
	}
	result["error_surface"] = agent.BuildErrorSurfaceFromResult(result, "", "")
	return result
}

// One turn reasoning level the executing admission committed with its model override, else the session's
func oneTurnReasoning(ctx context.Context, t *TurnRunner, reasoningConfig map[string]any) map[string]any {
	var inbound any
	if t.ctx.inboundMessageID != nil {
		inbound = *t.ctx.inboundMessageID
	}
	override, _ := gateway.CLITurn(ctx, inbound).GetOr("model_override", nil).(*pyjson.Object)
	if override == nil {
		return reasoningConfig
	}
	level, has := override.Get("reasoning")
	if !has {
		return reasoningConfig
	}
	if parsed := config.ParseReasoningEffort(level); len(parsed) > 0 {
		return parsed
	}
	return reasoningConfig
}
