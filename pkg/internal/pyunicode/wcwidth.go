package pyunicode

import "github.com/mattn/go-runewidth"

// Locale independent width table, ambiguous characters one cell and neutral emoji one cell as wcwidth has them
var wcCondition = &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}

// Terminal cells of one rune as wcwidth.wcwidth reports them, -1 for a C0 or C1 control character
func WcWidth(r rune) int {
	if r == 0 {
		return 0
	}
	if r < 0x20 || (r >= 0x7F && r < 0xA0) {
		return -1
	}
	return wcCondition.RuneWidth(r)
}

// Terminal cells of the string as wcwidth.wcswidth reports them, -1 when any rune is a control character
func WcsWidth(s string) int {
	total := 0
	for _, r := range s {
		w := WcWidth(r)
		if w < 0 {
			return -1
		}
		total += w
	}
	return total
}
