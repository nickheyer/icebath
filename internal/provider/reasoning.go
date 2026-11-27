package provider

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Efforts from least to most, as every wire names them
var Ladder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// Efforts a model can be told to think with, least first: what a setting offers and an OpenAI compatible endpoint takes in reasoning_effort
var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// DeepSeek V4 and later efforts, with the level a missing one maps to
var (
	DeepSeekEfforts   = []string{"low", "medium", "high", "max"}
	DeepSeekOverrides = map[string]string{"xhigh": "max"}
)

// GLM 5.2 efforts, with the level a missing one maps to
var (
	GLM52Efforts   = []string{"high", "max"}
	GLM52Overrides = map[string]string{"xhigh": "max"}
	// Spellings of GLM 5.2 in model ids
	GLM52Tokens = []string{"glm-5.2", "glm-5-2", "glm-5p2"}
)

// OpenAI and Codex Responses effort vocabularies by model generation
var (
	codexLegacyEfforts = []string{"none", "low", "medium", "high", "xhigh"}
	codexAstraEfforts  = []string{"low", "medium", "high", "xhigh", "max"}
	codexGPT56Efforts  = []string{"none", "low", "medium", "high", "xhigh", "max"}
	astraModels        = []string{"gpt-6-astra", "gpt-6-astra-900k"}
	gpt6Tiers          = []string{"gpt-6-sol", "gpt-6-luna"}
	noDisableTiers     = []string{"gpt-6.1-sol"}
	daybreakModels     = []string{"gpt-daybreak-blue-latest", "gpt-daybreak-blue-latest-900k"}
)

// What a request asks of the model's reasoning: nothing, thinking off, or an effort
type Ask struct {
	// Whether the request asks anything
	Set bool
	// Thinking turned off
	Off bool
	// Effort asked for in lower case, empty for the model's own
	Effort string
}

// What the request's reasoning asks: none, false and disabled turn thinking off, a level asks for it
func (r Reasoning) Ask() Ask {
	effort := strings.ToLower(strings.TrimSpace(r.Effort))
	switch effort {
	case "":
		return Ask{}
	case "none", "false", "disabled", "off":
		return Ask{Set: true, Off: true}
	}
	return Ask{Set: true, Effort: effort}
}

// Ask for thinking at the effort
func Effort(effort string) *Ask {
	return &Ask{Set: true, Effort: effort}
}

// Explicit effort of the ask, empty when it asks none or turns thinking off
func (a Ask) Requested() string {
	if !a.Set || a.Off {
		return ""
	}
	return a.Effort
}

// Ask as the reasoning object OpenAI compatible routers take: enabled, and the effort when one is asked
func (a Ask) Object() map[string]any {
	object := map[string]any{"enabled": !a.Off}
	if a.Effort != "" {
		object["effort"] = a.Effort
	}
	return object
}

// What a provider's listing says of a model's reasoning: whether the model takes reasoning fields, the efforts listed for it, and whether
// it can't be turned off
type Thinking struct {
	Takes     bool
	Efforts   []string
	Mandatory bool
	// Whether a listing said anything of the model
	Known bool
}

// Place of the effort on the ladder, -1 off it
func Rung(effort string) int {
	return slices.Index(Ladder, effort)
}

// Nearest weaker effort of those supported, the override of a missing one when supported, the request as it is when supported or off the
// ladder; the weakest supported when none is weaker
func ClampEffort(effort string, supported []string, overrides map[string]string) string {
	requested := strings.ToLower(strings.TrimSpace(effort))
	if requested == "" || len(supported) == 0 {
		return effort
	}
	var levels []string
	for _, s := range supported {
		if level := strings.ToLower(strings.TrimSpace(s)); Rung(level) >= 0 {
			levels = append(levels, level)
		}
	}
	if len(levels) == 0 || slices.Contains(levels, requested) {
		return effort
	}
	if override, ok := overrides[requested]; ok && slices.Contains(levels, override) {
		return override
	}
	at := Rung(requested)
	if at < 0 {
		return effort
	}
	best, bestAt, least, leastAt := "", -1, "", len(Ladder)
	for _, level := range levels {
		if level == "none" {
			continue
		}
		i := Rung(level)
		if i < at && i > bestAt {
			best, bestAt = level, i
		}
		if i < leastAt {
			least, leastAt = level, i
		}
	}
	switch {
	case best != "":
		return best
	case least != "":
		return least
	}
	return effort
}

// Ask with its effort clamped onto the supported vocabulary
func (a Ask) Clamped(supported []string) Ask {
	if a.Effort != "" {
		a.Effort = ClampEffort(a.Effort, supported, nil)
	}
	return a
}

// Moonshot or DeepSeek style thinking toggle in the body and the effort at its top: thinking disabled when turned off, the effort clamped onto
// the vocabulary when one is asked (with thinking enabled too when always is set), else thinking enabled
func ThinkingToggle(a Ask, efforts []string, overrides map[string]string, always bool) (extra, top map[string]any) {
	if a.Set && a.Off {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}, nil
	}
	effort := a.Requested()
	if effort == "none" {
		effort = ""
	}
	clamped := ClampEffort(effort, efforts, overrides)
	if effort != "" && slices.Contains(efforts, clamped) {
		extra = map[string]any{}
		if always {
			extra["thinking"] = map[string]any{"type": "enabled"}
		}
		return extra, map[string]any{"reasoning_effort": clamped}
	}
	return map[string]any{"thinking": map[string]any{"type": "enabled"}}, nil
}

// Nested reasoning object of a route without a declared shape: nothing unasked, disabled when off, else the effort or medium
func NestedReasoning(a Ask) map[string]any {
	switch {
	case !a.Set:
		return nil
	case a.Off:
		return map[string]any{"reasoning": map[string]any{"enabled": false}}
	}
	effort := a.Effort
	if effort == "" {
		effort = "medium"
	}
	return map[string]any{"reasoning": map[string]any{"enabled": true, "effort": effort}}
}

// Lower case model id without any vendor prefix
func BareModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	return m[strings.LastIndex(m, "/")+1:]
}

// Whether the text starts with any of the prefixes
func HasPrefix(s string, prefixes ...string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}

// Whether the text holds any of the parts
func Contains(s string, parts ...string) bool {
	return slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(s, p) })
}

// Whether the model is a GPT-6 Astra model
func Astra(model string) bool {
	return slices.Contains(astraModels, BareModel(model))
}

// Effort vocabulary of an OpenAI or Codex Responses model generation
func CodexEfforts(model string) []string {
	bare := BareModel(model)
	switch {
	case Astra(model) || HasPrefix(bare, noDisableTiers...):
		return codexAstraEfforts
	case strings.Contains(bare, "gpt-5.6") || HasPrefix(bare, gpt6Tiers...) || slices.Contains(daybreakModels, bare):
		return codexGPT56Efforts
	}
	return codexLegacyEfforts
}

// Effort vocabulary of a Responses route: the model generation's, except GPT-6 Astra off OpenAI's own API and the Codex backend, which
// takes the legacy one
func ResponsesEfforts(model string, official bool) []string {
	if Astra(model) && !official {
		return codexLegacyEfforts
	}
	return CodexEfforts(model)
}

// Thinking levels Gemini 4 takes
var gemini4Levels = []string{"low", "medium", "high"}

// Efforts Gemini maps to its high thinking level
var highEfforts = []string{"high", "xhigh", "max", "ultra"}

var geminiVersion = regexp.MustCompile(`^gemini-(\d+)(?:\.(\d+))?`)

// Published Gemini output ceiling that thinking requests are raised to
const GeminiMaxOutput = 65535

// Major and minor version of a Gemini model id, zero for an unversioned or other model
func GeminiVersion(model string) (int, int) {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"models/", "google/", "gemini/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	m := geminiVersion.FindStringSubmatch(name)
	if m == nil {
		return 0, 0
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major, minor
}

// Whether a Gemini model ignores sampling settings, as 3.6 and later do
func GeminiSamplingDeprecated(model string) bool {
	major, minor := GeminiVersion(model)
	return major > 3 || major == 3 && minor >= 6
}

var claudeVersion = regexp.MustCompile(`claude-(opus|sonnet|haiku)-(\d+)(?:[-.](\d{1,2}))?(?:\D|$)`)

// Whether a Claude model refuses sampling settings, as those after 4.5 do but for Opus and Sonnet 4.6
func ClaudeSamplingForbidden(model string) bool {
	bare := strings.ToLower(model)
	if !strings.Contains(bare, "claude") || strings.HasPrefix(bare, "claude-3") {
		return false
	}
	match := claudeVersion.FindStringSubmatch(bare)
	if match == nil {
		return false
	}
	major, _ := strconv.Atoi(match[2])
	minor, _ := strconv.Atoi(match[3])
	switch {
	case major < 4, major == 4 && (minor == 0 || minor == 1 || minor == 5):
		return false
	case major == 4 && minor == 6 && match[1] != "haiku":
		return false
	}
	return true
}

// Whether a Gemini model takes a thinking level rather than a budget: the -latest aliases and Gemini 3 on
func geminiLevels(model string) bool {
	bare := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "google/"), "gemini/")
	major, _ := GeminiVersion(bare)
	return strings.HasPrefix(bare, "gemini-") && strings.HasSuffix(bare, "-latest") || major >= 3
}

// Gemini thinkingConfig for an ask, nil for another model or no ask
func GeminiThinking(model string, a Ask) map[string]any {
	if !a.Set {
		return nil
	}
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "google/")
	if !strings.HasPrefix(name, "gemini") {
		return nil
	}
	effort := a.Effort
	if effort == "" {
		effort = "medium"
	}
	if strings.HasPrefix(name, "gemini-4") {
		off := a.Off || effort == "none"
		level := "low"
		if !off {
			if level = ClampEffort(effort, gemini4Levels, nil); !slices.Contains(gemini4Levels, level) {
				level = "medium"
			}
		}
		return map[string]any{"includeThoughts": !off, "thinkingLevel": level}
	}
	if a.Off || effort == "none" {
		config := map[string]any{"includeThoughts": false}
		if strings.HasPrefix(name, "gemini-2.5-") {
			config["thinkingBudget"] = 0
		} else if geminiLevels(name) {
			config["thinkingLevel"] = "low"
		}
		return config
	}
	config := map[string]any{"includeThoughts": true}
	if strings.HasPrefix(name, "gemini-2.5-") || !geminiLevels(name) {
		return config
	}
	if Rung(effort) < 0 {
		effort = "medium"
	}
	switch {
	case strings.Contains(name, "pro") && slices.Contains(highEfforts, effort):
		config["thinkingLevel"] = "high"
	case strings.Contains(name, "pro"):
		config["thinkingLevel"] = "low"
	case effort == "minimal" || effort == "low":
		config["thinkingLevel"] = "low"
	case slices.Contains(highEfforts, effort):
		config["thinkingLevel"] = "high"
	default:
		config["thinkingLevel"] = "medium"
	}
	return config
}

// Gemini thinking config in the snake case the OpenAI compatible surfaces take, nil when it says nothing
func GeminiSnake(config map[string]any) map[string]any {
	out := map[string]any{}
	if include, ok := config["includeThoughts"].(bool); ok {
		out["include_thoughts"] = include
	}
	if level, ok := config["thinkingLevel"].(string); ok && strings.TrimSpace(level) != "" {
		out["thinking_level"] = strings.ToLower(strings.TrimSpace(level))
	}
	if budget, ok := config["thinkingBudget"].(int); ok {
		out["thinking_budget"] = budget
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
