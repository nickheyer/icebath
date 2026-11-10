package app

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

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

// Canonical key name of a binding spelling
func ptParseKey(key string) string {
	if alias, ok := ptKeyAliases[key]; ok {
		return alias
	}
	return key
}

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

// The modifier with its CapsLock, NumLock and both lock twins
func lockVariants(modifier int) []int {
	out := make([]int, 0, len(lockBitOffsets))
	for _, off := range lockBitOffsets {
		out = append(out, modifier+off)
	}
	return out
}

// Only the lock twins of the modifier
func lockTwins(modifier int) []int { return lockVariants(modifier)[1:] }

// Drops the memoized prefix answers after the sequence table changed
func clearVT100PrefixCache() {
	vt100PrefixCache = map[string]bool{}
}

// aliasSet collects sequence aliases in first writer wins order
type aliasSet struct {
	order []string
	keys  map[string][]string
}

// Adds an alias unless the sequence already has one in the set
func (a *aliasSet) put(seq string, keys ...string) {
	if a.keys == nil {
		a.keys = map[string][]string{}
	}
	if _, ok := a.keys[seq]; ok {
		return
	}
	a.keys[seq] = keys
	a.order = append(a.order, seq)
}

// Replaces or adds an alias in the set
func (a *aliasSet) set(seq string, keys ...string) {
	if a.keys == nil {
		a.keys = map[string][]string{}
	}
	if _, ok := a.keys[seq]; !ok {
		a.order = append(a.order, seq)
	}
	a.keys[seq] = keys
}

// Reports whether two key lists are the same
func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Installs built aliases into the sequence table and returns how many entries changed
func installANSIAliases(build func(table map[string][]string) *aliasSet, overwrite bool) int {
	ansiSequencesMu.Lock()
	defer ansiSequencesMu.Unlock()
	changed := 0
	set := build(ansiSequences)
	for _, seq := range set.order {
		have, ok := ansiSequences[seq]
		if (overwrite && !(ok && sameKeys(have, set.keys[seq]))) || (!overwrite && !ok) {
			ansiSequences[seq] = set.keys[seq]
			changed++
		}
	}
	if changed > 0 {
		clearVT100PrefixCache()
	}
	return changed
}

// Makes a plain character mapped from an extended sequence carry that character as its typed text
func installKeypressDataNormalization() int {
	keypressNormalizationMu.Lock()
	defer keypressNormalizationMu.Unlock()
	if keypressDataNormalized {
		return 0
	}
	keypressDataNormalized = true
	return 1
}

// Maps the modifier plus Enter in its kitty and xterm spellings to the Alt Enter newline keys
func installEnterAlias(modifier int) int {
	return installANSIAliases(func(map[string][]string) *aliasSet {
		set := &aliasSet{}
		for _, m := range lockVariants(modifier) {
			set.set(fmt.Sprintf("\x1b[13;%du", m), ptKeyEscape, ptKeyEnter)
		}
		set.set(fmt.Sprintf("\x1b[27;%d;13~", modifier), ptKeyEscape, ptKeyEnter)
		set.set(fmt.Sprintf("\x1b[27;%d;13u", modifier), ptKeyEscape, ptKeyEnter)
		return set
	}, true)
}

// Maps Shift Enter to the Alt Enter newline keys
func installShiftEnterAlias() int { return installEnterAlias(2) }

// Maps Ctrl Enter to the Alt Enter newline keys
func installCtrlEnterAlias() int { return installEnterAlias(5) }

// Maps Cmd Backspace to line discard and Cmd forward delete to kill line
func installCmdBackspaceAlias() int {
	return installANSIAliases(func(map[string][]string) *aliasSet {
		set := &aliasSet{}
		var mods []int
		for _, base := range []int{9, 10} {
			mods = append(mods, lockVariants(base)...)
		}
		for _, mod := range mods {
			set.set(fmt.Sprintf("\x1b[127;%du", mod), "c-u")
		}
		for _, mod := range mods {
			set.set(fmt.Sprintf("\x1b[3;%d~", mod), "c-k")
		}
		set.set("\x1b[27;9;127~", "c-u")
		return set
	}, true)
}

// Kitty functional codepoints in table order with the key each one maps to
func kittyFunctionalMap() []kittyNamedKey {
	var out []kittyNamedKey
	seen := map[int]bool{}
	add := func(code int, key string) {
		if !seen[code] {
			seen[code] = true
			out = append(out, kittyNamedKey{code, key})
		}
	}
	for d := 0; d < 10; d++ {
		add(57399+d, string(rune('0'+d)))
	}
	for _, named := range kittyFunctionalNamed {
		add(named.Code, named.Key)
	}
	for n := 13; n < 25; n++ {
		add(57376+(n-13), fmt.Sprintf("f%d", n))
	}
	for _, code := range kittyFunctionalIgnored {
		add(code, ptKeyIgnore)
	}
	return out
}

// Maps modifyOtherKeys and kitty Ctrl, Alt and Shift key sequences to the keys their raw bytes produce
func installModifyOtherKeysAliases() int {
	return installANSIAliases(modifyOtherKeysAliases, false)
}

// Builds the modifyOtherKeys and kitty alias set against the current sequence table
func modifyOtherKeysAliases(table map[string][]string) *aliasSet {
	aliases := &aliasSet{}
	type pair struct {
		cp   int
		keys []string
	}
	installPaired := func(modifier int, mapping []pair) {
		for _, p := range mapping {
			if modifier != 1 {
				aliases.put(fmt.Sprintf("\x1b[27;%d;%d~", modifier, p.cp), p.keys...)
			}
			for _, mod := range lockVariants(modifier) {
				aliases.put(fmt.Sprintf("\x1b[%d;%du", p.cp, mod), p.keys...)
			}
		}
	}
	ctrlKeys := map[int][]string{}
	var ctrlOrder []pair
	var ctrlCodes []int
	for cp := int('a'); cp <= int('z'); cp++ {
		ctrlCodes = append(ctrlCodes, cp)
	}
	ctrlCodes = append(ctrlCodes, 64, 91, 92, 93, 94, 95, 32)
	for _, cp := range ctrlCodes {
		if keys, ok := table[string(rune(cp&0x1F))]; ok {
			ctrlKeys[cp] = keys
			ctrlOrder = append(ctrlOrder, pair{cp, keys})
		}
	}
	for d := 0; d < 10; d++ {
		keys := []string{fmt.Sprintf("c-%d", d)}
		ctrlKeys[int('0')+d] = keys
		ctrlOrder = append(ctrlOrder, pair{int('0') + d, keys})
	}
	installPaired(5, ctrlOrder)

	for ch := int('a'); ch <= int('z'); ch++ {
		upper := string(rune(ch - 32))
		ctrlKey, hasCtrl := ctrlKeys[ch]
		installPaired(3, []pair{{ch, []string{ptKeyEscape, string(rune(ch))}}, {ch - 32, []string{ptKeyEscape, upper}}})
		for _, cp := range []int{ch, ch - 32} {
			installPaired(2, []pair{{cp, []string{upper}}})
			installPaired(4, []pair{{cp, []string{ptKeyEscape, upper}}})
			if hasCtrl {
				installPaired(6, []pair{{cp, ctrlKey}})
				for _, modifier := range []int{7, 8} {
					installPaired(modifier, []pair{{cp, append([]string{ptKeyEscape}, ctrlKey...)}})
				}
			}
		}
	}

	for cp := 33; cp < 127; cp++ {
		for _, modifier := range []int{2, 9, 10} {
			aliases.put(fmt.Sprintf("\x1b[27;%d;%d~", modifier, cp), string(rune(cp)))
		}
	}

	aliases.put("\x1b[27u", ptKeyEscape)
	for m := 1; m < 17; m++ {
		for _, mod := range lockVariants(m) {
			aliases.put(fmt.Sprintf("\x1b[27;%du", mod), ptKeyEscape)
		}
	}

	altBackspace := []string{ptKeyEscape, ptKeyBackspace}
	installPaired(2, []pair{{9, []string{ptKeyBackTab}}, {127, []string{ptKeyBackspace}}, {32, []string{" "}}})
	installPaired(3, []pair{{13, []string{ptKeyEscape, ptKeyEnter}}, {127, altBackspace}, {32, []string{ptKeyEscape, " "}}})
	installPaired(5, []pair{{9, []string{ptKeyTab}}, {127, altBackspace}})
	installPaired(1, []pair{{9, []string{ptKeyTab}}, {13, []string{ptKeyEnter}}, {32, []string{" "}}, {127, []string{ptKeyBackspace}}})

	for m := 1; m < 17; m++ {
		type legacyForm struct{ base, twin, ss3 string }
		var legacy []legacyForm
		for _, t := range "ABCDFHPQRS" {
			base := fmt.Sprintf("\x1b[%c", t)
			if m > 1 {
				base = fmt.Sprintf("\x1b[1;%d%c", m, t)
			}
			legacy = append(legacy, legacyForm{base, "\x1b[1;%d" + string(t), "\x1bO" + string(t)})
		}
		for n := 1; n < 9; n++ {
			base := fmt.Sprintf("\x1b[%d~", n)
			if m > 1 {
				base = fmt.Sprintf("\x1b[%d;%d~", n, m)
			}
			legacy = append(legacy, legacyForm{base, fmt.Sprintf("\x1b[%d;", n) + "%d~", ""})
		}
		for _, form := range legacy {
			keys, ok := table[form.base]
			if !ok && m == 1 && form.ss3 != "" {
				keys, ok = table[form.ss3]
			}
			if !ok {
				continue
			}
			for _, mod := range lockTwins(m) {
				aliases.put(fmt.Sprintf(form.twin, mod), keys...)
			}
		}
	}

	keypadTwins := map[int]string{57414: "\x1b[13;%du"}
	for i, suffix := range "DCABHF" {
		keypadTwins[[]int{57417, 57418, 57419, 57420, 57423, 57424}[i]] = "\x1b[1;%d" + string(suffix)
	}
	for _, t := range [][2]int{{57421, 5}, {57422, 6}, {57425, 2}, {57426, 3}} {
		keypadTwins[t[0]] = fmt.Sprintf("\x1b[%d;", t[1]) + "%d~"
	}
	for _, entry := range kittyFunctionalMap() {
		aliases.put(fmt.Sprintf("\x1b[%du", entry.Code), entry.Key)
		for _, mod := range lockTwins(1) {
			aliases.put(fmt.Sprintf("\x1b[%d;%du", entry.Code, mod), entry.Key)
		}
		twin, hasTwin := keypadTwins[entry.Code]
		if !ptNamedKeys[entry.Key] && len([]rune(entry.Key)) == 1 {
			twin, hasTwin = fmt.Sprintf("\x1b[%d;", []rune(entry.Key)[0])+"%du", true
		}
		for modifier := 2; modifier < 9; modifier++ {
			for _, mod := range lockVariants(modifier) {
				var equivalent []string
				if hasTwin {
					source := fmt.Sprintf(twin, mod)
					if keys, ok := table[source]; ok {
						equivalent = keys
					} else if keys, ok := aliases.keys[source]; ok {
						equivalent = keys
					}
				} else if entry.Key == ptKeyIgnore {
					equivalent = []string{ptKeyIgnore}
				}
				if equivalent != nil {
					aliases.put(fmt.Sprintf("\x1b[%d;%du", entry.Code, mod), equivalent...)
				}
			}
		}
	}
	return aliases
}

// Maps terminal focus reports to the ignored key so their bytes never reach the buffer
func installIgnoredTerminalSequences() int {
	return installANSIAliases(func(map[string][]string) *aliasSet {
		set := &aliasSet{}
		set.put("\x1b[I", ptKeyIgnore)
		set.put("\x1b[O", ptKeyIgnore)
		return set
	}, false)
}

var promptKeyAliasesOnce sync.Once

// Installs every prompt key alias the classic CLI registers, once per process
func installPromptKeyAliases() {
	promptKeyAliasesOnce.Do(func() {
		installShiftEnterAlias()
		installCtrlEnterAlias()
		installCmdBackspaceAlias()
		installModifyOtherKeysAliases()
		installKeypressDataNormalization()
		installIgnoredTerminalSequences()
	})
}

// vt100Parser turns terminal input bytes into key presses through the sequence table
type vt100Parser struct {
	feedKey          func(ptKeyPress)
	prefix           string
	inBracketedPaste bool
	pasteBuffer      string
	pasteStart       *float64
}

// Builds a parser delivering each parsed key to the callback
func newVT100Parser(feedKey func(ptKeyPress)) *vt100Parser {
	return &vt100Parser{feedKey: feedKey}
}

// Forgets pending input and paste state
func (p *vt100Parser) reset() {
	p.prefix, p.inBracketedPaste, p.pasteBuffer, p.pasteStart = "", false, "", nil
}

// Reports whether some longer sequence or terminal response starts with the prefix
func isPrefixOfLongerMatch(prefix string) bool {
	ansiSequencesMu.Lock()
	defer ansiSequencesMu.Unlock()
	if v, ok := vt100PrefixCache[prefix]; ok {
		return v
	}
	result := cprResponsePrefixRe.MatchString(prefix) || mouseEventPrefixRe.MatchString(prefix)
	if !result {
		for k := range ansiSequences {
			if k != prefix && strings.HasPrefix(k, prefix) {
				result = true
				break
			}
		}
	}
	vt100PrefixCache[prefix] = result
	return result
}

// Keys the exact sequence maps to, nil when it maps to none
func (p *vt100Parser) match(prefix string) []string {
	if cprResponseRe.MatchString(prefix) {
		return []string{ptKeyCPRResponse}
	}
	if mouseEventRe.MatchString(prefix) {
		return []string{ptKeyVT100Mouse}
	}
	ansiSequencesMu.RLock()
	defer ansiSequencesMu.RUnlock()
	return ansiSequences[prefix]
}

// Runs the matching state machine over the pending prefix, flush forcing a decision
func (p *vt100Parser) step(flush bool) {
	for p.prefix != "" {
		longer := isPrefixOfLongerMatch(p.prefix)
		if !flush && longer {
			return
		}
		if keys := p.match(p.prefix); keys != nil {
			p.callHandler(keys, p.prefix)
			p.prefix = ""
			return
		}
		runes := []rune(p.prefix)
		found := false
		for i := len(runes); i > 0; i-- {
			n := min(i, len(runes))
			if keys := p.match(string(runes[:n])); n > 0 && keys != nil {
				p.callHandler(keys, string(runes[:n]))
				runes = runes[n:]
				found = true
			}
		}
		if !found {
			p.callHandler([]string{string(runes[0])}, string(runes[0]))
			runes = runes[1:]
		}
		p.prefix = string(runes)
		flush = false
	}
}

// Delivers the keys of one match, the raw text riding on the first key
func (p *vt100Parser) callHandler(keys []string, insertText string) {
	for i, key := range keys {
		data := insertText
		if i > 0 {
			data = ""
		}
		if key == ptKeyBracketedPaste {
			p.inBracketedPaste = true
			p.pasteBuffer = ""
			continue
		}
		if keypressDataNormalized && !ptNamedKeys[key] && len([]rune(key)) == 1 && strings.HasPrefix(data, "\x1b") {
			data = key
		}
		p.feedKey(ptKeyPress{Key: key, Data: data})
	}
}

// Feeds input text to the parser
func (p *vt100Parser) feed(data string) {
	if p.inBracketedPaste {
		p.pasteBuffer += data
		const endMark = "\x1b[201~"
		if idx := strings.Index(p.pasteBuffer, endMark); idx >= 0 {
			content := p.pasteBuffer[:idx]
			p.feedKey(ptKeyPress{Key: ptKeyBracketedPaste, Data: content})
			p.inBracketedPaste = false
			remaining := p.pasteBuffer[idx+len(endMark):]
			p.pasteBuffer = ""
			p.pasteStart = nil
			p.feed(remaining)
			return
		}
		if !bracketedPasteTimeoutPatched() {
			return
		}
		now := monotonic()
		switch {
		case p.pasteStart == nil:
			p.pasteStart = &now
		case now-*p.pasteStart > bracketedPasteTimeout:
			content, waited := p.pasteBuffer, now-*p.pasteStart
			p.inBracketedPaste, p.pasteBuffer, p.pasteStart = false, "", nil
			if content != "" {
				p.feedKey(ptKeyPress{Key: ptKeyBracketedPaste, Data: content})
				cliLogger.Warn(fmt.Sprintf("Bracketed-paste timeout (%.1fs) — flushed %d bytes without end mark. Terminal may have dropped ESC[201~ (see #16263).",
					waited, len([]rune(content))))
			}
		}
		return
	}
	runes := []rune(data)
	for i, r := range runes {
		if p.inBracketedPaste {
			p.feed(string(runes[i:]))
			return
		}
		p.prefix += string(r)
		p.step(false)
	}
}

// Decides everything still pending as it stands, no more input assumed
func (p *vt100Parser) flush() { p.step(true) }

// Feeds then flushes
func (p *vt100Parser) feedAndFlush(data string) {
	p.feed(data)
	p.flush()
}

// Key presses the byte sequence parses to once flushed
func ptParseSequence(seq string) []ptKeyPress {
	var out []ptKeyPress
	newVT100Parser(func(k ptKeyPress) { out = append(out, k) }).feedAndFlush(seq)
	return out
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

// Kitty modifier parameter of a key event, one plus its modifier and lock bits
func ptModifierParam(mod tea.KeyMod) int {
	param := 1
	for _, bit := range []struct {
		mod tea.KeyMod
		val int
	}{{tea.ModShift, 1}, {tea.ModAlt, 2}, {tea.ModCtrl, 4}, {tea.ModSuper, 8}, {tea.ModHyper, 16}, {tea.ModMeta, 32}, {tea.ModCapsLock, 64}, {tea.ModNumLock, 128}} {
		if mod.Contains(bit.mod) {
			param += bit.val
		}
	}
	return param
}

// Terminal byte sequence a decoded key event stands for, in the spelling the sequence table knows
func ptSequenceForKey(k tea.Key) string {
	param := ptModifierParam(k.Mod)
	base := param - (param-1)&192
	plain := base == 1
	action := k.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper|tea.ModHyper|tea.ModMeta) != 0
	if k.Text != "" && !action {
		if k.Mod.Contains(tea.ModShift) && k.Text == string(k.Code) && unicode.IsLower(k.Code) {
			return strings.ToUpper(k.Text)
		}
		return k.Text
	}
	if letter, ok := ptLegacyLetterKeys[k.Code]; ok {
		switch {
		case param == 1 && letter >= 'P':
			return "\x1bO" + string(letter)
		case param == 1:
			return "\x1b[" + string(letter)
		}
		return fmt.Sprintf("\x1b[1;%d%c", param, letter)
	}
	if n, ok := ptLegacyTildeKeys[k.Code]; ok {
		if param == 1 {
			return fmt.Sprintf("\x1b[%d~", n)
		}
		return fmt.Sprintf("\x1b[%d;%d~", n, param)
	}
	if code, ok := ptKittyCodes[k.Code]; ok {
		if param == 1 {
			return fmt.Sprintf("\x1b[%du", code)
		}
		return fmt.Sprintf("\x1b[%d;%du", code, param)
	}
	switch k.Code {
	case tea.KeyEnter:
		if plain {
			return "\r"
		}
		return fmt.Sprintf("\x1b[13;%du", param)
	case tea.KeyTab:
		if plain {
			return "\t"
		}
		return fmt.Sprintf("\x1b[9;%du", param)
	case tea.KeyBackspace:
		if plain {
			return "\x7f"
		}
		return fmt.Sprintf("\x1b[127;%du", param)
	case tea.KeyEscape:
		if plain {
			return "\x1b"
		}
		return fmt.Sprintf("\x1b[27;%du", param)
	}
	if k.Code >= tea.KeyExtended || k.Code == 0 {
		return ""
	}
	produced := k.Code
	if k.Mod.Contains(tea.ModShift) {
		switch {
		case k.ShiftedCode != 0:
			produced = k.ShiftedCode
		case unicode.IsLetter(k.Code):
			produced = unicode.ToUpper(k.Code)
		}
	}
	switch {
	case plain:
		return string(produced)
	case base == 3 || base == 4:
		return "\x1b" + string(produced)
	case base == 2:
		if k.Code == ' ' {
			return " "
		}
		return string(produced)
	case base == 9 || base == 10:
		return fmt.Sprintf("\x1b[27;%d;%d~", base, produced)
	}
	return fmt.Sprintf("\x1b[%d;%du", unicode.ToLower(k.Code), param)
}

// Key presses a decoded key event produces, a sequence the table does not know consumed as ignored
func ptKeyPressesForEvent(k tea.Key) []ptKeyPress {
	seq := ptSequenceForKey(k)
	if seq == "" {
		return []ptKeyPress{{Key: ptKeyIgnore}}
	}
	if len(seq) > 2 && strings.HasPrefix(seq, "\x1b[") {
		ansiSequencesMu.RLock()
		_, known := ansiSequences[seq]
		ansiSequencesMu.RUnlock()
		if !known {
			return []ptKeyPress{{Key: ptKeyIgnore, Data: seq}}
		}
	}
	return ptParseSequence(seq)
}
