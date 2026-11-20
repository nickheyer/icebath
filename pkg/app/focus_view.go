package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

const (
	FocusToolProgressMode = "off"
)

var toolProgressVisibleModes = map[string]bool{"new": true, "all": true, "verbose": true}

// Coerces a raw config or attribute value into a known tool progress mode, log kept as a gateway mode
func normalizeToolProgressMode(mode any, def string) string {
	return tuihost.NormalizeToolProgressMode(mode, def)
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
