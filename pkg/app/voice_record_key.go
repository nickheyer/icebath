package app

import (
	"runtime"
	"strings"
	"unicode/utf8"
)

// Modifier spellings of voice.record_key to their prompt key prefixes
var voiceModAliases = map[string]string{"ctrl": "c-", "control": "c-", "alt": "a-", "option": "a-", "opt": "a-"}

// Named keys voice.record_key accepts, aliases collapsed to the canonical spelling
var voiceNamedKeys = map[string]string{"space": "space", "spc": "space", "enter": "enter", "return": "enter", "ret": "enter", "tab": "tab",
	"escape": "escape", "esc": "escape", "backspace": "backspace", "bs": "backspace", "delete": "delete", "del": "delete"}

// Single characters the interrupt, quit and clear bindings already claim
var voiceReservedChars = map[string]bool{"c": true, "d": true, "l": true}

// Record key every malformed or unsupported setting falls back to
const defaultPTKey = "c-b"

// Platform the record key reservation reads, swapped by tests
var voiceKeyGOOS = runtime.GOOS

// The voice.record_key setting, nil when the voice section is missing or is not a mapping
func voiceRecordKeyFromConfig(cfg any) any {
	root, ok := cfg.(map[string]any)
	if !ok {
		return nil
	}
	voice, ok := root["voice"].(map[string]any)
	if !ok {
		return nil
	}
	return voice["record_key"]
}

// Coerces voice.record_key into the c-x or a-x key form, anything unsupported becoming c-b
func normalizeVoiceRecordKeyForPromptToolkit(raw any) string {
	text, ok := raw.(string)
	if !ok {
		return defaultPTKey
	}
	var parts []string
	for _, p := range strings.Split(strings.ToLower(strings.TrimSpace(text)), "+") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) != 2 {
		return defaultPTKey
	}
	mod, ok := voiceModAliases[parts[0]]
	if !ok {
		return defaultPTKey
	}
	key := parts[1]
	if utf8.RuneCountInString(key) == 1 {
		if (mod == "c-" || voiceKeyGOOS == "darwin") && voiceReservedChars[key] {
			return defaultPTKey
		}
		return mod + key
	}
	if named, ok := voiceNamedKeys[key]; ok {
		return mod + named
	}
	return defaultPTKey
}

// Key sequence of a c-x or a-x key, Alt spelled as escape then the key
func ptKeyToSequence(ptKey string) []string {
	if strings.HasPrefix(ptKey, "a-") {
		return []string{"escape", ptKey[2:]}
	}
	return []string{ptKey}
}

// Renders voice.record_key for the status line as Ctrl+B or Alt+Space
func formatVoiceRecordKeyForStatus(raw any) string {
	normalized := normalizeVoiceRecordKeyForPromptToolkit(raw)
	prefix := "Ctrl+"
	if strings.HasPrefix(normalized, "a-") {
		prefix = "Alt+"
	}
	key := normalized[2:]
	return prefix + strings.ToUpper(key[:1]) + key[1:]
}
