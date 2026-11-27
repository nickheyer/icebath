package provider

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// GLM 5.3 spellings and efforts; GLM 5.2's are the host's, which take 5.3 too
var (
	glm53Tokens    = []string{"glm-5.3", "glm-5-3", "glm-5p3"}
	glm53Efforts   = []string{"low", "medium", "high", "max"}
	glm53Overrides = map[string]string{"xhigh": "max"}
	glm52Tokens    = slices.Concat(provider.GLM52Tokens, glm53Tokens)
)

var glmVersion = regexp.MustCompile(`^glm-(\d+)(?:\.(\d+))?`)

// Whether a GLM model thinks: 4.5 and later
func thinks(model string) bool {
	m := glmVersion.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 4 || major == 4 && minor >= 5
}

// Effort on the GLM 5.2 or 5.3 vocabulary, an effort below them taken up to their floor; empty unasked or off
func effort(ask provider.Ask, model string) string {
	requested := ask.Requested()
	if requested == "" || requested == "none" {
		return ""
	}
	efforts, overrides, floor := provider.GLM52Efforts, provider.GLM52Overrides, "high"
	if provider.Contains(strings.ToLower(strings.TrimSpace(model)), glm53Tokens...) {
		efforts, overrides, floor = glm53Efforts, glm53Overrides, "low"
	}
	if clamped := provider.ClampEffort(requested, efforts, overrides); slices.Contains(efforts, clamped) {
		return clamped
	}
	return floor
}

// Thinking toggle in the body, keeping replayed reasoning, plus GLM 5.2's own effort; GLM 5.3 never takes thinking off
func shape(call *provider.Call) (map[string]any, map[string]any) {
	model := strings.ToLower(strings.TrimSpace(call.Model))
	is52 := provider.Contains(model, glm52Tokens...)
	if !thinks(model) && !is52 {
		return nil, nil
	}
	extra, top := map[string]any{}, map[string]any{}
	if call.Ask.Set {
		enabled := !call.Ask.Off
		if !enabled && provider.Contains(model, glm53Tokens...) {
			enabled = true
			top["reasoning_effort"] = "low"
		}
		extra["thinking"] = map[string]any{"type": "disabled"}
		if enabled {
			extra["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
		}
	}
	if is52 {
		if level := effort(call.Ask, model); level != "" {
			top["reasoning_effort"] = level
		}
	}
	return extra, top
}
