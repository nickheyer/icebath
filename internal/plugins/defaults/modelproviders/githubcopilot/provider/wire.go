package provider

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Host family of the Copilot API: the individual, business and enterprise subdomains
const copilotHost = "githubcopilot.com"

// Hosts whose first request of a turn the person started says so in x-initiator, and GitHub Models' free tier
var (
	initiatorHosts = []string{"api.githubcopilot.com", "models.github.ai"}
	freeTierHost   = "models.inference.ai.azure.com"
)

// What GitHub Models' free tier can't serve
var freeTierHint = []string{
	"   💡 GitHub Models free tier (models.inference.ai.azure.com) caps every",
	"      request at ~8K tokens. Icebath's system prompt + tool schemas baseline",
	"      exceeds that floor, so this endpoint cannot run an agentic loop.",
	"      Use the `copilot` provider with a Copilot subscription token (`icebath",
	"      auth add copilot`), or pick any other provider.",
}

// OpenRouter style and dashed ids mapped onto the ids Copilot serves them under
var aliases = map[string]string{
	"openai/gpt-5": "gpt-5-mini", "openai/gpt-5-chat": "gpt-5-mini", "openai/gpt-5-mini": "gpt-5-mini",
	"openai/gpt-5-nano": "gpt-5-mini", "openai/gpt-4.1": "gpt-4.1", "openai/gpt-4.1-mini": "gpt-4.1",
	"openai/gpt-4.1-nano": "gpt-4.1", "openai/gpt-4o": "gpt-4o", "openai/gpt-4o-mini": "gpt-4o-mini",
	"openai/o1": "gpt-5.2", "openai/o1-mini": "gpt-5-mini", "openai/o1-preview": "gpt-5.2",
	"openai/o3": "gpt-5.3-codex", "openai/o3-mini": "gpt-5-mini", "openai/o4-mini": "gpt-5-mini",
	"anthropic/claude-opus-4.6": "claude-opus-4.6", "anthropic/claude-sonnet-5": "claude-sonnet-5",
	"anthropic/claude-sonnet-4.6": "claude-sonnet-4.6", "anthropic/claude-sonnet-4": "claude-sonnet-4",
	"anthropic/claude-sonnet-4.5": "claude-sonnet-4.5", "anthropic/claude-haiku-4.5": "claude-haiku-4.5",
	"claude-sonnet-5": "claude-sonnet-5", "claude-opus-4-6": "claude-opus-4.6",
	"claude-sonnet-4-6": "claude-sonnet-4.6", "claude-sonnet-4-0": "claude-sonnet-4",
	"claude-sonnet-4-5": "claude-sonnet-4.5", "claude-haiku-4-5": "claude-haiku-4.5",
	"anthropic/claude-opus-4-6": "claude-opus-4.6", "anthropic/claude-sonnet-4-6": "claude-sonnet-4.6",
	"anthropic/claude-sonnet-4-0": "claude-sonnet-4", "anthropic/claude-sonnet-4-5": "claude-sonnet-4.5",
	"anthropic/claude-haiku-4-5": "claude-haiku-4.5",
}

// Efforts of Copilot's model families as their ids tell
var (
	gpt5Efforts    = []string{"minimal", "low", "medium", "high"}
	oSeriesEfforts = []string{"low", "medium", "high"}
	astraEfforts   = []string{"low", "medium", "high", "xhigh", "max"}
)

// Model id as Copilot serves it, from its aliases
func native(_ provider.Spec, model string) string {
	return Native(model, nil)
}

func copilot(base string) bool {
	return provider.BaseHostMatches(base, copilotHost)
}

// Copilot's own id of a model: its alias, else the form the account's catalog lists, else the id without its vendor
func Native(model string, catalog []string) string {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return ""
	}
	if alias := aliases[raw]; alias != "" {
		return alias
	}
	candidates := []string{raw}
	if _, bare, ok := strings.Cut(raw, "/"); ok {
		candidates = append(candidates, strings.TrimSpace(bare))
	}
	for _, suffix := range []string{"-mini", "-nano", "-chat"} {
		if strings.HasSuffix(raw, suffix) {
			candidates = append(candidates, strings.TrimSuffix(raw, suffix))
		}
	}
	for _, candidate := range candidates {
		if alias, ok := aliases[candidate]; ok && candidate != "" {
			return alias
		}
		if candidate != "" && slices.Contains(catalog, candidate) {
			return candidate
		}
	}
	if _, bare, ok := strings.Cut(raw, "/"); ok {
		if bare = strings.TrimSpace(bare); bare != "" && !strings.Contains(bare, "/") {
			return bare
		}
	}
	return raw
}

// Efforts a Copilot model's id says it takes: the o series, Astra and GPT-5
func Efforts(model string) []string {
	raw := strings.ToLower(strings.TrimSpace(model))
	if provider.HasPrefix(raw, "openai/o1", "openai/o3", "openai/o4", "o1", "o3", "o4") {
		return slices.Clone(oSeriesEfforts)
	}
	normalized := strings.ToLower(Native(model, nil))
	switch {
	case provider.Astra(normalized):
		return slices.Clone(astraEfforts)
	case strings.HasPrefix(normalized, "gpt-5"):
		return slices.Clone(gpt5Efforts)
	}
	return nil
}

// Effort of the ask on the model's list: the nearest weaker listed level, else medium or the first listed
func clampEffort(effort string, supported []string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		effort = "medium"
	}
	if slices.Contains(supported, effort) {
		return effort
	}
	if effort = provider.ClampEffort(effort, supported, nil); slices.Contains(supported, effort) {
		return effort
	}
	if slices.Contains(supported, "medium") {
		return "medium"
	}
	return supported[0]
}

// Reasoning of a model the catalog says takes it, medium unasked, nothing when thinking is off
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if payload := reasoningPayload(call); payload != nil {
		return map[string]any{"reasoning": payload}, nil
	}
	return nil, nil
}

// Effort of a model whose efforts the catalog lists, nil for one it lists none for or with thinking off
func reasoningPayload(call *provider.Call) map[string]any {
	if !call.Thinking.Takes || len(call.Thinking.Efforts) == 0 || call.Model == "" || call.Ask.Set && call.Ask.Off {
		return nil
	}
	return map[string]any{"effort": clampEffort(call.Ask.Effort, call.Thinking.Efforts)}
}

// Pictures marked as such, and the first request of a turn the person started marked as theirs, on Copilot's hosts
func headers(call *provider.Call) map[string]string {
	h := map[string]string{}
	if call.Images && copilot(call.Route.BaseURL) {
		h["Copilot-Vision-Request"] = "true"
	}
	if call.UserTurn && slices.ContainsFunc(initiatorHosts, func(host string) bool { return provider.BaseHostMatches(call.Route.BaseURL, host) }) {
		h["x-initiator"] = "user"
	}
	return h
}

// max_completion_tokens on a Copilot subdomain
func outputField(base, _ string) string {
	if strings.HasSuffix(provider.BaseHost(base), "."+copilotHost) {
		return "max_completion_tokens"
	}
	return ""
}

// The opaque reasoning a reply carried, with its text, replayed on Copilot's hosts only
func replay(call *provider.Call, kept map[string]string) map[string]string {
	if !copilot(call.Route.BaseURL) || kept["reasoning_opaque"] == "" {
		return nil
	}
	out := map[string]string{"reasoning_opaque": kept["reasoning_opaque"]}
	if text := kept["reasoning_text"]; text != "" {
		out["reasoning_text"] = text
	}
	return out
}

// Copilot's Gemini models take the thought signatures of their functions back on its own hosts
func functionSignatures(_, base string) bool {
	return copilot(base)
}

// What GitHub Models' free tier can't take when it refuses a request for its size
func rejectionHint(status int, base, _ string) []string {
	if status == 413 && provider.BaseHostMatches(base, freeTierHost) {
		return slices.Clone(freeTierHint)
	}
	return nil
}

func refreshNotice(reason string) string {
	switch reason {
	case "stale":
		return "Copilot credential re-exchanged after model_not_available 400. Retrying request..."
	case "unauthorized":
		return "Copilot credentials refreshed after 401. Retrying request..."
	}
	return ""
}
