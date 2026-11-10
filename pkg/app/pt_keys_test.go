package app

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Key names a byte sequence parses to, fed one character at a time then flushed
func parseKeys(seq string) []string {
	return pressKeys(parsePresses(seq))
}

// Key presses a byte sequence parses to, fed one character at a time then flushed
func parsePresses(seq string) []ptKeyPress {
	var out []ptKeyPress
	p := newVT100Parser(func(k ptKeyPress) { out = append(out, k) })
	for _, ch := range seq {
		p.feed(string(ch))
	}
	p.flush()
	return out
}

// Snapshots the sequence table for the test and installs the given aliases over it
func withAliasTable(t *testing.T, installs ...func() int) {
	t.Helper()
	ansiSequencesMu.Lock()
	saved := make(map[string][]string, len(ansiSequences))
	for k, v := range ansiSequences {
		saved[k] = v
	}
	normalized := keypressDataNormalized
	ansiSequencesMu.Unlock()
	t.Cleanup(func() {
		ansiSequencesMu.Lock()
		ansiSequences = saved
		clearVT100PrefixCache()
		keypressDataNormalized = normalized
		ansiSequencesMu.Unlock()
	})
	for _, install := range installs {
		install()
	}
}

// Table entry of a sequence, nil when it has none
func tableEntry(seq string) []string {
	ansiSequencesMu.RLock()
	defer ansiSequencesMu.RUnlock()
	return ansiSequences[seq]
}

func wantKeys(t *testing.T, seq string, want ...string) {
	t.Helper()
	if got := parseKeys(seq); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q parsed as %q, want %q", seq, got, want)
	}
}

func wantSameKeys(t *testing.T, seq, as string) {
	t.Helper()
	if got, want := parseKeys(seq), parseKeys(as); !reflect.DeepEqual(got, want) {
		t.Fatalf("%q parsed as %q, want %q like %q", seq, got, want, as)
	}
}

func TestModifyOtherKeysCtrlLetterParsesAsRawByte(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for letter := 'a'; letter <= 'z'; letter++ {
		raw := string(letter - 'a' + 1)
		if got := parseKeys(raw); len(got) != 1 {
			t.Fatalf("raw byte %q produced %q", raw, got)
		}
		wantSameKeys(t, fmt.Sprintf("\x1b[27;5;%d~", letter), raw)
		wantSameKeys(t, fmt.Sprintf("\x1b[%d;5u", letter), raw)
	}
}

func TestModifyOtherKeysCtrlDigit(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for d := 0; d < 10; d++ {
		wantKeys(t, fmt.Sprintf("\x1b[27;5;%d~", '0'+d), fmt.Sprintf("c-%d", d))
		wantKeys(t, fmt.Sprintf("\x1b[%d;5u", '0'+d), fmt.Sprintf("c-%d", d))
	}
}

func TestCtrlLeftBracketUnderModifyOtherKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[27;5;91~", "\x1b")
	wantSameKeys(t, "\x1b[91;5u", "\x1b")
}

func TestCtrlBackslashUnderModifyOtherKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[27;5;92~", "c-\\")
	wantKeys(t, "\x1b[92;5u", "c-\\")
}

func TestCtrlRightBracketUnderModifyOtherKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[27;5;93~", "c-]")
	wantKeys(t, "\x1b[93;5u", "c-]")
}

func TestCtrlSpaceUnderModifyOtherKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[27;5;32~", "\x00")
	wantSameKeys(t, "\x1b[32;5u", "\x00")
}

func TestModifyOtherKeysAltLetterParsesAsEscapeLetter(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for letter := 'a'; letter <= 'z'; letter++ {
		bare := "\x1b" + string(letter)
		if got := parseKeys(bare); len(got) != 2 {
			t.Fatalf("bare ESC+%c produced %q", letter, got)
		}
		wantSameKeys(t, fmt.Sprintf("\x1b[27;3;%d~", letter), bare)
		wantSameKeys(t, fmt.Sprintf("\x1b[%d;3u", letter), bare)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	if n := installModifyOtherKeysAliases(); n != 0 {
		t.Fatalf("second modifyOtherKeys install changed %d entries", n)
	}
	installCmdBackspaceAlias()
	if n := installCmdBackspaceAlias(); n != 0 {
		t.Fatalf("second cmd backspace install changed %d entries", n)
	}
}

func TestModifyOtherKeysShiftLetterProducesUppercase(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for letter := 'a'; letter <= 'z'; letter++ {
		upper := strings.ToUpper(string(letter))
		wantKeys(t, fmt.Sprintf("\x1b[27;2;%d~", letter), upper)
		wantKeys(t, fmt.Sprintf("\x1b[%d;2u", letter), upper)
	}
}

func TestModifyOtherKeysShiftSymbolProducesChar(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for _, cp := range []int{58, 95, 123, 125} {
		wantKeys(t, fmt.Sprintf("\x1b[27;2;%d~", cp), string(rune(cp)))
		if entry := tableEntry(fmt.Sprintf("\x1b[%d;2u", cp)); entry != nil {
			t.Fatalf("kitty spelling of shift symbol %d is mapped to %q", cp, entry)
		}
	}
}

func TestModifyOtherKeysSuperPrintableProducesChar(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[27;9;111~", "o")
	wantKeys(t, "\x1b[27;10;79~", "O")
}

func TestShiftSymbolDataNormalizedInBuffer(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases, installKeypressDataNormalization)
	out := parsePresses("\x1b[27;2;95~")
	if len(out) != 1 || out[0].Key != "_" || out[0].Data != "_" {
		t.Fatalf("presses %#v", out)
	}
}

func TestDoesNotClobberShiftEnterAlias(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases, installShiftEnterAlias)
	for _, seq := range []string{"\x1b[27;2;13~", "\x1b[13;2u"} {
		if got := tableEntry(seq); !reflect.DeepEqual(got, []string{"escape", "c-m"}) {
			t.Fatalf("%q maps to %q", seq, got)
		}
	}
}

func TestCtrlEnterStillWorksUnderModifyOtherKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases, installCtrlEnterAlias, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[27;5;13~", "\x1b\r")
	wantSameKeys(t, "\x1b[13;5u", "\x1b\r")
}

func TestKittyPlainEscapeKey(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[27u", "escape")
}

func TestKittyModifiedEscapeKey(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for modifier := 2; modifier < 9; modifier++ {
		wantKeys(t, fmt.Sprintf("\x1b[27;%du", modifier), "escape")
	}
}

func TestAltEnterProducesNewlineTuple(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[13;3u", "\x1b\r")
	wantSameKeys(t, "\x1b[27;3;13~", "\x1b\r")
}

func TestShiftTabProducesBacktab(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[9;2u", "s-tab")
	wantKeys(t, "\x1b[27;2;9~", "s-tab")
}

func TestCtrlBackspaceProducesBackwardKillWord(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[127;5u", "escape", "c-h")
	wantKeys(t, "\x1b[27;5;127~", "escape", "c-h")
}

func TestAltBackspaceProducesBackwardKillWord(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[127;3u", "escape", "c-h")
	wantKeys(t, "\x1b[27;3;127~", "escape", "c-h")
}

func TestShiftBackspaceIsPlainBackspace(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[127;2u", "\x7f")
}

func TestShiftSpaceInsertsSpace(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[32;2u", " ")
	wantKeys(t, "\x1b[27;2;32~", " ")
}

// Asserts every sequence parses to one key press carrying the key as its typed text
func wantNormalizedPress(t *testing.T, key string, seqs ...string) {
	t.Helper()
	withAliasTable(t, installModifyOtherKeysAliases, installKeypressDataNormalization)
	for _, seq := range seqs {
		presses := parsePresses(seq)
		if len(presses) != 1 || presses[0].Key != key || presses[0].Data != key {
			t.Fatalf("%q presses %#v, want key and data %q", seq, presses, key)
		}
	}
}

func TestShiftSpaceKeypressDataIsPlainSpace(t *testing.T) {
	wantNormalizedPress(t, " ", "\x1b[32;2u", "\x1b[27;2;32~")
}

func TestShiftLetterKeypressDataIsUppercase(t *testing.T) {
	wantNormalizedPress(t, "A", "\x1b[97;2u", "\x1b[27;2;97~")
}

func TestKeypadDigitKeypressDataIsDigit(t *testing.T) {
	wantNormalizedPress(t, "5", "\x1b[57404u")
}

// Types ab, the payload, then cd into a running application and returns the buffer text
func typedBufferText(t *testing.T, payload string) string {
	t.Helper()
	h := newPTHarness(t, 60, 8, func(a *ptApplication) { basicPTLayout(a, make(chan string, 1)) })
	h.start()
	h.typeBytes("ab")
	waitFor(t, "first characters", func() bool { return h.app.BufferText() == "ab" })
	h.typeBytes(payload)
	time.Sleep(120 * time.Millisecond)
	h.typeBytes("cd")
	waitFor(t, "trailing characters", func() bool { return strings.HasSuffix(h.app.BufferText(), "cd") })
	return h.app.BufferText()
}

func TestBufferLevelShiftSpaceNoRawCsi(t *testing.T) {
	for label, payload := range map[string]string{"Shift+Space xterm": "\x1b[27;2;32~", "Shift+Space kitty": "\x1b[32;2u", "plain space": " "} {
		if got := typedBufferText(t, payload); got != "ab cd" {
			t.Fatalf("%s: buffer %q, want %q", label, got, "ab cd")
		}
	}
}

func TestBufferLevelShiftLetterNoRawCsi(t *testing.T) {
	for label, payload := range map[string]string{"Shift+m xterm": "\x1b[27;2;109~", "Shift+m kitty": "\x1b[109;2u", "produced M xterm": "\x1b[27;2;77~", "plain M": "M"} {
		if got := typedBufferText(t, payload); got != "abMcd" {
			t.Fatalf("%s: buffer %q, want %q", label, got, "abMcd")
		}
	}
}

func TestCtrlShiftLetterBehavesAsCtrl(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for _, letter := range "crz" {
		raw := string(letter & 0x1F)
		for _, cp := range []rune{letter, letter - 32} {
			wantSameKeys(t, fmt.Sprintf("\x1b[%d;6u", cp), raw)
			wantSameKeys(t, fmt.Sprintf("\x1b[27;6;%d~", cp), raw)
		}
	}
}

func TestCtrlAltLetterBehavesAsEscapeCtrl(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[97;7u", "escape", "c-a")
}

func TestCtrlAltShiftLetterBehavesAsEscapeCtrl(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[97;8u", "escape", "c-a")
}

func TestShiftAltLetterBehavesAsEscapeUpper(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[102;4u", "escape", "F")
}

func TestKeypadEnterBehavesAsEnter(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[57414u", "\r")
}

func TestKeypadDigitsTypeDigits(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for d := 0; d < 10; d++ {
		wantKeys(t, fmt.Sprintf("\x1b[%du", 57399+d), fmt.Sprint(d))
	}
}

func TestKeypadNavigationMapsToArrows(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[57417u", "left")
	wantKeys(t, "\x1b[57418u", "right")
	wantKeys(t, "\x1b[57419u", "up")
	wantKeys(t, "\x1b[57420u", "down")
}

func TestF13MapsToF13(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[57376u", "f13")
}

func TestLockMediaModifierEventsAreConsumed(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for _, code := range []int{57358, 57428, 57441, 57448} {
		wantKeys(t, fmt.Sprintf("\x1b[%du", code), "<ignore>")
	}
}

func TestCmdBackspaceAliasNotClobbered(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases, installCmdBackspaceAlias, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[127;9u", "c-u")
}

func TestCtrlLetterWithNumlockParsesAsRawByte(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for letter := 'a'; letter <= 'z'; letter++ {
		wantSameKeys(t, fmt.Sprintf("\x1b[%d;133u", letter), string(letter-'a'+1))
	}
}

func TestCtrlLetterWithCapslockParsesAsRawByte(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for _, letter := range "acz" {
		wantSameKeys(t, fmt.Sprintf("\x1b[%d;69u", letter), string(letter-'a'+1))
	}
}

func TestCtrlCWithBothLocksParsesAsRawByte(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[99;197u", "\x03")
}

func TestAltLetterWithNumlockKeepsEscapePrefix(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[97;131u", "escape", "a")
}

func TestShiftLetterWithCapslockTypesUppercase(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[97;66u", "A")
}

func TestEscKeyWithNumlockIsEscape(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[27;129u", "escape")
	wantKeys(t, "\x1b[27;133u", "escape")
}

func TestCtrlBackspaceWithNumlockIsBackwardKillWord(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[127;133u", "escape", "c-h")
}

func TestModifyOtherKeysTildeFormHasNoLockVariants(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for _, seq := range []string{"\x1b[27;69;99~", "\x1b[27;133;99~", "\x1b[27;197;99~"} {
		if entry := tableEntry(seq); entry != nil {
			t.Fatalf("%q is mapped to %q", seq, entry)
		}
	}
}

func TestLockBitsOnLegacyCursorKeysMapToPlainKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	for _, mod := range []int{129, 65, 193} {
		wantKeys(t, fmt.Sprintf("\x1b[1;%dB", mod), "down")
	}
	wantKeys(t, "\x1b[1;130B", "s-down")
	wantKeys(t, "\x1b[1;131D", "escape", "left")
	wantKeys(t, "\x1b[1;133D", "c-left")
}

func TestLockBitsOnTildeNavigationKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[3;129~", "delete")
	wantKeys(t, "\x1b[3;69~", "c-delete")
	wantKeys(t, "\x1b[5;193~", "pageup")
}

func TestLockBitsOnPlainF1ThroughF4(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[1;129P", "\x1bOP")
	wantSameKeys(t, "\x1b[1;65P", "\x1bOP")
}

func TestLockBitsOnUnmodifiedCsiUKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantSameKeys(t, "\x1b[9;65u", "\t")
	wantSameKeys(t, "\x1b[13;193u", "\r")
	wantSameKeys(t, "\x1b[32;129u", " ")
	wantSameKeys(t, "\x1b[127;129u", "\x7f")
}

func TestLockBitsOnPuaFunctionalKeys(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases)
	wantKeys(t, "\x1b[57399;129u", "0")
	wantKeys(t, "\x1b[57376;129u", "f13")
	wantKeys(t, "\x1b[57427;129u", "<ignore>")
}

func TestLockBitsOnShiftEnterAndCtrlEnterAliases(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases, installShiftEnterAlias, installCtrlEnterAlias)
	wantSameKeys(t, "\x1b[13;130u", "\x1b\r")
	wantSameKeys(t, "\x1b[13;66u", "\x1b\r")
	wantSameKeys(t, "\x1b[13;133u", "\x1b\r")
}

func TestLockBitsOnCmdBackspaceAlias(t *testing.T) {
	withAliasTable(t, installModifyOtherKeysAliases, installCmdBackspaceAlias)
	wantKeys(t, "\x1b[127;137u", "c-u")
	wantKeys(t, "\x1b[127;73u", "c-u")
	wantKeys(t, "\x1b[3;137~", "c-k")
}

var shiftEnterSequences = []string{"\x1b[13;2u", "\x1b[27;2;13~", "\x1b[27;2;13u"}

func TestInstallRegistersAllThreeSequences(t *testing.T) {
	withAliasTable(t, installShiftEnterAlias)
	for _, seq := range shiftEnterSequences {
		if got := tableEntry(seq); !reflect.DeepEqual(got, []string{"escape", "c-m"}) {
			t.Fatalf("%q maps to %q", seq, got)
		}
	}
}

func TestCsiUShiftEnterParsesAsAltEnter(t *testing.T) {
	withAliasTable(t, installShiftEnterAlias)
	wantSameKeys(t, "\x1b[13;2u", "\x1b\r")
}

var cmdBackspaceSequences = []string{"\x1b[127;9u", "\x1b[127;10u", "\x1b[27;9;127~"}
var cmdForwardDeleteSequences = []string{"\x1b[3;9~", "\x1b[3;10~"}

func TestCmdBackspaceParsesAsCtrlU(t *testing.T) {
	withAliasTable(t, installCmdBackspaceAlias)
	for _, seq := range cmdBackspaceSequences {
		wantSameKeys(t, seq, "\x15")
	}
}

func TestCmdForwardDeleteParsesAsCtrlK(t *testing.T) {
	withAliasTable(t, installCmdBackspaceAlias)
	for _, seq := range cmdForwardDeleteSequences {
		wantSameKeys(t, seq, "\x0b")
	}
}

func TestSequencesEmitExactlyOneKeypress(t *testing.T) {
	withAliasTable(t, installCmdBackspaceAlias)
	for _, seq := range append(append([]string{}, cmdBackspaceSequences...), cmdForwardDeleteSequences...) {
		if got := parseKeys(seq); len(got) != 1 {
			t.Fatalf("%q emitted %q", seq, got)
		}
	}
}

func TestUnmodifiedKeysKeepTheirOwnBindings(t *testing.T) {
	withAliasTable(t, installCmdBackspaceAlias)
	if got := tableEntry("\x1b[3~"); !reflect.DeepEqual(got, []string{"delete"}) {
		t.Fatalf("delete maps to %q", got)
	}
	if got := tableEntry("\x7f"); !reflect.DeepEqual(got, []string{"c-h"}) {
		t.Fatalf("backspace maps to %q", got)
	}
}

func TestCtrlForwardDeleteIsNotRemapped(t *testing.T) {
	withAliasTable(t, installCmdBackspaceAlias)
	if got := tableEntry("\x1b[3;5~"); !reflect.DeepEqual(got, []string{"c-delete"}) {
		t.Fatalf("ctrl delete maps to %q", got)
	}
}

func TestModifiedKeypadMirrorsItsNonKeypadTwin(t *testing.T) {
	withAliasTable(t, installShiftEnterAlias, installCtrlEnterAlias, installModifyOtherKeysAliases, installKeypressDataNormalization)
	twins := map[int]string{57414: "\x1b[13;%du"}
	for i, suffix := range "DCABHF" {
		twins[[]int{57417, 57418, 57419, 57420, 57423, 57424}[i]] = "\x1b[1;%d" + string(suffix)
	}
	for _, pair := range [][2]int{{57421, 5}, {57422, 6}, {57425, 2}, {57426, 3}} {
		twins[pair[0]] = fmt.Sprintf("\x1b[%d;", pair[1]) + "%d~"
	}
	for d := 0; d < 10; d++ {
		twins[57399+d] = fmt.Sprintf("\x1b[%d;", '0'+d) + "%du"
	}
	for code, template := range twins {
		for base := 2; base < 9; base++ {
			for _, offset := range []int{0, 64, 128, 192} {
				modifier := base + offset
				sequence := fmt.Sprintf("\x1b[%d;%du", code, modifier)
				twin := fmt.Sprintf(template, modifier)
				if tableEntry(twin) != nil {
					wantSameKeys(t, sequence, twin)
				} else if entry := tableEntry(sequence); entry != nil {
					t.Fatalf("%q mapped to %q without a twin", sequence, entry)
				}
				if entry := tableEntry(fmt.Sprintf("\x1b[27;%d;%d~", modifier, code)); entry != nil {
					t.Fatalf("tilde form of keypad %d modifier %d mapped to %q", code, modifier, entry)
				}
			}
		}
	}
	ansiSequencesMu.RLock()
	installed := make(map[string][]string, len(ansiSequences))
	for k, v := range ansiSequences {
		installed[k] = v
	}
	ansiSequencesMu.RUnlock()
	installed["\x1b[13;3u"] = []string{"c-a"}
	aliases := modifyOtherKeysAliases(installed)
	if got := aliases.keys["\x1b[57414;3u"]; !reflect.DeepEqual(got, []string{"c-a"}) {
		t.Fatalf("keypad alt enter alias %q", got)
	}
}

func TestBracketedPasteTimeout(t *testing.T) {
	applyBracketedPasteTimeoutPatch()
	makeParser := func() (*vt100Parser, *[]ptKeyPress) {
		var calls []ptKeyPress
		return newVT100Parser(func(k ptKeyPress) { calls = append(calls, k) }), &calls
	}
	stale := func(p *vt100Parser, ago float64) {
		start := monotonic() - ago
		p.pasteStart = &start
	}
	t.Run("test_normal_bracketed_paste_works", func(t *testing.T) {
		p, calls := makeParser()
		p.feed("\x1b[200~hello world\x1b[201~")
		if len(*calls) != 1 || (*calls)[0].Data != "hello world" {
			t.Fatalf("calls %#v", *calls)
		}
	})
	t.Run("test_incomplete_paste_times_out", func(t *testing.T) {
		p, calls := makeParser()
		p.feed("\x1b[200~some pasted text")
		if !p.inBracketedPaste {
			t.Fatal("parser left paste mode early")
		}
		stale(p, 3.0)
		p.feed("more data")
		if p.inBracketedPaste || len(*calls) == 0 {
			t.Fatalf("in paste %v calls %#v", p.inBracketedPaste, *calls)
		}
	})
	t.Run("test_normal_keys_after_timeout_recovery", func(t *testing.T) {
		p, _ := makeParser()
		p.feed("\x1b[200~stuck")
		stale(p, 3.0)
		p.feed("")
		if p.inBracketedPaste {
			t.Fatal("parser stayed in paste mode")
		}
		p.feed("a")
		if p.inBracketedPaste {
			t.Fatal("a key reentered paste mode")
		}
	})
	t.Run("test_subsequent_data_after_incomplete_paste", func(t *testing.T) {
		p, calls := makeParser()
		p.feed("\x1b[200~content")
		stale(p, 5.0)
		p.feed("x")
		if p.inBracketedPaste || len(*calls) < 1 {
			t.Fatalf("in paste %v calls %#v", p.inBracketedPaste, *calls)
		}
	})
	t.Run("test_torn_end_mark_recovers", func(t *testing.T) {
		p, calls := makeParser()
		p.feed("\x1b[200~some content\x1b[20")
		if !p.inBracketedPaste {
			t.Fatal("parser left paste mode early")
		}
		p.feed("1~")
		if p.inBracketedPaste || len(*calls) != 1 || (*calls)[0].Data != "some content" {
			t.Fatalf("in paste %v calls %#v", p.inBracketedPaste, *calls)
		}
	})
	t.Run("test_no_timeout_under_threshold", func(t *testing.T) {
		p, calls := makeParser()
		p.feed("\x1b[200~waiting")
		stale(p, 0.5)
		p.feed("more waiting")
		if !p.inBracketedPaste || len(*calls) != 0 {
			t.Fatalf("in paste %v calls %#v", p.inBracketedPaste, *calls)
		}
	})
}

func TestClassicCliOutputSelection(t *testing.T) {
	onWindows := func(t *testing.T) {
		prev := ctrlEnterGOOS
		ctrlEnterGOOS = "windows"
		t.Cleanup(func() { ctrlEnterGOOS = prev })
		for _, name := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "PROMPT_TOOLKIT_NO_CPR"} {
			t.Setenv(name, "")
		}
	}
	t.Run("test_windows_preserves_default_output_selection", func(t *testing.T) {
		onWindows(t)
		if terminalMayLeakCPR() || selectClassicCLIPTOutput(nil) != nil {
			t.Fatal("Windows lost the default output selection")
		}
	})
	t.Run("test_windows_honors_explicit_no_cpr", func(t *testing.T) {
		onWindows(t)
		t.Setenv("PROMPT_TOOLKIT_NO_CPR", "1")
		if !terminalMayLeakCPR() {
			t.Fatal("explicit opt in ignored")
		}
		if out := selectClassicCLIPTOutput(nil); out != nil && out.enableCPR {
			t.Fatal("cursor position queries left on")
		}
	})
}

func TestStripLeakedTerminalResponses(t *testing.T) {
	strip := func(text string) string {
		cleaned, _ := stripLeakedTerminalResponsesWithMeta(text)
		return cleaned
	}
	t.Run("test_strips_multiple_dsr_responses", func(t *testing.T) {
		if got := strip("a\x1b[53;1Rb\x1b[51;1Rc\x1b[50;9Rd"); got != "abcd" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_strips_sgr_mouse_report_with_large_coordinates", func(t *testing.T) {
		if got := strip("abc\x1b[<10000;12345;98765Mdef"); got != "abcdef" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_strips_multiple_concatenated_sgr_mouse_reports", func(t *testing.T) {
		if got := strip("<65;1;49M<35;1;42Mhello<64;1;40m"); got != "hello" {
			t.Fatalf("got %q", got)
		}
	})
}

// Key and typed text pairs a byte sequence parses to when fed in one piece
func parseKeyData(data string) [][2]string {
	var out [][2]string
	newVT100Parser(func(k ptKeyPress) { out = append(out, [2]string{k.Key, k.Data}) }).feedAndFlush(data)
	return out
}

func TestFocusEventsAreParserLevelIgnoredBeforePromptBuffer(t *testing.T) {
	withAliasTable(t, installIgnoredTerminalSequences)
	want := [][2]string{{"<ignore>", "\x1b[O"}, {"<ignore>", "\x1b[I"}, {"h", "h"}, {"e", "e"}, {"l", "l"}, {"l", "l"}, {"o", "o"}}
	if got := parseKeyData("\x1b[O\x1b[Ihello"); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed %q", got)
	}
}

func TestRegularEscapeShortcutsStillParseNormally(t *testing.T) {
	withAliasTable(t, installIgnoredTerminalSequences)
	if got := parseKeyData("\x1bg"); !reflect.DeepEqual(got, [][2]string{{"escape", "\x1b"}, {"g", "g"}}) {
		t.Fatalf("parsed %q", got)
	}
}

func TestInstallIsIdempotentAndSetdefaultSafe(t *testing.T) {
	withAliasTable(t)
	first := installIgnoredTerminalSequences()
	if second := installIgnoredTerminalSequences(); second != 0 || first < 0 || first > 2 {
		t.Fatalf("first install %d second %d", first, second)
	}
}
