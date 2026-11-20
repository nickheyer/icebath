package tuihost

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Tool progress mode focus view pins while it is on
const FocusToolProgressMode = "off"

var toolProgressModeWords = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}

// Mouse tracking spellings mapped to off, wheel, buttons or all
var MouseTrackingAliases = tuigateway.MouseTrackingAliases

// Mouse tracking mode of the display config, legacy tui_mouse only when mouse_tracking is absent
func DisplayMouseTracking(display any) string {
	section, _ := display.(map[string]any)
	return string(tuigateway.MouseTrackingOf(section))
}

// Settings the terminal UI follows from a profile config, the local display overlay applied and the arena redacted
func ClientConfig(cfg map[string]any) tuigateway.ClientConfig {
	display := config.LocalDisplayConfig(cfg["display"])
	if arena, present := display["arena"]; present {
		display["arena"] = config.RedactConfigValue(arena)
	}
	return tuigateway.ClientConfigOf(map[string]any{"display": display, "approvals": cfg["approvals"], "voice": cfg["voice"],
		"paste_collapse_threshold": cfg["paste_collapse_threshold"], "paste_collapse_char_threshold": cfg["paste_collapse_char_threshold"]})
}

// Known tool progress mode of a raw config value, log kept as a gateway mode, else the default
func NormalizeToolProgressMode(mode any, def string) string {
	if b, ok := mode.(bool); ok {
		if b {
			return "all"
		}
		return "off"
	}
	text := ""
	if pyrepr.Truthy(mode) {
		text = pystr.Lower(pystr.Strip(pyrepr.Str(mode)))
	}
	if toolProgressModeWords[text] || text == "log" {
		return text
	}
	return def
}

var focusStatusWords = map[string]bool{"status": true, "show": true, "?": true}
var focusOnWords = map[string]bool{"on": true, "enable": true, "enabled": true, "true": true, "yes": true, "1": true}
var focusOffWords = map[string]bool{"off": true, "disable": true, "disabled": true, "false": true, "no": true, "0": true}

// Focus argument mapped to set, status or usage and the requested enabled state for set
func ResolveFocusArg(arg string, current bool) (string, *bool) {
	text := pystr.Lower(pystr.Strip(arg))
	switch {
	case text == "" || text == "toggle":
		target := !current
		return "set", &target
	case focusStatusWords[text]:
		return "status", nil
	case focusOnWords[text]:
		on := true
		return "set", &on
	case focusOffWords[text]:
		off := false
		return "set", &off
	}
	return "usage", nil
}
