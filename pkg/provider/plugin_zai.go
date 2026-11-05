package provider

import (
	"math/big"
	"strings"
	"unicode"
)

var (
	glm53Tokens = []string{"glm-5.3", "glm-5-3", "glm-5p3"}
	glm52Tokens = append([]string{"glm-5.2", "glm-5-2", "glm-5p2"}, glm53Tokens...)
)

// Profiles the bundled zai plugin registers
func zaiPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "zai", display: "Z.AI (GLM)", desc: "Z.AI / GLM — Zhipu AI models",
		signup: "https://z.ai/", keys: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, base: "https://api.z.ai/api/paas/v4",
		fallbacks: []string{"glm-5.2", "glm-5", "glm-4-9b"},
	}, "glm", "z-ai", "z.ai", "zhipu")
	p.DefaultAuxModel = "glm-4.5-flash"
	p.BuildAPIKwargsExtrasHook = zaiBuildAPIKwargsExtras
	return []Profile{p}
}

// Integer value of a run of Unicode decimal digits the way Python int parses them
func unicodeDigitsInt(s string) *big.Int {
	n := new(big.Int)
	for _, r := range s {
		start := r
		for start > 0 && unicode.IsDigit(start-1) {
			start--
		}
		d := int64((r - start) % 10)
		n.Mul(n, big.NewInt(10)).Add(n, big.NewInt(d))
	}
	return n
}

// True when the stripped lowercase model contains any token
func zaiModelHasToken(model string, tokens []string) bool {
	return containsAnySub(strings.ToLower(strings.TrimSpace(model)), tokens...)
}

// Effort mapped onto the GLM 5.2 or 5.3 vocabulary, below floor efforts clamping to the floor and off leaving the server default
func zaiGLM52ReasoningEffort(reasoning *Reasoning, model string) string {
	effort := requestedEffort(hookReasoningCfg(reasoning))
	if effort == "" || effort == "none" {
		return ""
	}
	efforts, overrides, floor := glm52Efforts, glm52Overrides, "high"
	if zaiModelHasToken(model, glm53Tokens) {
		efforts, overrides, floor = glm53Efforts, glm53Overrides, "low"
	}
	if clamped := clampEffort(effort, efforts, overrides); containsStr(efforts, clamped) {
		return clamped
	}
	return floor
}

// Thinking toggle in the extra body plus the GLM 5.2 native effort, 5.3 never accepting disabled thinking
func zaiBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	model := hookString(kw, "model")
	extra, top := map[string]any{}, map[string]any{}
	is52 := zaiModelHasToken(model, glm52Tokens)
	if !glmSupportsThinking(model) && !is52 {
		return extra, top
	}
	if reasoning != nil {
		enabled := reasoning.Enabled == nil || *reasoning.Enabled
		if !enabled && zaiModelHasToken(model, glm53Tokens) {
			enabled = true
			top["reasoning_effort"] = "low"
		}
		if enabled {
			extra["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
		} else {
			extra["thinking"] = map[string]any{"type": "disabled"}
		}
	}
	if is52 {
		if effort := zaiGLM52ReasoningEffort(reasoning, model); effort != "" {
			top["reasoning_effort"] = effort
		}
	}
	return extra, top
}
