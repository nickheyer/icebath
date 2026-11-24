package plugins

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Catalog plugin that applies idle and daily session resets
const SessionResetPluginName = "icebath-session-reset-policy"

// Config block the session reset plugin reads
const SessionResetConfigKey = "session_reset"

var sessionResetTimedModes = map[string]bool{"idle": true, "daily": true, "both": true}

// Mode of a timed session_reset block a config declares, ok false when there is none
func TimedSessionReset(cfg any) (mode string, ok bool) {
	doc, isMap := cfg.(map[string]any)
	if !isMap {
		return "", false
	}
	block, isBlock := doc[SessionResetConfigKey].(map[string]any)
	if !isBlock || !pyrepr.Truthy(block["mode"]) {
		return "", false
	}
	mode = strings.ToLower(strings.TrimSpace(pyrepr.Str(block["mode"])))
	if !sessionResetTimedModes[mode] {
		return "", false
	}
	return mode, true
}

// Whether the session reset plugin is installed and enabled in the plugin set of this process
func ResetPluginEnabled(ctx context.Context) bool {
	DiscoverPlugins(ctx, false)
	for _, p := range GetPluginManager(ctx).ListPlugins() {
		if pyrepr.Equal(p["name"], SessionResetPluginName) && pyrepr.Truthy(p["enabled"]) {
			return true
		}
	}
	return false
}

// Notice that a timed session_reset mode waits on the plugin that applies it
func FormatSessionResetNotice(mode string) string {
	return fmt.Sprintf("%s.mode: %s needs the %s plugin, which is not installed or enabled, so conversations in chat apps reset only on /new or /reset. "+
		"Run `icebath plugins install %s`.", SessionResetConfigKey, mode, SessionResetPluginName, SessionResetPluginName)
}
