package app

import (
	"runtime"
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
