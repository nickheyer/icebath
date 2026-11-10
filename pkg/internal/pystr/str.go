package pystr

import (
	"errors"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ValueError str.split raises for an empty separator
var ErrEmptySeparator = errors.New("empty separator")

// Code points str.isspace accepts, Unicode White_Space plus the ASCII file group record and unit separators
func IsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// str.strip with no argument, removing Python whitespace from both ends
func Strip(s string) string { return trim(s, IsSpace, true, true) }

// str.lstrip with no argument
func Lstrip(s string) string { return trim(s, IsSpace, true, false) }

// str.rstrip with no argument
func Rstrip(s string) string { return trim(s, IsSpace, false, true) }

// str.strip(chars) removing any code point of chars from both ends
func StripChars(s, chars string) string { return trim(s, inSet(chars), true, true) }

// str.lstrip(chars)
func LstripChars(s, chars string) string { return trim(s, inSet(chars), true, false) }

// str.rstrip(chars)
func RstripChars(s, chars string) string { return trim(s, inSet(chars), false, true) }

// Membership test over the WTF-8 code points of chars
func inSet(chars string) func(rune) bool {
	set := map[rune]bool{}
	for _, r := range Runes(chars) {
		set[r] = true
	}
	return func(r rune) bool { return set[r] }
}

// Cuts leading and trailing code points matching drop, reading surrogates and stray bytes as single code points
func trim(s string, drop func(rune) bool, left, right bool) string {
	start, end := 0, len(s)
	for left && start < end {
		r, n := DecodeRune(s[start:end])
		if !drop(r) {
			break
		}
		start += n
	}
	for right && end > start {
		r, n := DecodeLastRune(s[start:end])
		if !drop(r) {
			break
		}
		end -= n
	}
	return s[start:end]
}

// str.split(None, maxsplit), whitespace runs as separators and the remainder keeping trailing space once maxsplit is spent
func SplitWhitespace(s string, maxsplit int) []string {
	out := []string{}
	i := 0
	for i < len(s) {
		for i < len(s) {
			r, n := DecodeRune(s[i:])
			if !IsSpace(r) {
				break
			}
			i += n
		}
		if i >= len(s) {
			break
		}
		if maxsplit >= 0 && len(out) == maxsplit {
			out = append(out, s[i:])
			break
		}
		j := i
		for j < len(s) {
			r, n := DecodeRune(s[j:])
			if IsSpace(r) {
				break
			}
			j += n
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// str.split(sep, maxsplit) with a literal separator, ValueError for an empty one
func Split(s, sep string, maxsplit int) ([]string, error) {
	if sep == "" {
		return nil, ErrEmptySeparator
	}
	if maxsplit < 0 {
		return strings.Split(s, sep), nil
	}
	return strings.SplitN(s, sep, maxsplit+1), nil
}

// str.splitlines over every Python line boundary, carrying the boundaries when keepends is set
func Splitlines(s string, keepends bool) []string {
	lines := []string{}
	start := 0
	for i := 0; i < len(s); {
		r, size := DecodeRune(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			eol := i
			i += size
			if r == '\r' && i < len(s) && s[i] == '\n' {
				i++
			}
			if keepends {
				eol = i
			}
			lines = append(lines, s[start:eol])
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// str.lower with full case mapping and the Final_Sigma rule
func Lower(s string) string { return cases.Lower(language.Und).String(s) }

// str.upper with full case mapping
func Upper(s string) string { return cases.Upper(language.Und).String(s) }

// str.casefold with full case folding, Cherokee folding to its uppercase letters as CaseFolding.txt maps it
func Casefold(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xAB70 && r <= 0xABBF:
			return r - 0xAB70 + 0x13A0
		case r >= 0x13F8 && r <= 0x13FD:
			return r - 8
		}
		return r
	}, cases.Fold().String(s))
}

// str.title, titlecasing a code point after an uncased one and lowercasing it after a cased one
func Title(s string) string {
	rs := Runes(s)
	var b strings.Builder
	titler := cases.Title(language.Und, cases.NoLower)
	prevCased := false
	for i, r := range rs {
		switch {
		case prevCased && r == 0x3a3:
			b.WriteRune(finalSigma(rs, i))
		case prevCased && r == 0x130:
			b.WriteString("i̇")
		case prevCased:
			WriteRune(&b, unicode.ToLower(r))
		case IsSurrogate(r):
			WriteRune(&b, r)
		default:
			b.WriteString(titler.String(string(r)))
		}
		prevCased = IsCased(r)
	}
	return b.String()
}

// str.capitalize, the first code point in title case and the rest lowered
func Capitalize(s string) string {
	rs := Runes(s)
	if len(rs) == 0 {
		return ""
	}
	head := string(rs[0])
	if !IsSurrogate(rs[0]) {
		head = cases.Title(language.Und, cases.NoLower).String(head)
	}
	return head + Lower(FromRunes(rs[1:]))
}

// Lowercase sigma at i, final form when a cased letter precedes and none follows past case ignorables
func finalSigma(rs []rune, i int) rune {
	j := i - 1
	for j >= 0 && IsCaseIgnorable(rs[j]) {
		j--
	}
	final := j >= 0 && IsCased(rs[j])
	if final && i+1 < len(rs) {
		j = i + 1
		for j < len(rs) && IsCaseIgnorable(rs[j]) {
			j++
		}
		final = j == len(rs) || !IsCased(rs[j])
	}
	if final {
		return 0x3c2
	}
	return 0x3c3
}

// Unicode Cased property, uppercase lowercase or titlecase letters and the Other_Lowercase and Other_Uppercase sets
func IsCased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// Unicode Case_Ignorable property, marks format and modifier classes plus the word break mid letter set
func IsCaseIgnorable(r rune) bool {
	switch r {
	case 0x27, 0x2e, 0x3a, 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

// Code points with Numeric_Type Digit that sit outside the Nd category
var digitExtra = &unicode.RangeTable{R16: []unicode.Range16{
	{0xb2, 0xb3, 1}, {0xb9, 0xb9, 1}, {0x1369, 0x1371, 1}, {0x19da, 0x19da, 1}, {0x2070, 0x2070, 1}, {0x2074, 0x2079, 1},
	{0x2080, 0x2089, 1}, {0x2460, 0x2468, 1}, {0x2474, 0x247c, 1}, {0x2488, 0x2490, 1}, {0x24ea, 0x24ea, 1},
	{0x24f5, 0x24fd, 1}, {0x24ff, 0x24ff, 1}, {0x2776, 0x277e, 1}, {0x2780, 0x2788, 1}, {0x278a, 0x2792, 1},
}, R32: []unicode.Range32{
	{0x10a40, 0x10a43, 1}, {0x10e60, 0x10e68, 1}, {0x11052, 0x1105a, 1}, {0x1f100, 0x1f10a, 1},
}}

// str.isdigit on one code point, decimal digits plus superscripts circled and other Digit typed numbers
func IsDigit(r rune) bool { return unicode.Is(unicode.Nd, r) || unicode.Is(digitExtra, r) }

// str.isdecimal on one code point
func IsDecimal(r rune) bool { return unicode.Is(unicode.Nd, r) }

// str.isalpha on one code point
func IsAlpha(r rune) bool { return unicode.IsLetter(r) }

// str.isalnum on one code point, every N category code point carrying a numeric type
func IsAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// str.isprintable on one code point, refusing separators other than space and the C categories
func IsPrintable(r rune) bool { return r == ' ' || (unicode.IsPrint(r) && !IsSurrogate(r)) }

// Whole string form of a per code point predicate, false for the empty string like the str methods
func All(s string, pred func(rune) bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); {
		r, n := DecodeRune(s[i:])
		if !pred(r) {
			return false
		}
		i += n
	}
	return true
}

// str.isidentifier, an XID_Start or underscore followed by XID_Continue code points
func IsIdentifier(s string) bool {
	rs := Runes(s)
	if len(rs) == 0 || !(rs[0] == '_' || IsXIDStart(rs[0])) {
		return false
	}
	for _, r := range rs[1:] {
		if !IsXIDContinue(r) {
			return false
		}
	}
	return true
}

// Code points ID_Start holds that NFKC closure removes from XID_Start
var xidStartDrop = &unicode.RangeTable{R16: []unicode.Range16{
	{0x37a, 0x37a, 1}, {0xe33, 0xe33, 1}, {0xeb3, 0xeb3, 1}, {0x309b, 0x309c, 1}, {0xfc5e, 0xfc63, 1},
	{0xfdfa, 0xfdfb, 1}, {0xfe70, 0xfe7e, 2}, {0xff9e, 0xff9f, 1},
}}

// Code points ID_Continue holds that NFKC closure removes from XID_Continue
var xidContinueDrop = &unicode.RangeTable{R16: []unicode.Range16{
	{0x37a, 0x37a, 1}, {0x309b, 0x309c, 1}, {0xfc5e, 0xfc63, 1}, {0xfdfa, 0xfdfb, 1}, {0xfe70, 0xfe7e, 2},
}}

// Unicode XID_Start, letters letter numbers and Other_ID_Start minus pattern characters and NFKC unstable ones
func IsXIDStart(r rune) bool {
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) || unicode.Is(xidStartDrop, r) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start)
}

// Unicode XID_Continue, XID_Start plus marks digits connectors and Other_ID_Continue
func IsXIDContinue(r rune) bool {
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) || unicode.Is(xidContinueDrop, r) {
		return false
	}
	if unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue) {
		return true
	}
	return false
}

// Numeric value of a Unicode decimal digit as Python int parses it, false for any other rune
func DecimalValue(r rune) (int, bool) {
	if !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10, true
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10, true
		}
	}
	return 0, false
}

// True for a non empty string whose every character is a digit, str.isdigit
func StrIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !IsDigit(r) {
			return false
		}
	}
	return true
}
