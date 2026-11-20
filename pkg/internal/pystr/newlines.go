package pystr

import "strings"

// Text with CRLF and lone CR turned into LF, as Python text mode reads with universal newlines
func TranslateNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
