package provider

import (
	"context"
	"math"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Positive Python float of a timeout value, false for anything float rejects or that is not above zero
func coerceProviderTimeout(raw any) (float64, bool) {
	f, err := pyint.Float(raw)
	if err != nil || !(f > 0) {
		return 0, false
	}
	return f, true
}

// Per model timeout over the provider wide key from the providers block, in seconds
func configuredProviderSeconds(providers map[string]any, provider, model, modelKey, providerKey string) (float64, bool) {
	if provider == "" || providers == nil {
		return 0, false
	}
	pc, ok := providers[provider].(map[string]any)
	if !ok {
		return 0, false
	}
	if model != "" {
		models, _ := pc["models"].(map[string]any)
		if mc, ok := models[model].(map[string]any); ok {
			if f, ok := coerceProviderTimeout(mc[modelKey]); ok {
				return f, true
			}
		}
	}
	return coerceProviderTimeout(pc[providerKey])
}

// Configured provider request timeout in seconds from the home config
func GetProviderRequestTimeout(ctx context.Context, providerID, model string) (float64, bool) {
	return configuredProviderSeconds(loadWireSettings(ctx).providers, providerID, model, "timeout_seconds", "request_timeout_seconds")
}

// Configured provider stale timeout in seconds from the home config
func GetProviderStaleTimeout(ctx context.Context, providerID, model string) (float64, bool) {
	return configuredProviderSeconds(loadWireSettings(ctx).providers, providerID, model, "stale_timeout_seconds", "stale_timeout_seconds")
}

// Stream stale base from the environment or 180s, and whether the environment set it
func streamEnvStaleBase() (float64, bool) {
	_, explicit := os.LookupEnv(StreamStaleTimeoutEnv)
	return envFloat(StreamStaleTimeoutEnv, defaultStreamStaleTimeout), explicit
}

// Cloud stale patience scaled for context size and floored for known reasoning models
func cloudStaleTimeout(ctx context.Context, base float64, kwargs map[string]any) float64 {
	timeout := scaleStaleTimeoutForContext(base, EstimateRequestContextTokens(ctx, kwargs))
	model := stringOf(kwargs["model"])
	if model == "" {
		model = stringOf(kwargs["modelId"])
	}
	floor, ok := 0.0, false
	if d, found := ReasoningStaleTimeoutFloor(model); found {
		floor, ok = d.Seconds(), true
	}
	if !ok && truthy(kwargs["modelId"]) {
		floor, ok = bedrockReasoningStaleFloor(kwargs["modelId"])
	}
	if ok && floor > timeout {
		return floor
	}
	return timeout
}

// RunBudgetAgent is an agent carrying a wall clock run budget and the epoch second its turn clock started
type RunBudgetAgent interface {
	RunBudget() (seconds, startedAt float64)
}

// CapToRunBudget caps an implicit stale timeout at half the remaining run budget, never under sixty seconds
func CapToRunBudget(agent any, timeout float64) float64 {
	budgeted, ok := agent.(RunBudgetAgent)
	if !ok {
		return timeout
	}
	budget, started := budgeted.RunBudget()
	if budget == 0 || started == 0 {
		return timeout
	}
	remaining := budget - (float64(time.Now().UnixNano())/1e9 - started)
	return math.Min(timeout, math.Max(60.0, remaining*0.5))
}

// Explicit provider stale timeout, else the context scaled and reasoning floored default for a route without a run budget
func cloudStaleTimeoutFor(ctx context.Context, provider, model string, kwargs map[string]any) float64 {
	return cloudStaleTimeoutForAgent(ctx, nil, provider, model, kwargs)
}

// Explicit provider stale timeout, else the scaled and floored default capped by the agent run budget unless the env set it
func cloudStaleTimeoutForAgent(ctx context.Context, agent any, provider, model string, kwargs map[string]any) float64 {
	if explicit, ok := GetProviderStaleTimeout(ctx, provider, model); ok {
		return explicit
	}
	base, explicitEnv := streamEnvStaleBase()
	timeout := cloudStaleTimeout(ctx, base, kwargs)
	if explicitEnv {
		return timeout
	}
	return CapToRunBudget(agent, timeout)
}

// Replaces old with new wherever it sits between two decimal digits
func swapDigitSeparator(s string, old, repl rune) string {
	rs := []rune(s)
	for i := 1; i+1 < len(rs); i++ {
		if rs[i] == old && unicode.IsDigit(rs[i-1]) && unicode.IsDigit(rs[i+1]) {
			rs[i] = repl
		}
	}
	return string(rs)
}

// Reasoning stale floor of a Bedrock inference profile id tried without region and namespace, dashed and dotted
func bedrockReasoningStaleFloor(modelID any) (float64, bool) {
	id, ok := modelID.(string)
	if !ok || id == "" {
		return 0, false
	}
	name := pystr.Lower(pystr.Strip(id))
	for _, prefix := range bedrockRegionPrefixes {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
			break
		}
	}
	bases := []string{name}
	if i := strings.LastIndex(name, "."); i >= 0 {
		bases = append(bases, name[i+1:], strings.Replace(name, ".", "-", 1))
	}
	seen := map[string]bool{}
	for _, cand := range bases {
		for _, form := range []string{cand, swapDigitSeparator(cand, '-', '.'), swapDigitSeparator(cand, '.', '-')} {
			if seen[form] {
				continue
			}
			seen[form] = true
			if d, found := ReasoningStaleTimeoutFloor(form); found {
				return d.Seconds(), true
			}
		}
	}
	return 0, false
}
