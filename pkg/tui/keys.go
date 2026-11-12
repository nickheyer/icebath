package tui

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Host platform is macOS, where the action modifier is Cmd instead of Ctrl
var isMac = runtime.GOOS == "darwin"

// Environment lookups the platform probes read, the process environment in production
var getenv = os.Getenv

// Modifier and named key flags of one keypress
type inkKey struct {
	UpArrow, DownArrow, LeftArrow, RightArrow bool
	PageUp, PageDown, Home, End               bool
	Return, Escape, Tab, Backspace, Delete    bool
	Ctrl, Shift, Meta, Super                  bool
	WheelUp, WheelDown                        bool
}

// One decoded keypress with its text, name, raw bytes and paste origin
type inputEvent struct {
	Input          string
	Key            inkKey
	Name           string
	Raw            string
	Sequence       string
	IsPasted       bool
	IsControlChord bool
}

// Keypress of a bubbletea key message in the shape the composer and global handlers branch on
func keyEvent(msg tea.KeyPressMsg) inputEvent {
	k := msg.Key()
	ev := inputEvent{}
	ev.Key.Ctrl = k.Mod.Contains(tea.ModCtrl)
	ev.Key.Shift = k.Mod.Contains(tea.ModShift)
	ev.Key.Meta = k.Mod.Contains(tea.ModAlt) || k.Mod.Contains(tea.ModMeta)
	ev.Key.Super = k.Mod.Contains(tea.ModSuper)
	switch k.Code {
	case tea.KeyUp:
		ev.Key.UpArrow, ev.Name = true, "up"
	case tea.KeyDown:
		ev.Key.DownArrow, ev.Name = true, "down"
	case tea.KeyLeft:
		ev.Key.LeftArrow, ev.Name = true, "left"
	case tea.KeyRight:
		ev.Key.RightArrow, ev.Name = true, "right"
	case tea.KeyPgUp:
		ev.Key.PageUp, ev.Name = true, "pageup"
	case tea.KeyPgDown:
		ev.Key.PageDown, ev.Name = true, "pagedown"
	case tea.KeyHome:
		ev.Key.Home, ev.Name = true, "home"
	case tea.KeyEnd:
		ev.Key.End, ev.Name = true, "end"
	case tea.KeyEnter:
		ev.Key.Return, ev.Name, ev.Input, ev.Sequence = true, "return", "\r", "\r"
	case tea.KeyEscape:
		ev.Key.Escape, ev.Name = true, "escape"
	case tea.KeyTab:
		ev.Key.Tab, ev.Name = true, "tab"
	case tea.KeyBackspace:
		ev.Key.Backspace, ev.Name = true, "backspace"
	case tea.KeyDelete:
		ev.Key.Delete, ev.Name, ev.Raw = true, "delete", "\x1b[3~"
	case tea.KeySpace:
		ev.Name, ev.Input = "space", " "
	default:
		if k.Code >= tea.KeyF1 && k.Code <= tea.KeyF20 {
			ev.Name = "f" + strconv.Itoa(int(k.Code-tea.KeyF1)+1)
			break
		}
		if ev.Key.Ctrl && k.Code == 'j' && !ev.Key.Meta && !ev.Key.Super && !ev.Key.Shift {
			ev.Key.Ctrl = false
			ev.Key.Return, ev.Name, ev.Input, ev.Sequence = true, "return", "\r", "\n"
			break
		}
		if ev.Key.Ctrl && k.Code == 'h' && !ev.Key.Meta && !ev.Key.Super {
			ev.Key.Ctrl = false
			ev.Key.Backspace, ev.Name = true, "backspace"
			break
		}
		if ev.Key.Ctrl && k.Code == 'v' {
			ev.Raw = "\x16"
		}
		if ev.Key.Meta && (k.Code == 'v' || k.Code == 'V') && !ev.Key.Ctrl {
			ev.Raw = "\x1b" + string(k.Code)
		}
		if k.Text != "" && !ev.Key.Ctrl && !ev.Key.Meta && !ev.Key.Super {
			ev.Input = k.Text
		} else {
			ev.Input = string(k.Code)
			if ev.Key.Shift && k.ShiftedCode != 0 {
				ev.Input = string(k.ShiftedCode)
			}
		}
		ev.Name = strings.ToLower(string(k.Code))
	}
	ev.IsControlChord = ev.Key.Ctrl && !ev.IsPasted
	return ev
}

// Bracketed paste text as one pasted keypress
func pasteEvent(text string) inputEvent {
	return inputEvent{Input: text, IsPasted: true}
}

// Platform action modifier, Cmd or Option on macOS and Ctrl elsewhere
func isActionMod(k inkKey) bool {
	if isMac {
		return k.Meta || k.Super
	}
	return k.Ctrl
}

// Ctrl chord a macOS terminal sends where the user pressed the Cmd shortcut for target
func isMacActionFallback(k inkKey, ch, target string) bool {
	return isMac && k.Ctrl && !k.Meta && !k.Super && strings.ToLower(ch) == target
}

// Action modifier held with the target letter
func isAction(k inkKey, ch, target string) bool {
	return isActionMod(k) && strings.ToLower(ch) == target
}
