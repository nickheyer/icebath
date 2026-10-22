package cards

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Filter row with a glyph, the query, a cursor while it takes typing and a count at the right
type searchBar struct {
	query  string
	active bool
}

// Takes typing, keeping any query it already holds
func (b *searchBar) Open() { b.active = true }

// Keeps the query and stops taking typing
func (b *searchBar) Accept() { b.active = false }

// Drops the query and stops taking typing
func (b *searchBar) Close() {
	b.active = false
	b.query = ""
}

// Reports whether the bar takes typing
func (b *searchBar) Active() bool { return b.active }

// Reports whether the bar shows, while it takes typing or holds a query
func (b *searchBar) Shown() bool { return b.active || b.query != "" }

// Shows the bar taking typing, or drops it with its query when it already shows
func (b *searchBar) Toggle() {
	if b.Shown() {
		b.Close()
		return
	}
	b.Open()
}

// Keymap contexts while the bar takes typing, the toggle key closing it again until the first character
func (b *searchBar) Contexts() []string {
	if b.query == "" {
		return []string{keys.Search, keys.Filter}
	}
	return []string{keys.Search}
}

// Text searched for
func (b *searchBar) Query() string { return b.query }

// Carries out a search editing action, reporting whether the query changed
func (b *searchBar) Action(id string) bool {
	switch id {
	case "search.delete.back":
		if r := []rune(b.query); len(r) > 0 {
			b.query = string(r[:len(r)-1])
			return true
		}
	case "search.clear":
		if b.query != "" {
			b.query = ""
			return true
		}
	case "search.delete.word":
		r := []rune(b.query)
		i := len(r)
		for i > 0 && r[i-1] == ' ' {
			i--
		}
		for i > 0 && r[i-1] != ' ' {
			i--
		}
		if i != len(r) {
			b.query = string(r[:i])
			return true
		}
	}
	return false
}

// Appends typed text to the query, line breaks folded into spaces
func (b *searchBar) Type(text string) {
	b.query += strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
}

// Column the terminal cursor sits at inside the bar
func (b *searchBar) CursorX() int { return 3 + ansi.StringWidth(b.query) }

// Row at width with the count at the right, lit as a field while it takes typing
func (b *searchBar) View(w int, count string, placeholder string, t theme.Theme) string {
	glyph := "⌕"
	if t.Plain {
		glyph = "/"
	}
	base := lipgloss.NewStyle()
	if b.active {
		base = base.Background(t.Ghost)
	}
	text := base.Foreground(t.Text).Render(b.query)
	if b.query == "" {
		text = base.Foreground(t.TextDim).Render(placeholder)
	}
	mark := t.TextDim
	if b.active || b.query != "" {
		mark = t.Accent
	}
	left := base.Foreground(mark).Render(" "+glyph+" ") + text
	right := ""
	if count != "" {
		right = base.Foreground(t.TextDim).Render(count + " ")
	}
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if lw+rw+1 > w {
		return render.Fit(left, w)
	}
	return left + base.Render(strings.Repeat(" ", w-lw-rw)) + right
}
