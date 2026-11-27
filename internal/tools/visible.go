package tools

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/dlclark/regexp2/v2"
)

// Runs of code points outside printable ASCII, where hidden ones can sit
var unprintable = regexp.MustCompile("[^\x20-\x7e\t\n]+")

// Space a line can be padded with to push text out of view
const spaces = `[ \t\x{00a0}\x{1680}\x{2000}-\x{200a}\x{202f}\x{205f}\x{3000}]`

// Long runs of space after text, or a line of nothing but space
var padding = regexp2.MustCompile(`(?<=\S)` + spaces + `{21,}|(?m:^)` + spaces + `{65,}`)

// Three or more blank lines in a row
var blanks = regexp.MustCompile(`\n(?:` + spaces + `*\n){3,}`)

// Code points that render as nothing though they are letters or marks
var blankGlyphs = func() map[rune]bool {
	set := map[rune]bool{}
	for _, r := range "͏ᅟᅠ឴឵᠋᠌᠍᠏⠀ㅤﾠ" {
		set[r] = true
	}
	for r := rune(0xfe00); r < 0xfe10; r++ {
		set[r] = true
	}
	for r := rune(0xe0100); r < 0xe01f0; r++ {
		set[r] = true
	}
	return set
}()

// Text shown to the user to decide on, such as a command to approve or a question to answer, with everything that could hide part of it made
// visible: invisible and control code points escaped, long padding and runs of blank lines counted instead of shown
func Visible(text string) string {
	if text == "" {
		return ""
	}
	out := unprintable.ReplaceAllStringFunc(text, escapeHidden)
	out = blanks.ReplaceAllStringFunc(out, func(run string) string {
		return fmt.Sprintf("\n⟨+%d blank lines⟩\n", strings.Count(run, "\n")-1)
	})
	padded, err := padding.ReplaceFunc(out, func(m regexp2.Match) string {
		return fmt.Sprintf(" ⟨+%d whitespace chars⟩ ", len([]rune(m.String())))
	}, -1, -1)
	if err != nil {
		return out
	}
	return padded
}

func hidden(r rune) bool {
	assigned := unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.C)
	return blankGlyphs[r] || unicode.Is(unicode.C, r) || !assigned || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

// Escapes the hidden code points of a run, one repeated four or more times shown once with its count
func escapeHidden(run string) string {
	var b strings.Builder
	runes := []rune(run)
	for i := 0; i < len(runes); {
		r, n := runes[i], 1
		for i+n < len(runes) && runes[i+n] == r {
			n++
		}
		switch {
		case !hidden(r):
			b.WriteString(strings.Repeat(string(r), n))
		case n >= 4:
			fmt.Fprintf(&b, "⟨%s ×%d⟩", escape(r), n)
		default:
			b.WriteString(strings.Repeat(escape(r), n))
		}
		i += n
	}
	return b.String()
}

func escape(r rune) string {
	switch {
	case r < 0x100:
		return fmt.Sprintf(`\x%02x`, r)
	case r < 0x10000:
		return fmt.Sprintf(`\u%04x`, r)
	}
	return fmt.Sprintf(`\U%08x`, r)
}
