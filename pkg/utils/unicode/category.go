package unicodeutil

import "unicode"

// The general categories, the common ones first; no code point is in two of them
var categories = [...]struct {
	name  string
	table *unicode.RangeTable
}{
	{"Ll", unicode.Ll}, {"Lu", unicode.Lu}, {"Lo", unicode.Lo}, {"Nd", unicode.Nd}, {"Po", unicode.Po}, {"Zs", unicode.Zs},
	{"Cc", unicode.Cc}, {"Mn", unicode.Mn}, {"So", unicode.So}, {"Sm", unicode.Sm}, {"Pd", unicode.Pd}, {"Ps", unicode.Ps},
	{"Pe", unicode.Pe}, {"Lm", unicode.Lm}, {"Lt", unicode.Lt}, {"Mc", unicode.Mc}, {"Me", unicode.Me}, {"Nl", unicode.Nl},
	{"No", unicode.No}, {"Pc", unicode.Pc}, {"Pi", unicode.Pi}, {"Pf", unicode.Pf}, {"Sc", unicode.Sc}, {"Sk", unicode.Sk},
	{"Zl", unicode.Zl}, {"Zp", unicode.Zp}, {"Cf", unicode.Cf}, {"Cs", unicode.Cs}, {"Co", unicode.Co},
}

// Two-letter Unicode general category of a code point, Cn for one Unicode leaves unassigned
func Category(r rune) string {
	for _, c := range categories {
		if unicode.Is(c.table, r) {
			return c.name
		}
	}
	return "Cn"
}
