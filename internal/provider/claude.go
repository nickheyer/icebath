package provider

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Thinking budget of each effort for the models that take a manual budget
var budgets = map[string]int{"xhigh": 32000, "high": 16000, "medium": 8000, "low": 4000}

// Efforts as adaptive thinking's output_config.effort names them
var adaptiveEfforts = map[string]string{"ultra": "max", "max": "max", "xhigh": "xhigh", "high": "high", "medium": "medium", "low": "low", "minimal": "low"}

// Claude families by what they take of thinking
var (
	manualThinking = []string{"claude-3", "claude-opus-4-0", "claude-opus-4.0", "claude-opus-4-1", "claude-opus-4.1", "claude-sonnet-4-0",
		"claude-sonnet-4.0", "claude-opus-4-2025", "claude-sonnet-4-2025", "claude-opus-4-5", "claude-opus-4.5", "claude-sonnet-4-5", "claude-sonnet-4.5",
		"claude-haiku-4-5", "claude-haiku-4.5"}
	noXHigh            = []string{"claude-opus-4-6", "claude-opus-4.6", "claude-sonnet-4-6", "claude-sonnet-4.6"}
	mandatoryThinking  = []string{"claude-fable", "claude-mythos", "claude-opus-5-5", "claude-opus-5.5"}
	betweenToolsClaude = []string{"claude-sonnet-5-5", "claude-sonnet-5.5"}
)

// Whether the model is a Claude model
func Claude(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}

func familyOf(model string, lists ...[]string) bool {
	m := strings.ToLower(model)
	return slices.ContainsFunc(lists, func(list []string) bool { return Contains(m, list...) })
}

// Adaptive thinking: Kimi models, and Claude models past the manual budget generations, unknown ones included
func adaptive(model string) bool {
	return KimiModel(model) || Claude(model) && !familyOf(model, manualThinking)
}

func xhigh(model string) bool {
	return adaptive(model) && !familyOf(model, noXHigh)
}

// Claude models with adaptive thinking that take thinking turned off
func disables(model string) bool {
	return Claude(model) && adaptive(model) && !familyOf(model, mandatoryThinking, betweenToolsClaude)
}

// Whether the model is a Claude model that always thinks, which no request can switch off
func MandatoryThinking(model string) bool {
	return Claude(model) && familyOf(model, mandatoryThinking)
}

// Haiku generations before 5.5, which are never asked to think
func preThinkingHaiku(model string) bool {
	if !strings.Contains(strings.ToLower(model), "haiku") {
		return false
	}
	family, version, ok := ClaudeFamily(model)
	return !ok || family != "haiku" || !AtLeast(version, [2]int{5, 5})
}

// Prefixes of Kimi and Moonshot model ids
var kimiPrefixes = []string{"kimi-", "kimi_", "moonshot-", "moonshot_", "k1.", "k1-", "k2.", "k2-", "k25", "k2.5", "k3.", "k3-"}

// Whether the model is one of Kimi's or Moonshot's
func KimiModel(model string) bool {
	m := BareModel(model)
	return m == "k3" || m != "" && HasPrefix(m, kimiPrefixes...)
}

var claudeVersions = regexp.MustCompile(`(?i)claude[-_.](opus|sonnet|haiku|fable|mythos)[-_.](\d+)(?:[-_.](\d{1,2})(?:$|[-_.]))?`)

// Family and version of a Claude model id
func ClaudeFamily(model string) (string, [2]int, bool) {
	m := claudeVersions.FindStringSubmatch(strings.TrimSpace(model))
	if m == nil {
		return "", [2]int{}, false
	}
	major, _ := strconv.Atoi(m[2])
	minor, _ := strconv.Atoi(m[3])
	return strings.ToLower(m[1]), [2]int{major, minor}, true
}

// Whether a version is the floor or later
func AtLeast(v, floor [2]int) bool {
	return v[0] > floor[0] || v[0] == floor[0] && v[1] >= floor[1]
}

// Fields of an Anthropic Messages request that ask for the thinking the ask wants of a Claude or Kimi model: adaptive thinking at an effort,
// a manual budget with the temperature and output room it needs, thinking off where the model takes it, nothing when it asks nothing
func ClaudeThinking(ask Ask, model string, maxTokens int) map[string]any {
	out := map[string]any{}
	if !ask.Set {
		return out
	}
	if ask.Off {
		switch {
		case familyOf(model, betweenToolsClaude):
			out["thinking"] = map[string]any{"type": "between_tools"}
		case disables(model):
			out["thinking"] = map[string]any{"type": "disabled"}
		}
		return out
	}
	if preThinkingHaiku(model) {
		return out
	}
	effort := strings.ToLower(ask.Effort)
	if effort == "" {
		effort = "medium"
	}
	if adaptive(model) {
		level, ok := adaptiveEfforts[effort]
		if !ok {
			level = "medium"
		}
		if level == "xhigh" && !xhigh(model) {
			level = "max"
		}
		out["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		out["output_config"] = map[string]any{"effort": level}
		return out
	}
	budget, ok := budgets[effort]
	if !ok {
		budget = 8000
	}
	out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	out["temperature"] = 1
	out["max_tokens"] = max(maxTokens, budget+4096)
	return out
}
