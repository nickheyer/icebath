package tui

import "strings"

// Parsed voice push to talk chord, a modifier with a letter or a named key
type voiceRecordKey struct {
	Ch    string
	Mod   string
	Named string
}

// Ctrl+B, the chord used when the configured one is missing or invalid
var defaultVoiceRecordKey = voiceRecordKey{Ch: "b", Mod: "ctrl"}

var voiceModAliases = map[string]string{
	"alt": "alt", "control": "ctrl", "ctrl": "ctrl", "option": "alt", "opt": "alt",
	"super": "super", "win": "super", "windows": "super",
}

var voiceNamedKeyAliases = map[string]string{
	"backspace": "backspace", "bs": "backspace", "del": "delete", "delete": "delete", "enter": "enter",
	"esc": "escape", "escape": "escape", "ret": "enter", "return": "enter", "space": "space", "spc": "space", "tab": "tab",
}

var (
	reservedCtrlChars     = map[string]bool{"c": true, "d": true, "l": true, "r": true}
	reservedSuperChars    = map[string]bool{"c": true, "d": true, "l": true, "v": true}
	reservedAltCharsOnMac = map[string]bool{"c": true, "d": true, "l": true}
)

// Named key held in the event
func matchesNamedKey(named string, k inkKey, ch string) bool {
	switch named {
	case "backspace":
		return k.Backspace
	case "delete":
		return k.Delete
	case "enter":
		return k.Return
	case "escape":
		return k.Escape
	case "space":
		return ch == " "
	case "tab":
		return k.Tab
	}
	return false
}

// Voice chord of a voice.record_key config value, the default for anything invalid or reserved
func parseVoiceRecordKey(raw any) voiceRecordKey {
	text, isString := raw.(string)
	if !isString {
		return defaultVoiceRecordKey
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return defaultVoiceRecordKey
	}
	var parts []string
	for _, p := range strings.Split(lower, "+") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) != 2 {
		return defaultVoiceRecordKey
	}
	last := parts[1]
	mod, known := voiceModAliases[parts[0]]
	if !known {
		return defaultVoiceRecordKey
	}
	single := len([]rune(last)) == 1
	if mod == "ctrl" && single && reservedCtrlChars[last] {
		return defaultVoiceRecordKey
	}
	if isMac && mod == "super" && single && reservedSuperChars[last] {
		return defaultVoiceRecordKey
	}
	if isMac && mod == "alt" && single && reservedAltCharsOnMac[last] {
		return defaultVoiceRecordKey
	}
	if single {
		return voiceRecordKey{Ch: last, Mod: mod}
	}
	if named, ok := voiceNamedKeyAliases[last]; ok {
		return voiceRecordKey{Ch: named, Mod: mod, Named: named}
	}
	return defaultVoiceRecordKey
}

// Keypress is the configured voice chord, Cmd+B also counting for the default on macOS
func isVoiceToggleKey(k inkKey, ch string, configured voiceRecordKey) bool {
	if configured.Named != "" {
		if !matchesNamedKey(configured.Named, k, ch) {
			return false
		}
	} else if strings.ToLower(ch) != configured.Ch {
		return false
	}
	if k.Shift {
		return false
	}
	switch configured.Mod {
	case "alt":
		return k.Meta && !k.Escape && !k.Ctrl && !k.Super
	case "ctrl":
		if k.Ctrl {
			return !k.Meta && !k.Super
		}
		isDefault := configured.Mod == defaultVoiceRecordKey.Mod && configured.Ch == defaultVoiceRecordKey.Ch &&
			configured.Named == defaultVoiceRecordKey.Named
		return isDefault && isMac && k.Super && !k.Meta
	case "super":
		return k.Super && !k.Ctrl && !k.Meta
	}
	return false
}
