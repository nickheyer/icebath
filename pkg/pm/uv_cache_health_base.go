package pm

import (
	"unicode"
)

// Characters Python str patterns match with a backslash s class
const pyWhitespaceClass = `\t\n\x0b\x0c\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// Reports a character Python str patterns count as a word character, a letter, a number or underscore
func pyIsWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }
