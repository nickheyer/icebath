package provider

import (
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Upstage models with no reasoning control, and the efforts Solar takes
var (
	plain   = regexp.MustCompile(`solar-mini(?:[-:.@]|$)|syn-pro`)
	efforts = []string{"low", "medium", "high"}
)

// Solar's effort at the top: medium unasked, nothing when off or minimal, a level it doesn't take moved to its nearest one
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if plain.MatchString(strings.ToLower(strings.TrimSpace(call.Model))) {
		return nil, nil
	}
	switch ask := call.Ask; {
	case !ask.Set, ask.Effort == "" && !ask.Off:
		return nil, map[string]any{"reasoning_effort": "medium"}
	case ask.Off, ask.Effort == "minimal":
		return nil, nil
	}
	mapped := provider.ClampEffort(call.Ask.Effort, efforts, nil)
	if !slices.Contains(efforts, mapped) {
		mapped = ""
		if provider.Rung(call.Ask.Effort) < 0 {
			mapped = "high"
		}
	}
	if mapped == "" {
		return nil, nil
	}
	return nil, map[string]any{"reasoning_effort": mapped}
}
