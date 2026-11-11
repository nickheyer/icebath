package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Tool progress mode focus view pins while it is on
const FocusToolProgressMode = "off"

var toolProgressModeWords = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}

// Mouse tracking spellings mapped to off, wheel, buttons or all
var MouseTrackingAliases = map[string]string{
	"0": "off", "1": "all", "all": "all", "any": "all", "button": "buttons", "buttons": "buttons",
	"click": "buttons", "false": "off", "full": "all", "no": "off", "off": "off", "on": "all",
	"scroll": "wheel", "true": "all", "wheel": "wheel", "yes": "all",
}

// Mouse tracking mode of the display config, legacy tui_mouse only when mouse_tracking is absent
func DisplayMouseTracking(display any) string {
	section, ok := display.(map[string]any)
	if !ok {
		return "all"
	}
	raw, present := section["mouse_tracking"]
	if !present {
		raw, present = section["tui_mouse"]
		if !present {
			raw = true
		}
	}
	if s, isText := raw.(string); isText {
		if mode, known := MouseTrackingAliases[pystr.Lower(pystr.Strip(s))]; known {
			return mode
		}
		return "all"
	}
	if raw == false || raw == 0 || raw == int64(0) || raw == float64(0) {
		return "off"
	}
	return "all"
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
