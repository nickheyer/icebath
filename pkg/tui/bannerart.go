package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
)

// One [style] or [/] tag of banner art markup
var artTagRe = regexp.MustCompile(`\[(/?)([^\[\]\n]*)\]`)

// Style a markup tag names, false when its words are not all attributes or colors and it is plain text
func artTagStyle(words string, ink func(string) string) (lipgloss.Style, bool) {
	st := lipgloss.NewStyle()
	fields := strings.Fields(words)
	if len(fields) == 0 {
		return st, false
	}
	for _, word := range fields {
		switch strings.ToLower(word) {
		case "bold":
			st = st.Bold(true)
		case "dim":
			st = st.Faint(true)
		case "italic":
			st = st.Italic(true)
		default:
			c := toneColor(ink(word))
			if _, none := c.(lipgloss.NoColor); none {
				return st, false
			}
			st = st.Foreground(c)
		}
	}
	return st, true
}

// Rows of banner art written in rich markup with blank edge rows dropped, and the cells of its widest row
func artRows(markup string, ink func(string) string) ([]string, int) {
	var rows []string
	var blank []bool
	width := 0
	for line := range strings.SplitSeq(strings.Trim(markup, "\n"), "\n") {
		var painted, plain strings.Builder
		st, styled := lipgloss.NewStyle(), false
		emit := func(text string) {
			if text == "" {
				return
			}
			plain.WriteString(text)
			if styled {
				text = st.Render(text)
			}
			painted.WriteString(text)
		}
		last := 0
		for _, m := range artTagRe.FindAllStringSubmatchIndex(line, -1) {
			tag, closing, words := line[m[0]:m[1]], line[m[2]:m[3]] == "/", line[m[4]:m[5]]
			switch next, ok := artTagStyle(words, ink); {
			case closing:
				emit(line[last:m[0]])
				styled = false
			case !closing && ok:
				emit(line[last:m[0]])
				st, styled = next, true
			default:
				emit(line[last:m[0]] + tag)
			}
			last = m[1]
		}
		emit(line[last:])
		rows = append(rows, painted.String())
		blank = append(blank, strings.Trim(plain.String(), " ⠀") == "")
		width = max(width, ansi.StringWidth(plain.String()))
	}
	for len(rows) > 0 && blank[0] {
		rows, blank = rows[1:], blank[1:]
	}
	for len(rows) > 0 && blank[len(rows)-1] {
		rows, blank = rows[:len(rows)-1], blank[:len(rows)-1]
	}
	return rows, width
}

// Banner art over the session banner: the wordmark when the window fits it, else the stacked mark, else none.
// A skin's own art paints as authored, the icebath art in the text and primary tones of the theme
func bannerArt(c renderCtx) []string {
	if c.compact || c.ut.Plain {
		return nil
	}
	logo, hero := c.t.BannerLogo, c.t.BannerHero
	ink := func(tone string) string { return tone }
	if strings.TrimSpace(logo) == "" && strings.TrimSpace(hero) == "" {
		logo, hero = uitheme.Logo, uitheme.Hero
		ink = func(tone string) string {
			switch strings.ToLower(tone) {
			case uitheme.LogoInk:
				return c.t.Color.Text
			case uitheme.LogoBar:
				return c.t.Color.Primary
			}
			return tone
		}
	}
	room := c.width - transcriptIndent*2
	pad := strings.Repeat(" ", transcriptIndent)
	for _, markup := range []string{logo, hero} {
		if strings.TrimSpace(markup) == "" {
			continue
		}
		rows, width := artRows(markup, ink)
		if len(rows) == 0 || width > room {
			continue
		}
		for i := range rows {
			rows[i] = pad + rows[i]
		}
		return rows
	}
	return nil
}
