package gatewayrun

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Run result keys a classic CLI viewer reads that the gateway projection does not carry
var cliResultPassthrough = []string{"pre_transform_response", "billing_block", "last_reasoning", "response_previewed",
	"response_transformed", "turn_exit_reason"}

// Text of a runtime kwarg, empty when absent or falsy
func runtimeText(runtimeKwargs map[string]any, key string) string {
	if s, ok := runtimeKwargs[key].(string); ok {
		return s
	}
	return ""
}

// Model and runtime of a committed model override for this one turn, resolved by the model switch pipeline
func oneTurnRuntime(ctx context.Context, override *pyjson.Object, model string, runtimeKwargs map[string]any, cfg map[string]any) (string, map[string]any, error) {
	if cfg == nil {
		cfg = map[string]any{}
	}
	rawModel, _ := override.GetOr("model", "").(string)
	explicit, _ := override.GetOr("provider", "").(string)
	userProviders, _ := cfg["providers"].(map[string]any)
	custom := []any{}
	for _, entry := range provider.CompatibleCustomProviders(cfg) {
		custom = append(custom, entry)
	}
	result, err := provider.SwitchModel(ctx, provider.SwitchModelOptions{
		RawInput: rawModel, CurrentProvider: runtimeText(runtimeKwargs, "provider"),
		CurrentBaseURL: runtimeText(runtimeKwargs, "base_url"), CurrentAPIKey: runtimeText(runtimeKwargs, "api_key"),
		IsGlobal: false, ExplicitProvider: explicit, UserProviders: userProviders, CustomProviders: custom,
	})
	if err != nil {
		return "", nil, err
	}
	if !result.Success {
		message := result.ErrorMessage
		if message == "" {
			message = "model " + pyrepr.Repr(rawModel) + " could not be resolved"
		}
		return "", nil, errors.New(message)
	}
	runtime := maps.Clone(runtimeKwargs)
	if runtime == nil {
		runtime = map[string]any{}
	}
	currentProvider := runtimeText(runtimeKwargs, "provider")
	sameProvider := strings.ToLower(strings.TrimSpace(result.TargetProvider)) == strings.ToLower(strings.TrimSpace(currentProvider))
	if sameProvider && !result.AliasEndpointApplied {
		if _, derived := provider.ModelDerivedAPIMode(ctx, result.TargetProvider, result.NewModel, ""); derived && result.APIMode != "" {
			runtime["api_mode"] = result.APIMode
		}
		apiMode, _ := runtime["api_mode"].(string)
		if apiMode == "" {
			runtime["api_mode"] = nil
		} else {
			runtime["api_mode"] = apiMode
		}
		if base := runtimeText(runtime, "base_url"); base != "" && provider.OpenCodeFamily(result.TargetProvider) != "" {
			runtime["base_url"] = provider.NormalizeOpenCodeBaseURL(result.TargetProvider, provider.APIMode(apiMode), base)
		}
	} else {
		var apiMode any
		if result.APIMode != "" {
			apiMode = result.APIMode
		}
		overrides := map[string]any{}
		maps.Copy(overrides, result.RequestOverrides)
		capabilities := map[string]any{}
		for k, v := range result.RuntimeCapabilities {
			capabilities[k] = v
		}
		runtime["credential_pool"] = nil
		runtime["command"] = nil
		runtime["args"] = []any{}
		runtime["api_key"] = result.APIKey
		runtime["base_url"] = result.BaseURL
		runtime["api_mode"] = apiMode
		runtime["request_overrides"] = overrides
		runtime["capabilities"] = capabilities
	}
	runtime["provider"] = result.TargetProvider
	runtime["requested_provider"] = result.TargetProvider
	return result.NewModel, runtime, nil
}

// Committed model override of the executing admission, nil for any other turn
func turnModelOverride(ctx context.Context, t *TurnRunner) *pyjson.Object {
	var inbound any
	if _, inboundID, _ := t.runner.turnReplyAnchors(t.ctx); inboundID != nil {
		inbound = *inboundID
	}
	override, _ := gateway.CLITurn(ctx, inbound).GetOr("model_override", nil).(*pyjson.Object)
	if override == nil || override.Len() == 0 {
		return nil
	}
	return override
}

// Resolved route with the executing admission's model override applied, unchanged without one
func oneTurnRoute(ctx context.Context, t *TurnRunner, policy *gateway.LocalSessionPolicy, model string, runtimeKwargs map[string]any) (string, map[string]any, error) {
	override := turnModelOverride(ctx, t)
	if override == nil {
		return model, runtimeKwargs, nil
	}
	cfg := t.ctx.userConfig
	if policy != nil {
		authority, err := gateway.ActiveAuthority(ctx, t.runner)
		if err != nil {
			return "", nil, err
		}
		effective, _, err := policy.EffectiveFor(ctx, authority, true)
		if err != nil {
			return "", nil, err
		}
		cfg = effective
	}
	return oneTurnRuntime(ctx, override, model, runtimeKwargs, cfg)
}

// Receipt only fields of a classic CLI turn, pending steer removed from the raw result
func takeCLIResultFields(ctx context.Context, result map[string]any, auxBefore *pyjson.Object, db *state.SessionDB, sessionID string) map[string]any {
	if result == nil {
		return map[string]any{}
	}
	fields := map[string]any{}
	for _, key := range cliResultPassthrough {
		if v, ok := result[key]; ok {
			fields[key] = v
		}
	}
	finalResponse := result["final_response"]
	if !pyrepr.Truthy(finalResponse) {
		finalResponse = ""
	}
	fields["final_response"] = finalResponse
	steer := result["pending_steer"]
	delete(result, "pending_steer")
	if !pyrepr.Truthy(steer) {
		steer = nil
	}
	fields["pending_steer"] = steer
	if auxBefore != nil {
		holderSession := result["session_id"]
		if !pyrepr.Truthy(holderSession) {
			holderSession = sessionID
		}
		holder := map[string]any{"session_id": holderSession}
		agent.AttachAuxiliaryUsage(ctx, holder, db, auxBefore, sessionID)
		usage := holder["auxiliary_usage"]
		if !pyrepr.Truthy(usage) {
			usage = pyjson.NewObject()
		}
		fields["auxiliary_usage"] = usage
	}
	return fields
}

// Runs the turn's conversation, keeping a classic CLI turn's receipt only fields on the turn
func runClassicCLITurn(ctx context.Context, t *TurnRunner, run func() (map[string]any, error)) (map[string]any, error) {
	classic, err := classicCLITurn(ctx, t)
	if err != nil {
		return nil, err
	}
	if !classic {
		return run()
	}
	var auxBefore *pyjson.Object
	var cliDB *state.SessionDB
	if required, ok := gateway.FiniteTurnRequired(ctx).(bool); ok && required {
		if runnerDB := t.runner.sessionDB(ctx); runnerDB != nil {
			cliDB = runnerDB.DB()
		}
		auxBefore = agent.OneshotAuxiliaryUsage(ctx, cliDB, t.ctx.sessionID)
	}
	result, err := run()
	if err != nil {
		return nil, err
	}
	t.cliResultFields = takeCLIResultFields(ctx, result, auxBefore, cliDB, t.ctx.sessionID)
	return result, nil
}
