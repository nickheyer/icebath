package term

import "regexp"

// Every ECMA-48 escape sequence shape plus 8 bit C1 controls, an OSC a program was killed in the middle of writing taken to the next escape
var escapes = regexp.MustCompile(`(?s)\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][\s\S]*?(?:\x07|\x1b\\)|\][^\x07\x1b]*|[PX^_][\s\S]*?(?:\x1b\\)|[\x20-\x2f]+[\x30-\x7e]|[\x30-\x7e])|\x9b[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\x9d[\s\S]*?(?:\x07|\x9c)|[\x{80}-\x{9f}]`)

var escaped = regexp.MustCompile("[\x1b\u0080-\u009f]")

// Text without its ANSI escape sequences
func StripANSI(text string) string {
	if text == "" || !escaped.MatchString(text) {
		return text
	}
	return escapes.ReplaceAllString(text, "")
}
