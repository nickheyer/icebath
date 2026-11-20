package plugins

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Catalog plugin that restores idle and daily session resets
const SessionResetPluginName = "icebath-session-reset-policy"

var sessionResetTimedModes = map[string]bool{"idle": true, "daily": true, "both": true}

// Config path and mode of a timed session_reset block the config still declares, ok false when there is none
func RetiredResetPolicy(cfg any) (path, mode string, ok bool) {
	doc, isMap := cfg.(map[string]any)
	if !isMap {
		return "", "", false
	}
	var gatewayBlock any
	if gateway, isGateway := doc["gateway"].(map[string]any); isGateway {
		gatewayBlock = gateway["session_reset"]
	}
	for _, c := range []struct {
		path  string
		block any
	}{{"session_reset", doc["session_reset"]}, {"gateway.session_reset", gatewayBlock}} {
		block, isBlock := c.block.(map[string]any)
		if !isBlock {
			continue
		}
		m := ""
		if pyrepr.Truthy(block["mode"]) {
			m = pystr.Lower(pystr.Strip(pyrepr.Str(block["mode"])))
		}
		if sessionResetTimedModes[m] {
			return c.path, m, true
		}
	}
	return "", "", false
}

// Whether the restoring plugin is installed and enabled in the plugin set of this process
func ResetPluginEnabled(ctx context.Context) bool {
	DiscoverPlugins(ctx, false)
	for _, p := range GetPluginManager(ctx).ListPlugins() {
		if pyrepr.Equal(p["name"], SessionResetPluginName) && pyrepr.Truthy(p["enabled"]) {
			return true
		}
	}
	return false
}

// Notice that a timed session_reset mode is no longer applied, naming the plugin that restores it
func FormatSessionResetNotice(path, mode string) string {
	move := ""
	if path != "session_reset" {
		move = " (move the block to top-level session_reset first)"
	}
	return fmt.Sprintf("%s.mode: %s is no longer applied: gateway conversations reset only on /new or "+
		"/reset. To keep idle/daily resets, run `icebath plugins install %s`%s.", path, mode, SessionResetPluginName, move)
}
