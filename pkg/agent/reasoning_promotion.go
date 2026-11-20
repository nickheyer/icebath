package agent

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	answerInReasoningCap      = "answer_in_reasoning"
	nemotronParserModelMarker = "nemotron-3.5-lightning"
)

// Custom provider entries of the agent as dicts, nil when the agent carries none
func agentCustomProviderEntries(a *AIAgent) []map[string]any {
	if a.customProviders == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(a.customProviders))
	for _, p := range a.customProviders {
		if m, ok := p.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// True when the live route may return a clean stop reasoning payload as the answer
func answerInReasoningCapability(ctx context.Context, a *AIAgent) bool {
	baseURL := a.baseURL
	prov := strings.ToLower(pystr.Strip(a.provider))
	if hostMatches, _ := pyurl.BaseURLHostMatches(baseURL, "openrouter.ai"); prov == "openrouter" || hostMatches {
		return false
	}
	if mode := strings.ToLower(pystr.Strip(a.apiMode)); mode != "" && mode != "chat_completions" {
		return false
	}
	model := a.model
	customProviders := agentCustomProviderEntries(a)
	var cfg map[string]any
	loaded := true
	if customProviders == nil {
		tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
		if loaded = err == nil; loaded {
			cfg = map[string]any(tree)
		}
	}
	if loaded {
		if v, known := provider.CustomProviderModelCapability(model, baseURL, answerInReasoningCap, customProviders, cfg); known {
			return v
		}
	}
	requested := strings.ToLower(pystr.Strip(a.requestedProvider))
	ids := map[string]bool{}
	for _, p := range []string{prov, requested} {
		if strings.HasPrefix(p, "custom:") {
			ids[provider.CustomProviderSlug(p, "")] = true
		}
	}
	entries := provider.EntriesForRoute(baseURL, customProviders, cfg)
	var live []map[string]any
	for _, e := range entries {
		name, _ := e["name"].(string)
		key, _ := e["provider_key"].(string)
		if ids[provider.CustomProviderSlug(name, key)] {
			live = append(live, e)
		}
	}
	if len(live) == 0 {
		live = entries
	}
	hasTrue, hasFalse := false, false
	for _, e := range live {
		caps, _ := e["capabilities"].(map[string]any)
		if v, ok := caps[answerInReasoningCap].(bool); ok {
			hasTrue, hasFalse = hasTrue || v, hasFalse || !v
		}
	}
	if hasTrue || hasFalse {
		return !hasFalse
	}
	if !strings.Contains(strings.ToLower(model), nemotronParserModelMarker) {
		return false
	}
	return provider.IsLocalEndpoint(baseURL)
}
