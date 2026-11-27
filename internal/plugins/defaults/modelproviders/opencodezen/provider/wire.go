package provider

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"

	opencodezenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Most output of the Go models whose cap is above the default, by id
var goCaps = map[string]int{"mimo-v2.5-pro": 131072}

// Efforts of the free x-preview model, with the level a missing one maps to; the efforts of Kimi K2
var (
	previewEfforts   = []string{"low", "high", "max"}
	previewOverrides = map[string]string{"xhigh": "max"}
	k2Efforts        = []string{"low", "medium", "high"}
	thinkingModels   = []string{"deepseek-reasoner", "deepseek-flash"}
)

// Lower case model id without its vendor
func flat(model string) string {
	m := strings.TrimSpace(model)
	return strings.ToLower(m[strings.LastIndex(m, "/")+1:])
}

// Effort of the free x-preview model, low, high or max only
func zenShape(call *provider.Call) (map[string]any, map[string]any) {
	if flat(call.Model) != "x-preview-f-free" {
		return nil, nil
	}
	effort := call.Ask.Requested()
	if effort == "none" {
		effort = ""
	}
	if clamped := provider.ClampEffort(effort, previewEfforts, previewOverrides); effort != "" && slices.Contains(previewEfforts, clamped) {
		return nil, map[string]any{"reasoning_effort": clamped}
	}
	return nil, nil
}

// GLM 5.2's effort, Kimi K2's thinking toggle and DeepSeek's, by the model's family
func goShape(call *provider.Call) (map[string]any, map[string]any) {
	model := flat(call.Model)
	switch {
	case provider.Contains(model, provider.GLM52Tokens...):
		effort := call.Ask.Requested()
		if effort == "" || effort == "none" {
			return nil, nil
		}
		clamped := provider.ClampEffort(effort, provider.GLM52Efforts, provider.GLM52Overrides)
		if !slices.Contains(provider.GLM52Efforts, clamped) {
			clamped = "high"
		}
		return nil, map[string]any{"reasoning_effort": clamped}
	case strings.HasPrefix(model, "kimi-k2"):
		if !call.Ask.Set {
			return nil, nil
		}
		return provider.ThinkingToggle(call.Ask, k2Efforts, nil, false)
	case strings.HasPrefix(model, "deepseek-v") && !strings.HasPrefix(model, "deepseek-v3") || slices.Contains(thinkingModels, model):
		return provider.ThinkingToggle(call.Ask, provider.DeepSeekEfforts, provider.DeepSeekOverrides, false)
	}
	return nil, nil
}

// Output cap of a Go model: the one asked, else the model's own, else the kind's default
func goCap(call *provider.Call, requested int) int {
	if requested > 0 {
		return requested
	}
	return goCaps[flat(call.Model)]
}

// Session OpenCode keeps a conversation's requests together under, a one off key for a request of no session
func session(call *provider.Call) map[string]string {
	key := call.Scope
	if key == "" {
		var random [8]byte
		_, _ = rand.Read(random[:])
		key = "oneshot-" + hex.EncodeToString(random[:])
	}
	return map[string]string{"x-opencode-session": key}
}

// Model id as OpenCode names it: without its vendor, a Claude id on Zen with dashes for its dots
func native(spec provider.Spec, model string) string {
	if _, bare, ok := strings.Cut(model, "/"); ok && strings.TrimSpace(bare) != "" {
		model = strings.TrimSpace(bare)
	}
	if spec.Name == opencodezenconfig.ZenName && strings.HasPrefix(strings.ToLower(model), "claude-") {
		return strings.ReplaceAll(model, ".", "-")
	}
	return model
}

// Prompt caching of Qwen models on the message shape
func caching(model string, _ provider.APIMode) (bool, bool, bool) {
	if strings.Contains(strings.ToLower(model), "qwen") {
		return true, false, true
	}
	return false, false, false
}

// Zen keeps a prompt cache for five minutes
func zenTTL(string) string { return "5m" }

// Go keeps one for an hour, except for MiniMax M2.5, which it keeps five minutes
func goTTL(model string) string {
	if provider.BareModel(model) == "minimax-m2.5" {
		return "5m"
	}
	return "1h"
}

// Names OpenCode's backends keep for their own tools
var reserved = []string{"web_search", "search_files"}
