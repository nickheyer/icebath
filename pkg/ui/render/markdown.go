// Text rendering shared by every card, markdown, code, diffs, tables and small formatters tuned to the theme
package render

import (
	"fmt"
	"image/color"
	"strings"
	"sync"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"

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
	cfg.Heading.Color = hex(t.Accent)
	cfg.H1.Color = hex(t.Bg)
	cfg.H1.BackgroundColor = hex(t.Accent)
	cfg.H2.Color = hex(t.Accent)
	cfg.H3.Color = hex(t.Ice)
	cfg.H4.Color = hex(t.Ice)
	cfg.H5.Color = hex(t.TextDim)
	cfg.H6.Color = hex(t.TextDim)
	cfg.Link.Color = hex(t.Ice)
	cfg.LinkText.Color = hex(t.Ice)
	cfg.Code.Color = hex(t.Pulse)
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
	return strings.TrimRight(out, "\n")
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
