package tty

import (
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// Way a cluster is put on the terminal
type enc uint8

const (
	encText enc = iota
	encDEC
	encSized
)

// Cluster in one encoding, the unit a width is measured for
type probeKey struct {
	text string
	enc  enc
}

// Clusters sharing every property that decides a width in the tables terminals use
type classKey struct {
	page  rune
	eaw   width.Kind
	tail  uint16
	runes uint8
}

// Agreement of the probes taken within one class
type classStat struct {
	width     int
	agree     int
	contested bool
}

// Probes a class needs, all agreeing, before its other members are trusted to match
const classQuorum = 3

// Widths the terminal was measured to give clusters, and what can be inferred from them
type widths struct {
	known   map[probeKey]int
	pending map[probeKey]time.Time
	classes map[classKey]*classStat
	sized   bool
}

// Empty measurements
func newWidths() *widths {
	return &widths{known: map[probeKey]int{}, pending: map[probeKey]time.Time{}, classes: map[classKey]*classStat{}}
}

// Forgets every measurement, used when grapheme mode changes what the terminal measures
func (w *widths) clear() {
	clear(w.known)
	clear(w.pending)
	clear(w.classes)
}

// Width the terminal gives a cluster, false when it is neither measured nor inferable
func (w *widths) of(text string, e enc) (int, bool) {
	if e == encText && len(text) == 1 && text[0] >= 0x20 && text[0] < 0x7f {
		return 1, true
	}
	if n, ok := w.known[probeKey{text, e}]; ok {
		return n, true
	}
	if e != encText {
		return 0, false
	}
	k, inferable := classOf(text)
	if !inferable {
		return 0, false
	}
	if st := w.classes[k]; st != nil && !st.contested && st.agree >= classQuorum {
		return st.width, true
	}
	return 0, false
}

// Records a measured width and what it says about the cluster's class
func (w *widths) learn(text string, e enc, n int) {
	key := probeKey{text, e}
	delete(w.pending, key)
	w.known[key] = n
	if e != encText {
		return
	}
	k, inferable := classOf(text)
	if !inferable {
		return
	}
	st := w.classes[k]
	if st == nil {
		w.classes[k] = &classStat{width: n, agree: 1}
		return
	}
	if st.width == n {
		st.agree++
	} else {
		st.contested = true
	}
}

// Bits naming the kinds of rune that follow the first one in a cluster
const (
	tailVS15 uint16 = 1 << iota
	tailVS16
	tailZWJ
	tailSkin
	tailMark
	tailSpacingMark
	tailRegional
	tailTag
	tailKeycap
	tailOther
)

// Tail bits that mark an emoji sequence, which terminals measure each in its own way
const emojiTail = tailVS16 | tailZWJ | tailSkin | tailRegional | tailTag | tailKeycap

// Class of a cluster and whether its members may be inferred from each other
func classOf(text string) (classKey, bool) {
	first, size := utf8.DecodeRuneInString(text)
	k := classKey{page: first >> 7, eaw: width.LookupRune(first).Kind()}
	n := 1
	for _, r := range text[size:] {
		n++
		switch {
		case r == 0xfe0e:
			k.tail |= tailVS15
		case r == 0xfe0f:
			k.tail |= tailVS16
		case r == 0x200d:
			k.tail |= tailZWJ
		case r >= 0x1f3fb && r <= 0x1f3ff:
			k.tail |= tailSkin
		case regionalRune(r):
			k.tail |= tailRegional
		case r >= 0xe0020 && r <= 0xe007f:
			k.tail |= tailTag
		case r == 0x20e3:
			k.tail |= tailKeycap
		case unicode.In(r, unicode.Mn, unicode.Me):
			k.tail |= tailMark
		case unicode.Is(unicode.Mc, r):
			k.tail |= tailSpacingMark
		default:
			k.tail |= tailOther
		}
	}
	k.runes = uint8(min(n, 4))
	return k, !pictographicRune(first) && !regionalRune(first) && k.tail&emojiTail == 0
}

// Reports whether the rune is a regional indicator letter
func regionalRune(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// Reports whether the rune is Extended_Pictographic
func pictographicRune(r rune) bool {
	i := sort.Search(len(pictographic), func(i int) bool { return pictographic[i][1] >= r })
	return i < len(pictographic) && pictographic[i][0] <= r
}

// Reports whether a cluster holds a rune after which some terminals drop text or join the next cluster
func fragile(text string) bool {
	_, size := utf8.DecodeRuneInString(text)
	for _, r := range text[size:] {
		switch {
		case r == 0x200d, r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef:
			return true
		case unicode.In(r, unicode.Cf, unicode.Me):
			return true
		case unicode.In(r, unicode.Mn, unicode.Mc) && norm.NFD.PropertiesString(string(r)).CCC() == 0:
			return true
		}
	}
	return false
}

// Most columns a cluster can advance the cursor under any width table
func widthBound(text string) int {
	return 2 * utf8.RuneCountInString(text)
}

// DEC special graphics byte for each character the VT100 line drawing set holds
var decGraphics = map[string]byte{
	"◆": '`', "▒": 'a', "␉": 'b', "␌": 'c', "␍": 'd', "␊": 'e', "°": 'f', "±": 'g', "␤": 'h', "␋": 'i',
	"┘": 'j', "┐": 'k', "┌": 'l', "└": 'm', "┼": 'n', "⎺": 'o', "⎻": 'p', "─": 'q', "⎼": 'r', "⎽": 's',
	"├": 't', "┤": 'u', "┴": 'v', "┬": 'w', "│": 'x', "≤": 'y', "≥": 'z', "π": '{', "≠": '|', "£": '}', "·": '~',
}

// ASCII stand ins for symbols a terminal may draw wider than the toolkit laid them out
var lookalikes = func() map[rune]byte {
	m := map[rune]byte{}
	put := func(b byte, rs string) {
		for _, r := range rs {
			m[r] = b
		}
	}
	put('-', "─━═╌╍┄┅┈┉╴╶╸╺⎺⎻⎼⎽–—―‒⁃−")
	put('|', "│┃║╎╏┆┇┊┋╵╷╹╻▏▎▍▌▐▕")
	put('+', "┌┐└┘├┤┬┴┼┏┓┗┛┣┫┳┻╋╔╗╚╝╠╣╦╩╬╭╮╯╰╒╓╕╖╘╙╛╜╞╟╡╢╤╥╧╨╪╫┍┎┑┒┕┖┙┚┝┞┟┠┡┢┥┦┧┨┩┪┭┮┯┰┱┲┵┶┷┸┹┺┽┾┿╀╁╂╃╄╅╆╇╈╉╊±")
	put('#', "█▓▒░▀▄▁▂▃▅▆▇▉▊▋■◼◾")
	put('.', "…⋯·•∙⋅°")
	put('*', "★☆✦✧✱✲✳✴✵✶✷✸✹✺◆◇●○◉◎◯□▢▣▲△▼▽▶▷◀◁►◄※⁂")
	put('<', "←⇐⟵◂‹«")
	put('>', "→⇒⟶▸›»❯➜➤➔")
	put('^', "↑⇑▴")
	put('v', "↓⇓▾✓✔")
	put('x', "×✕✖✗✘")
	put('/', "÷⁄∕")
	put('"', "“”„‟″")
	put('\'', "‘’‚‛′")
	put('?', "�")
	return m
}()

// Forms to try in turn for a cluster drawn wider than its frame width, each showing less of it
func fallbacks(text string) []probeKey {
	var out []probeKey
	if _, ok := decGraphics[text]; ok {
		out = append(out, probeKey{text, encDEC})
	}
	first, size := utf8.DecodeRuneInString(text)
	base := text[:size]
	if pictographicRune(first) {
		out = append(out, probeKey{base + "\ufe0e", encText})
	}
	if b, ok := lookalikes[first]; ok {
		out = append(out, probeKey{asciiText[b], encText})
	}
	if size < len(text) {
		out = append(out, probeKey{base, encText})
	}
	return append(out, probeKey{"?", encText})
}
