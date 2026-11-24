package tuihost

import (
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var toolProgressModeWords = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}

// Known tool progress mode of a raw config value, log kept as a gateway mode, else the default
func NormalizeToolProgressMode(mode any, def string) string {
	if b, ok := mode.(bool); ok {
		if b {
			return "all"
		}
		return "off"
	}
	text := ""
	if anyutil.IsTrue(mode) {
		text = strings.ToLower(strings.TrimSpace(anyutil.String(mode)))
	}
	if toolProgressModeWords[text] || text == "log" {
		return text
	}
	return def
}

var focusStatusWords = map[string]bool{"status": true, "show": true, "?": true}
var focusOnWords = map[string]bool{"on": true, "enable": true, "enabled": true, "true": true, "yes": true, "1": true}
var focusOffWords = map[string]bool{"off": true, "disable": true, "disabled": true, "false": true, "no": true, "0": true}
