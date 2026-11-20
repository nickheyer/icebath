package pyunicode

import "unicode"

// General categories in the order unicodedata reports them, every rune outside them unassigned
var categoryOrder = []string{"Lu", "Ll", "Lt", "Lm", "Lo", "Mn", "Mc", "Me", "Nd", "Nl", "No", "Pc", "Pd", "Ps", "Pe", "Pi", "Pf", "Po", "Sm", "Sc", "Sk", "So", "Zs", "Zl", "Zp", "Cc", "Cf", "Cs", "Co"}

// Category is unicodedata.category, Cn for unassigned code points
func Category(r rune) string {
	for _, c := range categoryOrder {
		if unicode.Is(unicode.Categories[c], r) {
			return c
		}
	}
	return "Cn"
}
