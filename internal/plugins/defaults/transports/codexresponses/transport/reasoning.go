package transport

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

var effortLadder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

var codexLegacyEfforts = []string{"none", "low", "medium", "high", "xhigh"}
var codexAstraEfforts = []string{"low", "medium", "high", "xhigh", "max"}
var codexGPT56Efforts = []string{"none", "low", "medium", "high", "xhigh", "max"}
var astraModelIDs = map[string]bool{"gpt-6-astra": true, "gpt-6-astra-900k": true}
var gpt6TierPrefixes = []string{"gpt-6-sol", "gpt-6-luna"}
var noDisableTierPrefixes = []string{"gpt-6.1-sol"}
var daybreakModelIDs = map[string]bool{"gpt-daybreak-blue-latest": true, "gpt-daybreak-blue-latest-900k": true}
var xaiGrok46Efforts = []string{"low", "medium", "high", "xhigh"}
var xaiLegacyEfforts = []string{"low", "medium", "high"}

// Grok ids that accept the effort dial
var grokEffortCapablePrefixes = []string{"grok-3-mini", "grok-4.20-multi-agent", "grok-4.3", "grok-4.5", "grok-4.6"}

func bareModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

func hasAnyPrefix(s string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}

func isAstraModel(model string) bool {
	return astraModelIDs[bareModel(model)]
}

// Exact api.openai.com host only
func isOpenAIAPIOrigin(baseURL string) bool {
	return provider.BaseHost(baseURL) == "api.openai.com"
}

func isXAI(baseURL string) bool {
	return provider.BaseHostMatches(baseURL, "x.ai")
}

// True for chat era ids that 400 on any reasoning field: gpt-3.5, gpt-4 with no digit after it, chatgpt-, each also behind ft:
func openAIModelRejectsReasoning(model string) bool {
	name := strings.TrimPrefix(bareModel(model), "ft:")
	if rest, ok := strings.CutPrefix(name, "gpt-4"); ok {
		return rest == "" || rest[0] < '0' || rest[0] > '9'
	}
	return strings.HasPrefix(name, "gpt-3.5") || strings.HasPrefix(name, "chatgpt-")
}

func isGrok46Family(model string) bool {
	name := bareModel(strings.ReplaceAll(model, "_", "-"))
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

func grokSupportsReasoningEffort(model string) bool {
	name := bareModel(model)
	return name != "" && hasAnyPrefix(name, grokEffortCapablePrefixes)
}

// Effort vocabulary of an OpenAI or Codex Responses model generation
func codexSupportedEfforts(model string) []string {
	bare := bareModel(model)
	if isAstraModel(model) || hasAnyPrefix(bare, noDisableTierPrefixes) {
		return codexAstraEfforts
	}
	if strings.Contains(bare, "gpt-5.6") || hasAnyPrefix(bare, gpt6TierPrefixes) || daybreakModelIDs[bare] {
		return codexGPT56Efforts
	}
	return codexLegacyEfforts
}

// Effort vocabulary of a Responses route, empty when the model takes no reasoning field
func codexEffortsForRoute(model, baseURL string) []string {
	if isOpenAIAPIOrigin(baseURL) && openAIModelRejectsReasoning(model) {
		return nil
	}
	if isAstraModel(model) && !isOpenAIAPIOrigin(baseURL) {
		return codexLegacyEfforts
	}
	return codexSupportedEfforts(model)
}

// Nearest weaker supported effort, the request verbatim when supported or outside the ladder
func clampEffort(effort string, supported []string) string {
	requested := strings.ToLower(strings.TrimSpace(effort))
	ri := slices.Index(effortLadder, requested)
	if requested == "" || len(supported) == 0 || slices.Contains(supported, requested) || ri < 0 {
		return effort
	}
	best, bestIdx := "", -1
	minLevel, minIdx := "", len(effortLadder)
	for _, l := range supported {
		idx := slices.Index(effortLadder, l)
		if l == "none" || idx < 0 {
			continue
		}
		if idx < ri && idx > bestIdx {
			best, bestIdx = l, idx
		}
		if idx < minIdx {
			minLevel, minIdx = l, idx
		}
	}
	switch {
	case best != "":
		return best
	case minLevel != "":
		return minLevel
	}
	return effort
}

// Effort a request reasons at and whether reasoning is on: medium unless asked otherwise, off when asked for none or the route takes no reasoning field
func resolveReasoning(model, baseURL, asked string) (string, bool) {
	effort := "medium"
	if level := strings.ToLower(strings.TrimSpace(asked)); level != "" {
		effort = level
	}
	supported := codexEffortsForRoute(model, baseURL)
	if isXAI(baseURL) {
		if supported = xaiLegacyEfforts; isGrok46Family(model) {
			supported = xaiGrok46Efforts
		}
	}
	if len(supported) == 0 {
		return "", false
	}
	if effort == "none" {
		if slices.Contains(supported, "none") {
			return "none", false
		}
		return "", false
	}
	return clampEffort(effort, supported), true
}

// Reasoning and include fields of a request body
func reasoningFields(model, baseURL, asked string) map[string]any {
	effort, enabled := resolveReasoning(model, baseURL, asked)
	include := []any{"reasoning.encrypted_content"}
	fields := map[string]any{}
	switch {
	case enabled && isXAI(baseURL):
		fields["include"] = include
		if grokSupportsReasoningEffort(model) {
			fields["reasoning"] = map[string]any{"effort": effort}
		}
	case enabled:
		fields["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
		fields["include"] = include
	case !isXAI(baseURL):
		fields["include"] = []any{}
		if effort == "none" {
			fields["reasoning"] = map[string]any{"effort": "none"}
		}
	}
	return fields
}
