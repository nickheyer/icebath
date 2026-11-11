package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	FocusConfigKey        = "display.focus_view"
	FocusToolProgressMode = "off"
	FocusStatusbarLabel   = "◉ focus"
)

var toolProgressVisibleModes = map[string]bool{"new": true, "all": true, "verbose": true}

// Coerces a raw config or attribute value into a known tool progress mode, log kept as a gateway mode
func normalizeToolProgressMode(mode any, def string) string {
	return tuigateway.NormalizeToolProgressMode(mode, def)
}

// Maps a focus argument onto its action and the requested enabled state for set
func resolveFocusArg(arg string, current bool) (string, *bool) {
	return tuigateway.ResolveFocusArg(arg, current)
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
	return tuigateway.FormatFocusStatus(enabled, configuredMode)
}

// Confirmation line printed when focus view is switched without ANSI
func formatFocusToggleMessage(enabled bool, configuredMode any) string {
	return tuigateway.FormatFocusToggleMessage(enabled, configuredMode)
}
