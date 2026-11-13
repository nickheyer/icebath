package chrome

import "strings"

// Keyboard event a window reports before the page sees it, in the shape of the Electron input
type KeyInput struct {
	Type         string
	Key          string
	Code         string
	Control      bool
	Shift        bool
	Alt          bool
	Meta         bool
	IsAutoRepeat bool
}

// What an F12 keydown does, forward to the renderer, block, open DevTools or nothing
type F12ShortcutDecision string

const (
	F12Ignore   F12ShortcutDecision = "ignore"
	F12Forward  F12ShortcutDecision = "forward"
	F12Block    F12ShortcutDecision = "block"
	F12DevTools F12ShortcutDecision = "devtools"
)

// Forward while a renderer binding claims F12, block while disabled, otherwise DevTools
func DecideF12Shortcut(input KeyInput, shortcutActive, disabled bool) F12ShortcutDecision {
	if input.Type != "keyDown" {
		return F12Ignore
	}
	if shortcutActive {
		return F12Forward
	}
	if disabled {
		return F12Block
	}
	return F12DevTools
}

// Keyboard event the renderer F12 binding receives
type F12KeyboardEvent struct {
	Alt     bool   `json:"alt"`
	Code    string `json:"code"`
	Control bool   `json:"control"`
	Key     string `json:"key"`
	Meta    bool   `json:"meta"`
	Repeat  bool   `json:"repeat"`
	Shift   bool   `json:"shift"`
}

// Renderer payload of a forwarded F12 press
func ToF12KeyboardEventPayload(input KeyInput) F12KeyboardEvent {
	return F12KeyboardEvent{Alt: input.Alt, Code: input.Code, Control: input.Control, Key: input.Key, Meta: input.Meta, Repeat: input.IsAutoRepeat, Shift: input.Shift}
}

// Ctrl+Shift+I, Cmd+Opt+I on macOS, or F12 toggles DevTools
func isInspectShortcut(input KeyInput, isMac bool) bool {
	key := strings.ToLower(input.Key)
	return input.Key == "F12" || (isMac && input.Meta && input.Alt && key == "i") || (!isMac && input.Control && input.Shift && key == "i")
}

// Ctrl+F, or Cmd+F and Ctrl+F on macOS, without Alt or Shift
func IsFindChord(input KeyInput, isMac bool) bool {
	hasMod := input.Control
	if isMac {
		hasMod = input.Meta || input.Control
	}
	return strings.ToLower(input.Key) == "f" && hasMod && !input.Alt && !input.Shift
}
