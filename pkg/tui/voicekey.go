package tui

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Parsed voice push to talk chord, a modifier with a letter or a named key
type voiceRecordKey struct {
	Ch    string
	Mod   string
	Named string
}

// No chord of its own, the key recording through the leader alone, which is what a missing, invalid or reserved configured key leaves
var defaultVoiceRecordKey = voiceRecordKey{}

var voiceModAliases = map[string]string{
	"alt": "alt", "control": "ctrl", "ctrl": "ctrl", "option": "alt", "opt": "alt",
	"super": "super", "win": "super", "windows": "super",
}

var voiceNamedKeyAliases = map[string]string{
	"backspace": "backspace", "bs": "backspace", "del": "delete", "delete": "delete", "enter": "enter",
	"esc": "escape", "escape": "escape", "ret": "enter", "return": "enter", "space": "space", "spc": "space", "tab": "tab",
}

var (
	reservedCtrlChars     = map[string]bool{"c": true, "d": true, "l": true, "r": true, "x": true}
	reservedSuperChars    = map[string]bool{"c": true, "d": true, "l": true, "v": true}
	reservedAltCharsOnMac = map[string]bool{"c": true, "d": true, "l": true}
)

// Voice chord of a voice.record_key config value, none for anything invalid or reserved
func parseVoiceRecordKey(text string) voiceRecordKey {
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

// Keymap chords of the voice key, none for the key of its own left unset or one the keymap cannot spell
func (k voiceRecordKey) chords() []string {
	if k == defaultVoiceRecordKey {
		return nil
	}
	key := k.Ch
	if k.Named == "escape" {
		key = "esc"
	}
	chord, err := keys.Parse(k.Mod + "+" + key)
	if err != nil {
		return nil
	}
	return []string{chord}
}

// Contexts a chord of the voice key competes in, those of the session window
var voiceKeyContexts = []string{tuiContext, keys.Session, keys.Text, keys.Global}

// Binds the voice action to the configured chord as its default: keys a person bound to it themselves stay, and a chord another key of the session
// window already holds is refused with a notice naming that key rather than taken from it
func (a *app) bindVoiceKey() {
	const id = "tui.voice"
	if _, linked := a.keys.Action(id); !linked {
		return
	}
	var free []string
	for _, chord := range a.voiceKey.chords() {
		holder := ""
		for _, other := range a.keys.Lookup(voiceKeyContexts, keys.Press(chord)) {
			if other != id {
				holder = other
				break
			}
		}
		if holder == "" {
			free = append(free, chord)
			continue
		}
		if warned := keys.Format(chord) + ":" + holder; warned != a.voiceKeyWarned {
			a.voiceKeyWarned = warned
			act, _ := a.keys.Action(holder)
			a.sys(warningLine(tr(txt.SessionInputVoiceKeyTaken, keys.Format(chord), act.LabelText(), a.keys.Chord("leader.tui.voice"))))
		}
	}
	if err := a.keys.SetDefault(id, free); err != nil {
		a.sys(warningLine(err.Error()))
	}
}

// Keys that record voice as /voice names them, the chord of its own or else the leader key, empty in a build without voice
func (a *app) voiceKeyLabel() string {
	if k := a.keys.Display("tui.voice"); k != "" {
		return k
	}
	return a.keys.Chord("leader.tui.voice")
}
