// Text rendering shared by every card, markdown, code, diffs, tables and small formatters tuned to the theme
package render

import (
	"bytes"
	"fmt"
	"image/color"
	"path/filepath"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Markdown renderer cached per theme and width
type Markdown struct {
	mu    sync.Mutex
	cache map[string]*glamour.TermRenderer
}

func NewMarkdown() *Markdown { return &Markdown{cache: map[string]*glamour.TermRenderer{}} }

var shared = NewMarkdown()

// Renders markdown at width in the theme using the shared cache
func MarkdownText(src string, width int, t theme.Theme) string {
	return shared.Render(src, width, t)
}

// Hex form of a color as glamour styles expect
func hex(c color.Color) *string {
	if c == nil {
		return nil
	}
	r, g, b, _ := c.RGBA()
	s := fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
	return &s
}

// Glamour style derived from the theme, starting from the dark, light or ASCII set
func StyleConfig(t theme.Theme) ansi.StyleConfig {
	var cfg ansi.StyleConfig
	switch {
	case t.Plain:
		cfg = styles.ASCIIStyleConfig
	case isLight(t):
		cfg = styles.LightStyleConfig
	default:
		cfg = styles.DarkStyleConfig
	}
	zero := uint(0)
	cfg.Document.Margin = &zero
	cfg.Document.BlockPrefix = ""
	cfg.Document.BlockSuffix = ""
	if t.Plain {
		return cfg
	}
	cfg.Document.Color = hex(t.Text)
	cfg.Text.Color = hex(t.Text)
	cfg.Heading.Color = hex(t.Text)
	cfg.H1.Color = hex(t.Text)
	cfg.H1.BackgroundColor = hex(t.Steel)
	cfg.H2.Color = hex(t.Ice)
	cfg.H3.Color = hex(t.Ice)
	cfg.H4.Color = hex(t.Ice)
	cfg.H5.Color = hex(t.TextDim)
	cfg.H6.Color = hex(t.TextDim)
	cfg.Link.Color = hex(t.Ice)
	cfg.LinkText.Color = hex(t.Ice)
	cfg.Code.Color = hex(t.Ice)
	cfg.Code.BackgroundColor = hex(t.Surface)
	cfg.BlockQuote.Color = hex(t.TextDim)
	cfg.Emph.Color = hex(t.Text)
	cfg.Strong.Color = hex(t.Text)
	cfg.Item.Color = hex(t.Text)
	cfg.Enumeration.Color = hex(t.Ice)
	cfg.HorizontalRule.Color = hex(t.Steel)
	cfg.CodeBlock.Theme = CodeStyle(t)
	return cfg
}

// Reports whether the theme has a light background
func isLight(t theme.Theme) bool {
	if t.Bg == nil {
		return false
	}
	r, g, b, _ := t.Bg.RGBA()
	return (r>>8)+(g>>8)+(b>>8) > 3*128
}

// Renders markdown at width in the theme, falling back to the raw text when glamour refuses the input
func (m *Markdown) Render(src string, width int, t theme.Theme) string {
	if width < 10 {
		width = 10
	}
	key := fmt.Sprintf("%s:%d", t.Name, width)
	m.mu.Lock()
	r, ok := m.cache[key]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithStyles(StyleConfig(t)), glamour.WithWordWrap(width), glamour.WithPreservedNewLines())
		if err != nil {
			m.mu.Unlock()
			return src
		}
		m.cache[key] = r
	}
	m.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	out, err := r.Render(src)
	if err != nil {
		return src
	}
	return compact(strings.TrimRight(out, "\n"))
}

// Drops the padding glamour puts after every line, keeping spaces that carry a background color
func compact(out string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if cut, ok := paddingStart(l); ok {
			lines[i] = l[:cut] + "\x1b[m"
		}
	}
	return strings.Join(lines, "\n")
}

// Start of the trailing spaces and style sequences that paint nothing, false when a line has no such spaces
func paddingStart(l string) (int, bool) {
	end, spaces := len(l), 0
	for end > 0 && (l[end-1] == ' ' || l[end-1] == 'm') {
		if l[end-1] == ' ' {
			end--
			spaces++
			continue
		}
		start := strings.LastIndex(l[:end-1], "\x1b[")
		if start < 0 || strings.Trim(l[start+2:end-1], "0123456789;") != "" {
			return 0, false
		}
		end = start
	}
	if spaces == 0 {
		return 0, false
	}
	return max(end, lastPainted(l, end)), true
}

// Offset past the last space at or after from under a background or reverse video, zero when none is
func lastPainted(l string, from int) int {
	bg, last := false, 0
	for i := 0; i < len(l); {
		if strings.HasPrefix(l[i:], "\x1b[") {
			if j := strings.IndexByte(l[i:], 'm'); j > 0 && strings.Trim(l[i+2:i+j], "0123456789;") == "" {
				bg = background(bg, l[i+2:i+j])
				i += j + 1
				continue
			}
		}
		if i >= from && l[i] == ' ' && bg {
			last = i + 1
		}
		i++
	}
	return last
}

// Background or reverse video state after a style sequence with the parameters is applied to the state before it
func background(bg bool, params string) bool {
	if params == "" {
		return false
	}
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		switch {
		case p == "0" || p == "49" || p == "27":
			bg = false
		case p == "7" || p == "48" || len(p) == 2 && p[0] == '4' || len(p) == 3 && p[0] == '1' && p[1] == '0':
			bg = true
		}
		if p == "38" || p == "48" || p == "58" {
			if i+1 < len(parts) && parts[i+1] == "2" {
				i += 4
			} else {
				i += 2
			}
		}
	}
	return bg
}

// Hyperlink text that opens url in terminals that follow OSC 8
func Link(text, url string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Ice).Underline(true).Hyperlink(url).Render(text)
}

// Hyperlink naming a file position and opening the file
func FileLink(path string, line int, t theme.Theme) string {
	label := path
	if line > 0 {
		label = fmt.Sprintf("%s:%d", path, line)
	}
	url := "file://" + path
	if line > 0 {
		url += fmt.Sprintf("#%d", line)
	}
	return Link(label, url, t)
}

// Chroma style name that suits the theme
func CodeStyle(t theme.Theme) string {
	switch {
	case t.Plain:
		return "bw"
	case t.Name == "contrast":
		return "github-dark"
	case isLight(t):
		return "github"
	}
	return "catppuccin-mocha"
}

// Lexer for a language name or a file name, analysed from the text when neither is known
func lexerFor(lang, path, src string) chroma.Lexer {
	var l chroma.Lexer
	if lang != "" {
		l = lexers.Get(lang)
	}
	if l == nil && path != "" {
		l = lexers.Match(filepath.Base(path))
	}
	if l == nil && src != "" {
		l = lexers.Analyse(src)
	}
	if l == nil {
		l = lexers.Fallback
	}
	return chroma.Coalesce(l)
}

// Highlights source for the language or file name in the theme, one line per input line
func Code(src, lang, path string, t theme.Theme) string {
	src = strings.TrimRight(src, "\n")
	if src == "" {
		return ""
	}
	l := lexerFor(lang, path, src)
	formatter := formatters.Get("terminal16m")
	if t.Plain {
		formatter = formatters.Get("terminal16")
	}
	if formatter == nil {
		formatter = formatters.Fallback
	}
	style := chromastyles.Get(CodeStyle(t))
	if style == nil {
		style = chromastyles.Fallback
	}
	it, err := l.Tokenise(nil, src)
	if err != nil {
		return src
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, it); err != nil {
		return src
	}
	return strings.TrimRight(buf.String(), "\n")
}

// Language a lexer would call the file, empty when none matches
func Language(path string) string {
	l := lexers.Match(filepath.Base(path))
	if l == nil {
		return ""
	}
	return l.Config().Name
}
