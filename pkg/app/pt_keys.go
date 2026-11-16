package app

import (
	"regexp"
	"sync"

	tea "charm.land/bubbletea/v2"
)

const (
	ptKeyEscape         = "escape"
	ptKeyEnter          = "c-m"
	ptKeyTab            = "c-i"
	ptKeyBackspace      = "c-h"
	ptKeyBackTab        = "s-tab"
	ptKeyAny            = "<any>"
	ptKeyIgnore         = "<ignore>"
	ptKeyBracketedPaste = "<bracketed-paste>"
	ptKeyCPRResponse    = "<cursor-position-response>"
	ptKeyVT100Mouse     = "<vt100-mouse-event>"
)

// Binding spellings that name another key
var ptKeyAliases = map[string]string{
	"backspace": "c-h", "c-space": "c-@", "enter": "c-m", "tab": "c-i", "space": " ",
	"s-c-left": "c-s-left", "s-c-right": "c-s-right", "s-c-home": "c-s-home", "s-c-end": "c-s-end"}

// ptKeyPress is one parsed key with the raw text that produced it
type ptKeyPress struct{ Key, Data string }

var (
	ansiSequencesMu         sync.RWMutex
	ansiSequences           = stockANSISequences()
	vt100PrefixCache        = map[string]bool{}
	keypressDataNormalized  bool
	cprResponseRe           = regexp.MustCompile(`^\x1b\[\d+;\d+R$`)
	mouseEventRe            = regexp.MustCompile(`(?s)^\x1b\[(<?[\d;]+[mM]|M...)$`)
	cprResponsePrefixRe     = regexp.MustCompile(`^\x1b\[[\d;]*$`)
	mouseEventPrefixRe      = regexp.MustCompile(`(?s)^\x1b\[(<?[\d;]*|M.{0,2})$`)
	lockBitOffsets          = []int{0, 64, 128, 192}
	kittyFunctionalNamed    = []kittyNamedKey{{57409, "."}, {57410, "/"}, {57411, "*"}, {57412, "-"}, {57413, "+"}, {57414, "c-m"}, {57415, "="}, {57416, ","}, {57417, "left"}, {57418, "right"}, {57419, "up"}, {57420, "down"}, {57421, "pageup"}, {57422, "pagedown"}, {57423, "home"}, {57424, "end"}, {57425, "insert"}, {57426, "delete"}}
	kittyFunctionalIgnored  = kittyIgnoredCodes()
	ptNamedKeys             = ptNamedKeySet()
	keypressNormalizationMu sync.Mutex
)

// kittyNamedKey pairs a kitty private use codepoint with the key it stands for
type kittyNamedKey struct {
	Code int
	Key  string
}

// Kitty codepoints of lock, media and bare modifier keys the prompt consumes silently
func kittyIgnoredCodes() []int {
	var out []int
	for c := 57358; c < 57364; c++ {
		out = append(out, c)
	}
	for c := 57388; c < 57399; c++ {
		out = append(out, c)
	}
	out = append(out, 57427)
	for c := 57428; c < 57455; c++ {
		out = append(out, c)
	}
	return out
}

// Every multi character key name of the stock table, the names that are keys and never typed text
func ptNamedKeySet() map[string]bool {
	out := map[string]bool{ptKeyAny: true, ptKeyIgnore: true, ptKeyBracketedPaste: true, ptKeyCPRResponse: true, ptKeyVT100Mouse: true}
	for _, keys := range stockANSISequences() {
		for _, k := range keys {
			out[k] = true
		}
	}
	return out
}

// aliasSet collects sequence aliases in first writer wins order
type aliasSet struct {
	order []string
	keys  map[string][]string
}

var promptKeyAliasesOnce sync.Once

// vt100Parser turns terminal input bytes into key presses through the sequence table
type vt100Parser struct {
	feedKey          func(ptKeyPress)
	prefix           string
	inBracketedPaste bool
	pasteBuffer      string
	pasteStart       *float64
}

// Forgets pending input and paste state
func (p *vt100Parser) reset() {
	p.prefix, p.inBracketedPaste, p.pasteBuffer, p.pasteStart = "", false, "", nil
}

// Legacy CSI final letters of cursor and function keys
var ptLegacyLetterKeys = map[rune]rune{tea.KeyUp: 'A', tea.KeyDown: 'B', tea.KeyRight: 'C', tea.KeyLeft: 'D', tea.KeyEnd: 'F', tea.KeyHome: 'H', tea.KeyF1: 'P', tea.KeyF2: 'Q', tea.KeyF3: 'R', tea.KeyF4: 'S'}

// Legacy CSI tilde numbers of navigation and function keys
var ptLegacyTildeKeys = map[rune]int{tea.KeyInsert: 2, tea.KeyDelete: 3, tea.KeyPgUp: 5, tea.KeyPgDown: 6, tea.KeyF5: 15, tea.KeyF6: 17, tea.KeyF7: 18, tea.KeyF8: 19, tea.KeyF9: 20, tea.KeyF10: 21, tea.KeyF11: 23, tea.KeyF12: 24}

// Kitty private use codepoints of keypad, lock, media and modifier keys
var ptKittyCodes = kittyCodeTable()

// Builds the key code to kitty codepoint table
func kittyCodeTable() map[rune]int {
	out := map[rune]int{}
	run := func(first int, keys ...rune) {
		for i, k := range keys {
			out[k] = first + i
		}
	}
	run(57358, tea.KeyCapsLock, tea.KeyScrollLock, tea.KeyNumLock, tea.KeyPrintScreen, tea.KeyPause, tea.KeyMenu)
	run(57376, tea.KeyF13, tea.KeyF14, tea.KeyF15, tea.KeyF16, tea.KeyF17, tea.KeyF18, tea.KeyF19, tea.KeyF20, tea.KeyF21, tea.KeyF22, tea.KeyF23, tea.KeyF24,
		tea.KeyF25, tea.KeyF26, tea.KeyF27, tea.KeyF28, tea.KeyF29, tea.KeyF30, tea.KeyF31, tea.KeyF32, tea.KeyF33, tea.KeyF34, tea.KeyF35)
	run(57399, tea.KeyKp0, tea.KeyKp1, tea.KeyKp2, tea.KeyKp3, tea.KeyKp4, tea.KeyKp5, tea.KeyKp6, tea.KeyKp7, tea.KeyKp8, tea.KeyKp9,
		tea.KeyKpDecimal, tea.KeyKpDivide, tea.KeyKpMultiply, tea.KeyKpMinus, tea.KeyKpPlus, tea.KeyKpEnter, tea.KeyKpEqual, tea.KeyKpSep,
		tea.KeyKpLeft, tea.KeyKpRight, tea.KeyKpUp, tea.KeyKpDown, tea.KeyKpPgUp, tea.KeyKpPgDown, tea.KeyKpHome, tea.KeyKpEnd, tea.KeyKpInsert, tea.KeyKpDelete, tea.KeyKpBegin)
	run(57428, tea.KeyMediaPlay, tea.KeyMediaPause, tea.KeyMediaPlayPause, tea.KeyMediaReverse, tea.KeyMediaStop, tea.KeyMediaFastForward, tea.KeyMediaRewind,
		tea.KeyMediaNext, tea.KeyMediaPrev, tea.KeyMediaRecord, tea.KeyLowerVol, tea.KeyRaiseVol, tea.KeyMute,
		tea.KeyLeftShift, tea.KeyLeftCtrl, tea.KeyLeftAlt, tea.KeyLeftSuper, tea.KeyLeftHyper, tea.KeyLeftMeta,
		tea.KeyRightShift, tea.KeyRightCtrl, tea.KeyRightAlt, tea.KeyRightSuper, tea.KeyRightHyper, tea.KeyRightMeta, tea.KeyIsoLevel3Shift, tea.KeyIsoLevel5Shift)
	return out
}
