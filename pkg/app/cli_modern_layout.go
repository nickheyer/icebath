package app

import (
	"strings"
	"sync/atomic"
)

var modernLayoutActive atomic.Bool

// Reports the glyph layout, the default, with classic keeping the boxed transcript
func layoutFromConfig(display map[string]any) bool {
	return strings.ToLower(strings.TrimSpace(cliStr(display["cli_layout"], "modern"))) != "classic"
}

// Reads display.busy_input_mode, unset interrupting in both layouts
func busyInputModeFromConfig(display map[string]any) string {
	return strings.ToLower(strings.TrimSpace(cliStr(display["busy_input_mode"], "interrupt")))
}

// Records whether the modern layout is active for the process
func setLayoutActive(enabled bool) { modernLayoutActive.Store(enabled) }

// Reports whether the modern layout is active for the process
func layoutActive() bool { return modernLayoutActive.Load() }
