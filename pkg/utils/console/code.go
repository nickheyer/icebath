package consoleutil

import (
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Monokai theme code blocks are highlighted in, loaded on first use
var codeTheme = sync.OnceValue(func() *chroma.Style { return styles.Get("monokai") })

func chromaColor(c chroma.Colour) colorSpec {
	if !c.IsSet() {
		return colorSpec{}
	}
	return rgbColor(c.Red(), c.Green(), c.Blue())
}

// Style of a token under the theme, attributes the theme leaves unset switched off so surrounding styles stay out of the code
func tokenStyle(theme *chroma.Style, tt chroma.TokenType) Style {
	e := theme.Get(tt)
	st := Style{fg: chromaColor(e.Colour), bg: chromaColor(e.Background)}
	for _, a := range [...]struct {
		on   chroma.Trilean
		attr attr
	}{{e.Bold, attrBold}, {e.Italic, attrItalic}, {e.Underline, attrUnderline}} {
		if a.on == chroma.Yes {
			st.on |= a.attr
		} else {
			st.off |= a.attr
		}
	}
	return st
}

// Source with tabs expanded to multiples of size columns, counted in characters from each line start
func expandCodeTabs(code string, size int) string {
	if !strings.Contains(code, "\t") {
		return code
	}
	var b strings.Builder
	col := 0
	for _, r := range code {
		switch r {
		case '\t':
			n := size - col%size
			b.WriteString(spaces(n))
			col += n
		case '\n', '\r':
			b.WriteRune(r)
			col = 0
		default:
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

// Lines of a code block at width cells: highlighted for its language, word wrapped, set on the theme background with a cell of padding on every side
func codeLines(code, language string, width int) []segments {
	theme := codeTheme()
	bg := Style{bg: chromaColor(theme.Get(chroma.Background).Background)}
	code = expandCodeTabs(code, 4) + "\n"
	lexer := lexers.Fallback
	if language != "" {
		if l := lexers.Get(language); l != nil {
			lexer = l
		}
	}
	var highlighted segments
	if it, err := lexer.Tokenise(nil, code); err == nil {
		for tok := it(); tok != chroma.EOF; tok = it() {
			if tok.Value != "" {
				highlighted = append(highlighted, segment{tok.Value, tokenStyle(theme, tok.Type)})
			}
		}
	} else {
		highlighted = segments{{code, bg}}
	}
	if plain := highlighted.plain(); strings.HasSuffix(plain, "\n") {
		highlighted = highlighted.slice(0, len(plain)-1)
	}
	codeWidth := max(1, width-2)
	blank := segments{{spaces(width), bg}}
	out := []segments{blank}
	pad := segment{" ", bg}
	for _, line := range highlighted.lines() {
		for _, w := range wrap(line, codeWidth, wrapOptions{justify: "left", pad: bg}) {
			out = append(out, append(segments{pad}, w.padTo(codeWidth, bg)...).with(pad.text, pad.style))
		}
	}
	return append(out, blank)
}
