package app

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	FocusConfigKey        = "display.focus_view"
	FocusToolProgressMode = "off"
	FocusStatusbarLabel   = "◉ focus"
)

var toolProgressVisibleModes = map[string]bool{"new": true, "all": true, "verbose": true}

var toolProgressModes = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}

// Focus argument words mapped to their action and target enabled state
var focusWords = map[string]struct {
	action string
	target *bool
}{}

func init() {
	on, off := true, false
	for _, w := range []string{"status", "show", "?"} {
		focusWords[w] = struct {
			action string
			target *bool
		}{"status", nil}
	}
	for _, w := range []string{"on", "enable", "enabled", "true", "yes", "1"} {
		focusWords[w] = struct {
			action string
			target *bool
		}{"set", &on}
	}
	for _, w := range []string{"off", "disable", "disabled", "false", "no", "0"} {
		focusWords[w] = struct {
			action string
			target *bool
		}{"set", &off}
	}
}

// Coerces a raw config or attribute value into a known tool progress mode, log kept as a gateway mode
func normalizeToolProgressMode(mode any, def string) string {
	if b, ok := mode.(bool); ok {
		if b {
			return "all"
		}
		return "off"
	}
	text := ""
	if pyrepr.Truthy(mode) {
		text = strings.ToLower(strings.TrimSpace(pyrepr.Str(mode)))
	}
	if toolProgressModes[text] || text == "log" {
		return text
	}
	return def
}

// Maps a focus argument onto its action and the requested enabled state for set
func resolveFocusArg(arg string, current bool) (string, *bool) {
	text := strings.ToLower(strings.TrimSpace(arg))
	if text == "" || text == "toggle" {
		target := !current
		return "set", &target
	}
	if entry, ok := focusWords[text]; ok {
		return entry.action, entry.target
	}
	return "usage", nil
}

// Reports whether the CLI would have committed a scrollback line for this tool call under the mode
func wouldDisplayToolLine(mode any, functionName, lastToolName string) bool {
	if functionName == "" {
		return false
	}
	normalized := normalizeToolProgressMode(mode, "all")
	return toolProgressVisibleModes[normalized] && !(normalized == "new" && functionName == lastToolName)
}

// Dim post turn recovery line, empty when nothing was hidden or the count does not parse
func formatHiddenLine(count any) string {
	n, ok := pyIntOf(count)
	if !ok || n <= 0 {
		return ""
	}
	noun := "tool lines"
	if n == 1 {
		noun = "tool line"
	}
	return fmt.Sprintf("⋯ %d %s hidden · /focus off to show", n, noun)
}

// Status bar segment text for focus view, empty when off
func focusStatusbarSegment(enabled bool) string {
	if enabled {
		return FocusStatusbarLabel
	}
	return ""
}

// Human readable focus status body without ANSI
func formatFocusStatus(enabled bool, configuredMode any) string {
	mode := strings.ToUpper(normalizeToolProgressMode(configuredMode, "all"))
	if enabled {
		return fmt.Sprintf("Focus view: ON — only your prompt and the final response.\n  /focus off restores tool progress: %s", mode)
	}
	return fmt.Sprintf("Focus view: OFF — tool progress: %s", mode)
}

// Confirmation line printed when focus view is switched without ANSI
func formatFocusToggleMessage(enabled bool, configuredMode any) string {
	if enabled {
		return "Focus view enabled — just your prompt and the final response"
	}
	return fmt.Sprintf("Focus view disabled — tool progress: %s", strings.ToUpper(normalizeToolProgressMode(configuredMode, "all")))
}
