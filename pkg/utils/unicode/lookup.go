package unicodeutil

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/unicode/runenames"
)

// Code point of a Unicode character name or formal alias in any letter case, the names Unicode derives for Hangul syllables and CJK unified and Tangut ideographs included
func Lookup(name string) (rune, bool) {
	upper := strings.Map(asciiUpper, name)
	if rest, ok := strings.CutPrefix(upper, "HANGUL SYLLABLE "); ok {
		return hangulSyllable(rest)
	}
	if hex, ok := strings.CutPrefix(upper, "CJK UNIFIED IDEOGRAPH-"); ok {
		return derivedName(hex, "<CJK Ideograph")
	}
	if hex, ok := strings.CutPrefix(upper, "TANGUT IDEOGRAPH-"); ok {
		return derivedName(hex, "<Tangut Ideograph")
	}
	r, ok := names()[upper]
	return r, ok
}

// Upper case of an ASCII letter, every other character unchanged: names are ASCII, so a letter such as dotless i that upper cases to I must not match
func asciiUpper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 'a' + 'A'
	}
	return r
}

// Code point of every character name the Unicode data spells out and of every formal alias, built on first use
var names = sync.OnceValue(func() map[string]rune {
	m := make(map[string]rune, 48<<10)
	scan := func(lo, hi rune) {
		for r := lo; r <= hi; r++ {
			if n := runenames.Name(r); n != "" && n[0] != '<' {
				m[n] = r
			}
		}
	}
	// Planes 0 to 3 and the tag plane 14 hold every named character, planes 4 to 13 being unassigned and 15 and 16 private use
	scan(0, 0x3ffff)
	scan(0xe0000, 0xe0fff)
	for _, a := range aliases {
		m[a.name] = a.r
	}
	return m
})

// Code point the hexadecimal suffix of a derived name gives, when the Unicode data marks it with the placeholder of that derivation
func derivedName(hex, placeholder string) (rune, bool) {
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || fmt.Sprintf("%04X", v) != hex {
		return 0, false
	}
	r := rune(v)
	if !strings.HasPrefix(runenames.Name(r), placeholder) {
		return 0, false
	}
	return r, true
}

// Short names of the leading consonant, vowel and trailing consonant jamo that compose Hangul syllable names
var (
	jamoLead  = [...]string{"G", "GG", "N", "D", "DD", "R", "M", "B", "BB", "S", "SS", "", "J", "JJ", "C", "K", "T", "P", "H"}
	jamoVowel = [...]string{"A", "AE", "YA", "YAE", "EO", "E", "YEO", "YE", "O", "WA", "WAE", "OE", "YO", "U", "WEO", "WE", "WI", "YU", "EU", "YI", "I"}
	jamoTrail = [...]string{"", "G", "GG", "GS", "N", "NJ", "NH", "D", "L", "LG", "LM", "LB", "LS", "LT", "LP", "LH", "M", "B", "BS", "S", "SS", "NG", "J", "C", "K", "T", "P", "H"}
)

// Hangul syllable a run of jamo short names spells, taking the longest lead, vowel and trail in turn
func hangulSyllable(s string) (rune, bool) {
	lead, s, ok := longestJamo(s, jamoLead[:])
	if !ok {
		return 0, false
	}
	vowel, s, ok := longestJamo(s, jamoVowel[:])
	if !ok {
		return 0, false
	}
	trail, s, ok := longestJamo(s, jamoTrail[:])
	if !ok || s != "" {
		return 0, false
	}
	return 0xac00 + rune((lead*len(jamoVowel)+vowel)*len(jamoTrail)+trail), true
}

// Index of the longest short name that starts s and what follows it, false when none does
func longestJamo(s string, short []string) (int, string, bool) {
	best := -1
	for i, n := range short {
		if strings.HasPrefix(s, n) && (best < 0 || len(n) > len(short[best])) {
			best = i
		}
	}
	if best < 0 {
		return 0, s, false
	}
	return best, s[len(short[best]):], true
}
