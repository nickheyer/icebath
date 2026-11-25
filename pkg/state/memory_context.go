package state

import "regexp"

// Python re whitespace class for str patterns, every code point str isspace accepts
const reSpaceClass = `\t\n\x0b\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	memoryFenceTagRe        = regexp.MustCompile(`(?i)</?[` + reSpaceClass + `]*memory-context[` + reSpaceClass + `]*>`)
	memoryInternalContextRe = regexp.MustCompile(`(?i)<[` + reSpaceClass + `]*memory-context[` + reSpaceClass + `]*>(?s:.)*?</[` + reSpaceClass + `]*memory-context[` + reSpaceClass + `]*>`)
	memoryInternalNoteRe    = regexp.MustCompile(`(?i)\[System note:[` + reSpaceClass + `]*The following is recalled memory context,[` + reSpaceClass + `]*NOT new user input\.[` + reSpaceClass + `]*Treat as (?:informational background data|authoritative reference data[^\]]*)\.\][` + reSpaceClass + `]*`)
)

// Strips fence tags, injected context blocks and system notes from provider output
func SanitizeContext(text string) string {
	for _, re := range []*regexp.Regexp{memoryInternalContextRe, memoryInternalNoteRe, memoryFenceTagRe} {
		text = re.ReplaceAllLiteralString(text, "")
	}
	return text
}
