package pyrich

import (
	"regexp"
	"strings"
)

var tagRE = regexp.MustCompile(`(\\*)\[([a-z#/@][^\[]*?)]`)

var escapeRE = regexp.MustCompile(`(\\*)(\[[a-z#/@][^\[]*?])`)

var sgr = map[string]string{
	"bold": "1", "dim": "2", "italic": "3", "underline": "4", "blink": "5", "reverse": "7", "strike": "9",
	"black": "30", "red": "31", "green": "32", "yellow": "33", "blue": "34", "magenta": "35", "cyan": "36", "white": "37",
	"bright_black": "90", "bright_red": "91", "bright_green": "92", "bright_yellow": "93", "bright_blue": "94",
	"bright_magenta": "95", "bright_cyan": "96", "bright_white": "97",
}

// Escapes square bracket tags and a trailing backslash so text prints literally, as rich.markup.escape does
func Escape(markup string) string {
	markup = escapeRE.ReplaceAllStringFunc(markup, func(m string) string {
		sub := escapeRE.FindStringSubmatch(m)
		return sub[1] + sub[1] + `\` + sub[2]
	})
	if strings.HasSuffix(markup, `\`) && !strings.HasSuffix(markup, `\\`) {
		return markup + `\`
	}
	return markup
}

// Plain text of console markup with every style tag removed and escaped tags kept literally
func Plain(markup string) string { return Render(markup, false) }

// Text of console markup, styled with ANSI SGR sequences when color is true
func Render(markup string, color bool) string {
	if !strings.Contains(markup, "[") {
		return markup
	}
	var b strings.Builder
	var stack []string
	apply := func() {
		if !color {
			return
		}
		b.WriteString("\x1b[0m")
		var codes []string
		for _, style := range stack {
			for _, word := range strings.Fields(style) {
				if code, ok := sgr[word]; ok {
					codes = append(codes, code)
				}
			}
		}
		if len(codes) > 0 {
			b.WriteString("\x1b[" + strings.Join(codes, ";") + "m")
		}
	}
	pos := 0
	for _, m := range tagRE.FindAllStringSubmatchIndex(markup, -1) {
		start, end := m[0], m[1]
		b.WriteString(strings.ReplaceAll(markup[pos:start], `\[`, "["))
		pos = end
		backslashes, escaped := (m[3]-m[2])/2, (m[3]-m[2])%2
		b.WriteString(strings.Repeat(`\`, backslashes))
		if escaped == 1 {
			b.WriteString(markup[m[3]:end])
			continue
		}
		tag := markup[m[4]:m[5]]
		if strings.HasPrefix(tag, "/") {
			name := strings.TrimSpace(tag[1:])
			for i := len(stack) - 1; i >= 0; i-- {
				if name == "" || stack[i] == name {
					stack = append(stack[:i], stack[i+1:]...)
					break
				}
			}
		} else if !strings.HasPrefix(tag, "@") {
			name, _, _ := strings.Cut(tag, "=")
			stack = append(stack, strings.TrimSpace(name))
		}
		apply()
	}
	b.WriteString(strings.ReplaceAll(markup[pos:], `\[`, "["))
	if color && len(stack) > 0 {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
