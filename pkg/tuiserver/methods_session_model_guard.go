package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Stripped str of a param or the empty string when it is falsy
func paramText(params map[string]any, key string) string {
	return pystr.Strip(pyrepr.Str(orEmpty(params[key])))
}

// Conflict record of the create params' model override, nil when coherent or undecidable
func modelOverrideConflict(ctx context.Context, params map[string]any, buildScope func(context.Context) context.Context) (map[string]any, error) {
	model := paramText(params, "model")
	if model == "" {
		return nil, nil
	}
	providerName := paramText(params, "provider")
	if providerName == "" {
		scoped := buildScope(ctx)
		resolved, err := provider.ResolveRequestedProvider(scoped, home.Dir(scoped), "")
		if err != nil {
			return nil, err
		}
		providerName = resolved
	}
	conflict, found := provider.StaticModelProviderConflict(ctx, model, providerName, 5)
	if !found {
		return nil, nil
	}
	suggestions := make([]any, len(conflict.Suggestions))
	for i, s := range conflict.Suggestions {
		suggestions[i] = s
	}
	return map[string]any{"model": conflict.Model, "provider": conflict.Provider, "suggestions": suggestions, "message": conflict.Message}, nil
}

// Per session model, reasoning and service tier overrides of the composer, an unknown tier word an error
func createOverrides(params map[string]any) (modelOverride, reasoningOverride map[string]any, serviceTier *string, err error) {
	if model := paramText(params, "model"); model != "" {
		var providerOverride any
		if p := paramText(params, "provider"); p != "" {
			providerOverride = p
		}
		modelOverride = map[string]any{"model": model, "provider": providerOverride}
	}
	if effort := paramText(params, "reasoning_effort"); effort != "" {
		reasoningOverride = parseEffortQuietly(effort)
	}
	if raw, present := params["service_tier"]; present && raw != nil {
		tier, perr := provider.ParseExactServiceTier(pyrepr.Str(orEmpty(raw)))
		if perr != nil {
			return nil, nil, nil, perr
		}
		serviceTier = &tier
	} else if _, present := params["fast"]; present {
		tier := ""
		if provider.IsTruthyValue(params["fast"]) {
			tier = "priority"
		}
		serviceTier = &tier
	}
	return modelOverride, reasoningOverride, serviceTier, nil
}

// Parsed reasoning effort, nil when the word cannot be parsed
func parseEffortQuietly(effort string) (parsed map[string]any) {
	defer func() {
		if recover() != nil {
			parsed = nil
		}
	}()
	return config.ParseReasoningEffort(effort)
}
