package wire

import (
	"encoding/json"
	"slices"
	"strings"
)

// Reasoning levels one wire accepts, with the rounding its vendor declares for levels it lacks
type Efforts struct {
	Levels []string
	Round  map[string]string
}

// Named reasoning vocabulary of a wire or vendor family
type EffortFamily uint8

const (
	EffortsOpenAICompat EffortFamily = iota + 1
	EffortsCodexGPT56
	EffortsCodexLegacy
	EffortsCodexAstra
	EffortsXAIGrok46
	EffortsXAILegacy
	EffortsActualRelay
	EffortsKimiK3
	EffortsKimiK2
	EffortsOxAlpha
	EffortsTokenHub
	EffortsNebius
	EffortsSolar
	EffortsGLM52
	EffortsGLM53
	EffortsDeepSeekV4
	EffortsOllamaCloud
	EffortsMetaAI
	EffortsGemini4
)

// Every reasoning level from weakest to strongest, none included so a disable can be clamped
func EffortLadder() []string {
	ladder := effortLadder()
	return ladder[:]
}

// Levels in ladder order as a value
func effortLadder() [8]string {
	return [8]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

// Position of a level on the ladder, negative for a word the ladder does not know
func EffortRank(effort string) int {
	for rank, level := range effortLadder() {
		if level == effort {
			return rank
		}
	}
	return -1
}

// Vocabulary of the family as a fresh value
func (f EffortFamily) Efforts() Efforts {
	top := map[string]string{"xhigh": "max"}
	switch f {
	case EffortsOpenAICompat:
		return Efforts{Levels: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}}
	case EffortsCodexGPT56:
		return Efforts{Levels: []string{"none", "low", "medium", "high", "xhigh", "max"}}
	case EffortsCodexLegacy:
		return Efforts{Levels: []string{"none", "low", "medium", "high", "xhigh"}}
	case EffortsCodexAstra:
		return Efforts{Levels: []string{"low", "medium", "high", "xhigh", "max"}}
	case EffortsXAIGrok46:
		return Efforts{Levels: []string{"low", "medium", "high", "xhigh"}}
	case EffortsXAILegacy, EffortsKimiK2, EffortsTokenHub, EffortsNebius, EffortsSolar, EffortsGemini4:
		return Efforts{Levels: []string{"low", "medium", "high"}}
	case EffortsActualRelay:
		return Efforts{Levels: []string{"none", "low", "medium", "high", "max"}}
	case EffortsKimiK3:
		return Efforts{Levels: []string{"low", "high", "max"}, Round: map[string]string{"medium": "high", "xhigh": "max"}}
	case EffortsOxAlpha:
		return Efforts{Levels: []string{"low", "high", "max"}, Round: top}
	case EffortsGLM52:
		return Efforts{Levels: []string{"high", "max"}, Round: top}
	case EffortsGLM53, EffortsDeepSeekV4:
		return Efforts{Levels: []string{"low", "medium", "high", "max"}, Round: top}
	case EffortsOllamaCloud:
		return Efforts{Levels: []string{"none", "low", "medium", "high", "max"}, Round: top}
	case EffortsMetaAI:
		return Efforts{Levels: []string{"minimal", "low", "medium", "high", "xhigh"}}
	}
	return Efforts{}
}

// Reports whether the wire accepts the level as written
func (e Efforts) Has(effort string) bool {
	return slices.Contains(e.Levels, effort)
}

// Clamps a requested level onto the vocabulary with its declared rounding
func (e Efforts) Clamp(effort string) string {
	return ClampEffort(effort, e.Levels, e.Round)
}

// Clamps a level onto supported ones, rounding first, else the nearest weaker level, else the weakest
func ClampEffort(effort string, supported []string, round map[string]string) string {
	requested := strings.ToLower(trimSpace(effort))
	if requested == "" || len(supported) == 0 {
		return effort
	}
	var known []string
	for _, level := range supported {
		if level = strings.ToLower(trimSpace(level)); EffortRank(level) >= 0 {
			known = append(known, level)
		}
	}
	if len(known) == 0 || slices.Contains(known, requested) {
		return effort
	}
	if rounded, ok := round[requested]; ok && slices.Contains(known, rounded) {
		return rounded
	}
	rank := EffortRank(requested)
	if rank < 0 {
		return effort
	}
	below, floor := "", ""
	for _, level := range known {
		if level == "none" {
			continue
		}
		r := EffortRank(level)
		if floor == "" || r < EffortRank(floor) {
			floor = level
		}
		if r < rank && (below == "" || r > EffortRank(below)) {
			below = level
		}
	}
	switch {
	case below != "":
		return below
	case floor != "":
		return floor
	}
	return effort
}

// Bare lowercase model id with any vendor prefix removed
func bareModel(model string) string {
	bare := strings.ToLower(trimSpace(model))
	return bare[strings.LastIndex(bare, "/")+1:]
}

// Reports whether the model is GPT-6 Astra under either of its ids, with or without a vendor prefix
func IsAstraModel(model string) bool {
	bare := bareModel(model)
	return bare == "gpt-6-astra" || bare == "gpt-6-astra-900k"
}

// Family of the levels an OpenAI or Codex Responses model accepts
func CodexEfforts(model string) EffortFamily {
	bare := bareModel(model)
	switch {
	case IsAstraModel(model) || strings.HasPrefix(bare, "gpt-6.1-sol"):
		return EffortsCodexAstra
	case strings.Contains(bare, "gpt-5.6") || strings.HasPrefix(bare, "gpt-6-sol") || strings.HasPrefix(bare, "gpt-6-luna"),
		bare == "gpt-daybreak-blue-latest" || bare == "gpt-daybreak-blue-latest-900k":
		return EffortsCodexGPT56
	}
	return EffortsCodexLegacy
}

// Family of the levels a Moonshot or Kimi model accepts, K3 for k3 as a delimited token of the id
func KimiEfforts(model string) EffortFamily {
	m := strings.ToLower(trimSpace(model))
	m = m[strings.LastIndex(m, "/")+1:]
	alnum := func(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
	for i := 0; i+2 <= len(m); i++ {
		if m[i:i+2] == "k3" && (i == 0 || !alnum(m[i-1])) && (i+2 == len(m) || !alnum(m[i+2])) {
			return EffortsKimiK3
		}
	}
	return EffortsKimiK2
}

// Levels a route's entry clamp accepts, the Codex ladder per model and ultra on the app server where max exists
func RouteEfforts(provider, model string, mode Mode) []string {
	if strings.ToLower(trimSpace(provider)) != "openai-codex" {
		return EffortsOpenAICompat.Efforts().Levels
	}
	levels := CodexEfforts(model).Efforts().Levels
	if mode == ModeCodexAppServer && slices.Contains(levels, "max") {
		levels = append(levels, "ultra")
	}
	return levels
}

// Status label of a level, saying what the route sends when it does not send the level itself
func EffortLabel(effort, provider, model string, mode Mode) string {
	requested := strings.ToLower(trimSpace(effort))
	clamped := ClampEffort(requested, RouteEfforts(provider, model, mode), nil)
	if requested == "" || clamped == requested {
		return requested
	}
	return requested + " (sends " + clamped + " on this route)"
}

// Level the user explicitly asked for, empty when reasoning is unset, disabled or has no level
func RequestedEffort(r *Reasoning) string {
	if r == nil || !r.Enabled {
		return ""
	}
	return strings.ToLower(trimSpace(r.Effort))
}

// Top-level reasoning_effort for TokenHub, high when no level was chosen
func TokenHubEffort(requested string) string {
	if requested == "" {
		return "high"
	}
	return EffortsTokenHub.Efforts().Clamp(requested)
}

// The reasoning ask with its level clamped onto supported ones, unchanged when the level already fits
func ClampReasoning(r *Reasoning, supported []string) *Reasoning {
	if r == nil {
		return nil
	}
	effort := strings.ToLower(trimSpace(r.Effort))
	if effort == "" {
		return r
	}
	clamped := ClampEffort(effort, supported, nil)
	if clamped == effort {
		return r
	}
	out := *r
	out.Effort = clamped
	return &out
}

// Value of the nested reasoning member a route with no declared reasoning shape takes, false when unset
func NestedReasoning(r *Reasoning) (json.RawMessage, bool) {
	switch {
	case r == nil:
		return nil, false
	case !r.Enabled:
		return json.RawMessage(`{"enabled":false}`), true
	}
	effort := r.Effort
	if effort == "" {
		effort = "medium"
	}
	return append(appendString([]byte(`{"enabled":true,"effort":`), effort, 0), '}'), true
}

// Body members for the thinking toggle and top-level effort of a Moonshot or DeepSeek style wire
func ThinkingToggleExtras(r *Reasoning, e Efforts, alwaysToggle bool) (extraBody, topLevel map[string]json.RawMessage) {
	enabled := map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"enabled"}`)}
	if r != nil && !r.Enabled {
		return map[string]json.RawMessage{"thinking": json.RawMessage(`{"type":"disabled"}`)}, nil
	}
	effort := RequestedEffort(r)
	if effort == "none" {
		effort = ""
	}
	clamped := e.Clamp(effort)
	if !e.Has(clamped) {
		return enabled, nil
	}
	topLevel = map[string]json.RawMessage{"reasoning_effort": appendString(nil, clamped, 0)}
	if alwaysToggle {
		return enabled, topLevel
	}
	return nil, topLevel
}

// Body members that make a DashScope-hosted model keep replayed reasoning, nil for other models
func DashScopePreserveThinking(model string) map[string]json.RawMessage {
	m := strings.ToLower(trimSpace(model))
	for _, token := range [...]string{"qwen3.6", "qwen3.7", "qwen3.8", "kimi-k2.6", "kimi-k2.7"} {
		if strings.Contains(m, token) {
			return map[string]json.RawMessage{"preserve_thinking": json.RawMessage("true")}
		}
	}
	if strings.HasPrefix(m, "glm-5") {
		return map[string]json.RawMessage{"clear_thinking": json.RawMessage("false")}
	}
	return nil
}

// Top-level effort member for the Ox Alpha preview model, nil for other models and unset levels
func OxAlphaReasoning(r *Reasoning, model string) map[string]json.RawMessage {
	bare := trimSpace(model)
	if strings.ToLower(bare[strings.LastIndex(bare, "/")+1:]) != "x-preview-f-free" {
		return nil
	}
	effort := RequestedEffort(r)
	if effort == "none" {
		effort = ""
	}
	e := EffortsOxAlpha.Efforts()
	clamped := e.Clamp(effort)
	if !e.Has(clamped) {
		return nil
	}
	return map[string]json.RawMessage{"reasoning_effort": appendString(nil, clamped, 0)}
}
