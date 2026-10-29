package term

import (
	"strconv"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Key event from the host, codes as ultraviolet names them
type Key struct {
	Code    rune
	Shifted rune
	Base    rune
	Text    string
	Mod     uv.KeyMod
	Repeat  bool
	Release bool
}

// Modes and flags a key is encoded under
type keyState struct {
	cursorKeys  bool
	keypadApp   bool
	newline     bool
	backspaceBS bool
	kitty       int
	modify      int
}

// Bytes the program receives for the key, none when the key sends nothing
func (e *Emulator) Key(k Key) []byte {
	e.mu.Lock()
	st := keyState{cursorKeys: e.modes.cursorKeys, keypadApp: e.modes.keypadApp, newline: e.modes.newline, backspaceBS: e.modes.backspaceBS, kitty: e.kittyFlags(), modify: e.modifyKeys}
	locked := e.modes.lockKeyboard
	e.mu.Unlock()
	if locked {
		return nil
	}
	return encodeKey(k, st)
}

// Bytes the program receives for pasted text, bracketed when it asked for that
func (e *Emulator) Paste(text string) []byte {
	e.mu.Lock()
	bracketed := e.modes.bracketedPaste
	e.mu.Unlock()
	if bracketed {
		text = strings.ReplaceAll(text, "\x1b[201~", "")
		return []byte("\x1b[200~" + cleanPaste(text, true) + "\x1b[201~")
	}
	return []byte(cleanPaste(text, false))
}

// Pasted text without control characters, line breaks kept inside brackets and sent as returns otherwise
func cleanPaste(text string, bracketed bool) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var sb strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' && !bracketed:
			sb.WriteByte('\r')
		case r == '\n' || r == '\t' || r == '\r':
			sb.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			continue
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// Bytes the program receives when the terminal gains or loses focus, none unless it asked
func (e *Emulator) Focus(in bool) []byte {
	e.mu.Lock()
	want := e.modes.focus
	e.mu.Unlock()
	if !want {
		return nil
	}
	if in {
		return []byte("\x1b[I")
	}
	return []byte("\x1b[O")
}

// Keypad keys folded onto the keys they stand for outside application keypad mode
var keypadPlain = map[rune]rune{
	uv.KeyKpEnter: uv.KeyEnter, uv.KeyKpUp: uv.KeyUp, uv.KeyKpDown: uv.KeyDown, uv.KeyKpLeft: uv.KeyLeft, uv.KeyKpRight: uv.KeyRight,
	uv.KeyKpPgUp: uv.KeyPgUp, uv.KeyKpPgDown: uv.KeyPgDown, uv.KeyKpHome: uv.KeyHome, uv.KeyKpEnd: uv.KeyEnd, uv.KeyKpInsert: uv.KeyInsert,
	uv.KeyKpDelete: uv.KeyDelete, uv.KeyKpBegin: uv.KeyBegin, uv.KeyKpEqual: '=', uv.KeyKpMultiply: '*', uv.KeyKpPlus: '+', uv.KeyKpComma: ',',
	uv.KeyKpMinus: '-', uv.KeyKpDecimal: '.', uv.KeyKpDivide: '/', uv.KeyKp0: '0', uv.KeyKp1: '1', uv.KeyKp2: '2', uv.KeyKp3: '3', uv.KeyKp4: '4',
	uv.KeyKp5: '5', uv.KeyKp6: '6', uv.KeyKp7: '7', uv.KeyKp8: '8', uv.KeyKp9: '9',
}

// Final bytes of the SS3 sequences keypad keys send in application keypad mode
var keypadApp = map[rune]byte{
	uv.KeyKpEnter: 'M', uv.KeyKpEqual: 'X', uv.KeyKpMultiply: 'j', uv.KeyKpPlus: 'k', uv.KeyKpComma: 'l', uv.KeyKpMinus: 'm', uv.KeyKpDecimal: 'n',
	uv.KeyKpDivide: 'o', uv.KeyKp0: 'p', uv.KeyKp1: 'q', uv.KeyKp2: 'r', uv.KeyKp3: 's', uv.KeyKp4: 't', uv.KeyKp5: 'u', uv.KeyKp6: 'v', uv.KeyKp7: 'w',
	uv.KeyKp8: 'x', uv.KeyKp9: 'y',
}

// Final letters of cursor style keys
var cursorKeys = map[rune]byte{uv.KeyUp: 'A', uv.KeyDown: 'B', uv.KeyRight: 'C', uv.KeyLeft: 'D', uv.KeyHome: 'H', uv.KeyEnd: 'F', uv.KeyBegin: 'E'}

// Numbers of tilde style keys
var tildeKeys = map[rune]int{
	uv.KeyFind: 1, uv.KeyInsert: 2, uv.KeyDelete: 3, uv.KeySelect: 4, uv.KeyPgUp: 5, uv.KeyPgDown: 6,
	uv.KeyF5: 15, uv.KeyF6: 17, uv.KeyF7: 18, uv.KeyF8: 19, uv.KeyF9: 20, uv.KeyF10: 21, uv.KeyF11: 23, uv.KeyF12: 24,
	uv.KeyF13: 25, uv.KeyF14: 26, uv.KeyF15: 28, uv.KeyF16: 29, uv.KeyF17: 31, uv.KeyF18: 32, uv.KeyF19: 33, uv.KeyF20: 34,
}

// Final letters of the first four function keys
var fnKeys = map[rune]byte{uv.KeyF1: 'P', uv.KeyF2: 'Q', uv.KeyF3: 'R', uv.KeyF4: 'S'}

// Kitty key codes of keys without a legacy form
var kittyCodes = map[rune]int{
	uv.KeyCapsLock: 57358, uv.KeyScrollLock: 57359, uv.KeyNumLock: 57360, uv.KeyPrintScreen: 57361, uv.KeyPause: 57362, uv.KeyMenu: 57363,
	uv.KeyF13: 57376, uv.KeyF14: 57377, uv.KeyF15: 57378, uv.KeyF16: 57379, uv.KeyF17: 57380, uv.KeyF18: 57381, uv.KeyF19: 57382, uv.KeyF20: 57383,
	uv.KeyF21: 57384, uv.KeyF22: 57385, uv.KeyF23: 57386, uv.KeyF24: 57387, uv.KeyF25: 57388, uv.KeyF26: 57389, uv.KeyF27: 57390, uv.KeyF28: 57391,
	uv.KeyF29: 57392, uv.KeyF30: 57393, uv.KeyF31: 57394, uv.KeyF32: 57395, uv.KeyF33: 57396, uv.KeyF34: 57397, uv.KeyF35: 57398,
	uv.KeyKp0: 57399, uv.KeyKp1: 57400, uv.KeyKp2: 57401, uv.KeyKp3: 57402, uv.KeyKp4: 57403, uv.KeyKp5: 57404, uv.KeyKp6: 57405, uv.KeyKp7: 57406,
	uv.KeyKp8: 57407, uv.KeyKp9: 57408, uv.KeyKpDecimal: 57409, uv.KeyKpDivide: 57410, uv.KeyKpMultiply: 57411, uv.KeyKpMinus: 57412, uv.KeyKpPlus: 57413,
	uv.KeyKpEnter: 57414, uv.KeyKpEqual: 57415, uv.KeyKpSep: 57416, uv.KeyKpLeft: 57417, uv.KeyKpRight: 57418, uv.KeyKpUp: 57419, uv.KeyKpDown: 57420,
	uv.KeyKpPgUp: 57421, uv.KeyKpPgDown: 57422, uv.KeyKpHome: 57423, uv.KeyKpEnd: 57424, uv.KeyKpInsert: 57425, uv.KeyKpDelete: 57426, uv.KeyKpBegin: 57427,
	uv.KeyMediaPlay: 57428, uv.KeyMediaPause: 57429, uv.KeyMediaPlayPause: 57430, uv.KeyMediaReverse: 57431, uv.KeyMediaStop: 57432, uv.KeyMediaFastForward: 57433,
	uv.KeyMediaRewind: 57434, uv.KeyMediaNext: 57435, uv.KeyMediaPrev: 57436, uv.KeyMediaRecord: 57437, uv.KeyLowerVol: 57438, uv.KeyRaiseVol: 57439, uv.KeyMute: 57440,
	uv.KeyLeftShift: 57441, uv.KeyLeftCtrl: 57442, uv.KeyLeftAlt: 57443, uv.KeyLeftSuper: 57444, uv.KeyLeftHyper: 57445, uv.KeyLeftMeta: 57446,
	uv.KeyRightShift: 57447, uv.KeyRightCtrl: 57448, uv.KeyRightAlt: 57449, uv.KeyRightSuper: 57450, uv.KeyRightHyper: 57451, uv.KeyRightMeta: 57452,
	uv.KeyIsoLevel3Shift: 57453, uv.KeyIsoLevel5Shift: 57454,
}

// Reports whether the code is a modifier key on its own
func modifierKey(code rune) bool {
	return code >= uv.KeyLeftShift && code <= uv.KeyIsoLevel5Shift
}

// Modifier parameter of xterm style sequences, one plus the modifier bits
func xtermMod(mod uv.KeyMod) int {
	n := 1
	if mod&uv.ModShift != 0 {
		n += 1
	}
	if mod&(uv.ModAlt|uv.ModMeta) != 0 {
		n += 2
	}
	if mod&uv.ModCtrl != 0 {
		n += 4
	}
	return n
}

// Modifier parameter of kitty sequences, one plus the kitty modifier bits
func kittyMod(mod uv.KeyMod) int {
	n := 1
	pairs := []struct {
		m   uv.KeyMod
		bit int
	}{{uv.ModShift, 1}, {uv.ModAlt, 2}, {uv.ModCtrl, 4}, {uv.ModSuper, 8}, {uv.ModHyper, 16}, {uv.ModMeta, 32}, {uv.ModCapsLock, 64}, {uv.ModNumLock, 128}}
	for _, p := range pairs {
		if mod&p.m != 0 {
			n += p.bit
		}
	}
	return n
}

// Encodes a key for the program under the modes in force
func encodeKey(k Key, st keyState) []byte {
	if k.Release && st.kitty&ansi.KittyReportEventTypes == 0 {
		return nil
	}
	if st.kitty != 0 {
		return kittyEncode(k, st)
	}
	if k.Release {
		return nil
	}
	return legacyEncode(k, st)
}

// Control byte a printable key sends with ctrl held, false when it has none
func ctrlByte(r rune) (byte, bool) {
	r = unicode.ToLower(r)
	switch {
	case r >= 'a' && r <= 'z':
		return byte(r) & 0x1f, true
	}
	switch r {
	case ' ', '@', '2':
		return 0, true
	case '[', '3':
		return 0x1b, true
	case '\\', '4':
		return 0x1c, true
	case ']', '5':
		return 0x1d, true
	case '^', '6', '~':
		return 0x1e, true
	case '_', '7', '/', '-':
		return 0x1f, true
	case '?', '8':
		return 0x7f, true
	}
	return 0, false
}

// Encodes a key the xterm way, with modifyOtherKeys forms when that is turned on
func legacyEncode(k Key, st keyState) []byte {
	mod := k.Mod &^ (uv.ModCapsLock | uv.ModNumLock | uv.ModScrollLock)
	alt := mod&(uv.ModAlt|uv.ModMeta) != 0
	ctrl := mod&uv.ModCtrl != 0
	shift := mod&uv.ModShift != 0
	xm := xtermMod(mod)
	code := k.Code
	if plain, ok := keypadPlain[code]; ok && !st.keypadApp {
		code = plain
	}
	other := func(key rune) []byte {
		return []byte("\x1b[27;" + strconv.Itoa(xm) + ";" + strconv.Itoa(int(key)) + "~")
	}
	prefix := func(b []byte) []byte {
		if alt {
			return append([]byte{0x1b}, b...)
		}
		return b
	}
	if letter, ok := cursorKeys[code]; ok {
		if xm > 1 {
			return []byte("\x1b[1;" + strconv.Itoa(xm) + string(letter))
		}
		if st.cursorKeys {
			return []byte{0x1b, 'O', letter}
		}
		return []byte{0x1b, '[', letter}
	}
	if n, ok := tildeKeys[code]; ok {
		if xm > 1 {
			return []byte("\x1b[" + strconv.Itoa(n) + ";" + strconv.Itoa(xm) + "~")
		}
		return []byte("\x1b[" + strconv.Itoa(n) + "~")
	}
	if letter, ok := fnKeys[code]; ok {
		if xm > 1 {
			return []byte("\x1b[1;" + strconv.Itoa(xm) + string(letter))
		}
		return []byte{0x1b, 'O', letter}
	}
	if final, ok := keypadApp[code]; ok && st.keypadApp {
		if xm > 1 {
			return []byte("\x1bO" + strconv.Itoa(xm) + string(final))
		}
		return []byte{0x1b, 'O', final}
	}
	switch code {
	case uv.KeyTab:
		switch {
		case (ctrl || alt) && st.modify > 0:
			return other('\t')
		case shift:
			return prefix([]byte("\x1b[Z"))
		}
		return prefix([]byte{'\t'})
	case uv.KeyEnter:
		if (ctrl || shift) && st.modify > 0 {
			return other('\r')
		}
		if st.newline {
			return prefix([]byte("\r\n"))
		}
		return prefix([]byte{'\r'})
	case uv.KeyBackspace:
		b := byte(0x7f)
		if st.backspaceBS {
			b = 0x08
		}
		if ctrl {
			if st.modify > 0 {
				return other(0x7f)
			}
			b ^= 0x7f ^ 0x08
		}
		return prefix([]byte{b})
	case uv.KeyEscape:
		if (ctrl || shift) && st.modify > 0 {
			return other(0x1b)
		}
		return prefix([]byte{0x1b})
	case uv.KeySpace:
		if ctrl {
			if st.modify > 0 {
				return other(' ')
			}
			return prefix([]byte{0})
		}
		return prefix([]byte{' '})
	}
	if modifierKey(code) || code > unicode.MaxRune {
		return nil
	}
	key := code
	if shift && unicode.IsLetter(key) {
		key = unicode.ToUpper(key)
	}
	if ctrl {
		if st.modify >= 2 {
			return other(key)
		}
		if b, ok := ctrlByte(code); ok && !shift {
			return prefix([]byte{b})
		}
		if st.modify >= 1 {
			return other(key)
		}
		return prefix([]byte(string(key)))
	}
	if alt && st.modify >= 2 {
		return other(key)
	}
	text := k.Text
	if text == "" {
		text = string(key)
	}
	return prefix([]byte(text))
}

// Encodes a key under the kitty keyboard protocol with the flags in force
func kittyEncode(k Key, st keyState) []byte {
	flags := st.kitty
	mod := k.Mod &^ uv.ModScrollLock
	plainMods := mod &^ (uv.ModShift | uv.ModCapsLock | uv.ModNumLock)
	event := 1
	switch {
	case k.Release:
		event = 3
	case k.Repeat && flags&ansi.KittyReportEventTypes != 0:
		event = 2
	}
	all := flags&ansi.KittyReportAllKeysAsEscapeCodes != 0
	code := k.Code
	text := k.Text
	textKey := code <= unicode.MaxRune && unicode.IsPrint(code) && !modifierKey(code)
	if n, ok := kittyCodes[code]; ok && (text == "" || all || plainMods != 0) {
		return kittyCSI(n, 0, 0, 'u', mod, event, "", flags)
	}
	if textKey && !all && plainMods == 0 {
		if event == 3 {
			return kittyCSI(int(kittyKey(k)), 0, 0, 'u', mod, event, "", flags)
		}
		if text == "" {
			text = string(code)
		}
		return []byte(text)
	}
	switch code {
	case uv.KeyEnter, uv.KeyTab, uv.KeyBackspace:
		if !all && mod == 0 && event == 1 {
			return legacyEncode(k, st)
		}
		if code == uv.KeyBackspace {
			return kittyCSI(127, 0, 0, 'u', mod, event, "", flags)
		}
		return kittyCSI(int(code), 0, 0, 'u', mod, event, "", flags)
	case uv.KeyEscape:
		return kittyCSI(27, 0, 0, 'u', mod, event, "", flags)
	case uv.KeyF3:
		return kittyCSI(13, 0, 0, '~', mod, event, "", flags)
	}
	if letter, ok := cursorKeys[code]; ok {
		return kittyCSI(1, 0, 0, letter, mod, event, "", flags)
	}
	if n, ok := tildeKeys[code]; ok && code < uv.KeyF13 {
		return kittyCSI(n, 0, 0, '~', mod, event, "", flags)
	}
	if letter, ok := fnKeys[code]; ok {
		return kittyCSI(1, 0, 0, letter, mod, event, "", flags)
	}
	if n, ok := kittyCodes[code]; ok {
		return kittyCSI(n, 0, 0, 'u', mod, event, "", flags)
	}
	if !textKey {
		return nil
	}
	key := kittyKey(k)
	shifted, base := rune(0), rune(0)
	if flags&ansi.KittyReportAlternateKeys != 0 {
		if mod&uv.ModShift != 0 && k.Shifted != 0 && k.Shifted != key {
			shifted = k.Shifted
		}
		if k.Base != 0 && k.Base != key {
			base = k.Base
		}
	}
	assoc := ""
	if flags&ansi.KittyReportAssociatedKeys != 0 && plainMods&(uv.ModCtrl|uv.ModAlt|uv.ModSuper|uv.ModHyper|uv.ModMeta) == 0 && event != 3 {
		assoc = text
		if assoc == "" && mod&^(uv.ModShift|uv.ModCapsLock|uv.ModNumLock) == 0 {
			assoc = string(key)
		}
	}
	return kittyCSI(int(key), shifted, base, 'u', mod, event, assoc, flags)
}

// Unicode key code the kitty protocol names a text key by, letters lower case
func kittyKey(k Key) rune {
	if unicode.IsUpper(k.Code) && k.Mod&uv.ModShift != 0 {
		return unicode.ToLower(k.Code)
	}
	if unicode.IsUpper(k.Code) && k.Base != 0 {
		return k.Base
	}
	return k.Code
}

// Kitty sequence with the key and alternates, the modifiers with the event type, the text and the final byte
func kittyCSI(code int, shifted, base rune, final byte, mod uv.KeyMod, event int, text string, flags int) []byte {
	var sb strings.Builder
	sb.WriteString("\x1b[")
	km := kittyMod(mod)
	modField := ""
	if km > 1 || event > 1 {
		modField = strconv.Itoa(km)
		if event > 1 {
			modField += ":" + strconv.Itoa(event)
		}
	}
	textField := ""
	if text != "" {
		parts := make([]string, 0, len(text))
		for _, r := range text {
			parts = append(parts, strconv.Itoa(int(r)))
		}
		textField = strings.Join(parts, ":")
	}
	keyField := strconv.Itoa(code)
	if shifted != 0 || base != 0 {
		keyField += ":"
		if shifted != 0 {
			keyField += strconv.Itoa(int(shifted))
		}
		if base != 0 {
			keyField += ":" + strconv.Itoa(int(base))
		}
	}
	if final != 'u' && final != '~' {
		if modField == "" {
			sb.WriteByte(final)
			return []byte(sb.String())
		}
		sb.WriteString("1;" + modField)
		sb.WriteByte(final)
		return []byte(sb.String())
	}
	sb.WriteString(keyField)
	if modField != "" || textField != "" {
		sb.WriteByte(';')
		sb.WriteString(modField)
	}
	if textField != "" {
		sb.WriteByte(';')
		sb.WriteString(textField)
	}
	sb.WriteByte(final)
	return []byte(sb.String())
}
