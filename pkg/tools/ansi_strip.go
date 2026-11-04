package tools

import (
	"regexp"
	"strings"
)

// Every ECMA-48 escape sequence shape plus 8 bit C1 controls
var ansiEscapeRe = regexp.MustCompile(`(?s)\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][\s\S]*?(?:\x07|\x1b\\)|[PX^_][\s\S]*?(?:\x1b\\)|[\x20-\x2f]+[\x30-\x7e]|[\x30-\x7e])|\x9b[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\x9d[\s\S]*?(?:\x07|\x9c)|[\x{80}-\x{9f}]`)

var (
	hasEscapeRe        = regexp.MustCompile("[\x1b\u0080-\u009f]")
	hasControlRe       = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f-\u009f]")
	hasUnicodeTagRe    = regexp.MustCompile("[\U000E0000-\U000E007F]")
	ansiControlCharsRe = regexp.MustCompile("[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]")
	unicodeTagSubRe    = regexp.MustCompile("(\U0001F3F4[\U000E0020-\U000E007E]+\U000E007F)|[\U000E0000-\U000E007F]")
	ansiFastPathSkip   = func(text string) bool { return text == "" || !hasEscapeRe.MatchString(text) }
)

// Removes ANSI escape sequences, clean text passes through unchanged
func StripANSI(text string) string {
	if ansiFastPathSkip(text) {
		return text
	}
	return ansiEscapeRe.ReplaceAllString(text, "")
}

// Strips escapes and bare control characters keeping newlines and tabs, CRs become newlines
func SanitizeDisplayText(text string) string {
	if text == "" || !hasControlRe.MatchString(text) {
		return text
	}
	text = StripANSI(text)
	if strings.Contains(text, "\r") {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	}
	return ansiControlCharsRe.ReplaceAllString(text, "")
}

// Removes invisible Unicode TAG characters while keeping valid emoji tag sequences
func StripUnicodeTags(text string) string {
	if text == "" || !hasUnicodeTagRe.MatchString(text) {
		return text
	}
	return unicodeTagSubRe.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(m, "\U0001F3F4") {
			return m
		}
		return ""
	})
}
