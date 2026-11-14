package tool

import (
	"regexp"
	"strings"
)

var (
	ansiEscape   = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][\s\S]*?(?:\x07|\x1b\\)|[PX^_][\s\S]*?\x1b\\|[\x20-\x2f]+[\x30-\x7e]|[\x30-\x7e])|\x{9b}[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\x{9d}[\s\S]*?(?:\x07|\x{9c})|[\x{80}-\x{9f}]`)
	controlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	unicodeTags  = regexp.MustCompile(`(\x{1F3F4}[\x{E0020}-\x{E007E}]+\x{E007F})|[\x{E0000}-\x{E007F}]`)
)

func StripANSI(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return r == 0x1b || (r >= 0x80 && r <= 0x9f) }) {
		return text
	}
	return ansiEscape.ReplaceAllString(text, "")
}

func SanitizeDisplayText(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool {
		return r <= 0x08 || (r >= 0x0b && r <= 0x1f) || (r >= 0x7f && r <= 0x9f)
	}) {
		return text
	}
	text = StripANSI(text)
	if strings.Contains(text, "\r") {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	}
	return controlChars.ReplaceAllString(text, "")
}

func StripUnicodeTags(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }) {
		return text
	}
	return unicodeTags.ReplaceAllString(text, "$1")
}
