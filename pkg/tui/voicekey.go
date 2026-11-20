package tui

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/ui/keys"
)

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

// Voice chord of a voice.record_key config value, the default for anything invalid or reserved
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

// Keymap chords of the voice key, the default one also answering the command key on a Mac, none when the keymap cannot spell it
func (k voiceRecordKey) chords() []string {
	key := k.Ch
	if k.Named == "escape" {
		key = "esc"
	}
	chord, err := keys.Parse(k.Mod + "+" + key)
	if err != nil {
		return nil
	}
	if isMac && k == defaultVoiceRecordKey {
		return []string{chord, "super+" + k.Ch}
	}
	return []string{chord}
}

// Binds the voice action to the configured chord. Session keys that act in every state give the chord up while it is the voice key and take it back
// when the voice key changes; keys that act only in some state, such as deleting a queued input being edited, keep it and come first
func (a *app) bindVoiceKey() {
	const id = "tui.voice"
	action, linked := a.keys.Action(id)
	if !linked {
		return
	}
	chords := a.voiceKey.chords()
	if len(chords) == 0 {
		chords = defaultVoiceRecordKey.chords()
	}
	if slices.Equal(a.keys.Keys(id), chords) {
		return
	}
	for _, displaced := range a.voiceDisplaced {
		a.keys.Register(displaced)
	}
	a.voiceDisplaced = nil
	conditional := map[string]bool{}
	for _, c := range a.commands() {
		conditional[c.ID] = c.When != nil
	}
	for _, other := range a.keys.Actions() {
		if other.ID == id || other.Context != tuiContext || conditional[other.ID] {
			continue
		}
		kept := slices.DeleteFunc(slices.Clone(other.Default), func(c string) bool { return slices.Contains(chords, c) })
		if len(kept) == len(other.Default) {
			continue
		}
		a.voiceDisplaced = append(a.voiceDisplaced, other)
		other.Default = kept
		a.keys.Register(other)
	}
	action.Default = chords
	a.keys.Register(action)
}

// Chord the voice action is bound to as /voice names it, empty in a build without voice
func (a *app) voiceKeyLabel() string { return keys.Format(a.keys.First("tui.voice")) }
