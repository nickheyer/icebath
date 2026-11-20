package state

import "regexp"

// Python re whitespace class for str patterns, every code point str isspace accepts
const pyReSpaceClass = `\t\n\x0b\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	memoryFenceTagRe        = regexp.MustCompile(`(?i)</?[` + pyReSpaceClass + `]*memory-context[` + pyReSpaceClass + `]*>`)
	memoryInternalContextRe = regexp.MustCompile(`(?i)<[` + pyReSpaceClass + `]*memory-context[` + pyReSpaceClass + `]*>(?s:.)*?</[` + pyReSpaceClass + `]*memory-context[` + pyReSpaceClass + `]*>`)
	memoryInternalNoteRe    = regexp.MustCompile(`(?i)\[System note:[` + pyReSpaceClass + `]*The following is recalled memory context,[` + pyReSpaceClass + `]*NOT new user input\.[` + pyReSpaceClass + `]*Treat as (?:informational background data|authoritative reference data[^\]]*)\.\][` + pyReSpaceClass + `]*`)
)

// Strips fence tags, injected context blocks and system notes from provider output
func SanitizeContext(text string) string {
	for _, re := range []*regexp.Regexp{memoryInternalContextRe, memoryInternalNoteRe, memoryFenceTagRe} {
		text = re.ReplaceAllLiteralString(text, "")
	}
	return text
}
