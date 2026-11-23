package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

var _ cards.Readable = (*transcriptView)(nil)

// Rows of the transcript as last laid out
func (v *transcriptView) ReadRows() int { return len(v.lines) }

// Plain text of a transcript row
func (v *transcriptView) ReadRow(i int) string {
	if i < 0 || i >= len(v.lines) {
		return ""
	}
	return ansi.Strip(v.lines[i])
}

// First row in view and the rows of the window above the reading line
func (v *transcriptView) ReadWindow() (int, int) { return v.top(), max(v.height-1, 1) }

// Scrolls so that the row is the first in view, following again at the newest page
func (v *transcriptView) ReadScroll(top int) {
	last := max(v.total-v.height, 0)
	v.offset = min(max(top, 0), last)
	v.follow = v.offset >= last
	v.manualAt = v.now()
}

// Row of the prompt before or after a row
func (v *transcriptView) ReadPrompt(from, d int) (int, bool) {
	if d < 0 {
		for i := len(v.prompts) - 1; i >= 0; i-- {
			if v.prompts[i].row < from {
				return v.prompts[i].row, true
			}
		}
		return 0, false
	}
	for _, p := range v.prompts {
		if p.row > from {
			return p.row, true
		}
	}
	return 0, false
}

// Rows of the message holding a row
func (v *transcriptView) ReadBlock(row int) (int, int) {
	for _, b := range v.placed {
		if row >= b.row && row < b.row+b.rows {
			return b.row, b.row + b.rows
		}
	}
	return row, row + 1
}

// Whether reading mode holds the keys of the session card
func (v *transcriptView) reading() bool { return v.reader != nil && v.reader.Active() }

// Keymap contexts of the session card while it reads its transcript
func (c *sessionCard) readContexts() []string { return c.view.reader.Contexts() }

// Typed or pasted text while the transcript is read, taken into a search being typed and else dropped so nothing reaches the prompt
func (c *sessionCard) readKey(m tea.Msg) tea.Cmd {
	c.view.reader.Take(m)
	return nil
}

// Reading keys of the session card: the leader keys start reading or a search, the open key opens or folds the part under the cursor, and the
// reader carries out the rest; false for a key that is none of these
func (a *app) readAction(id string) (tea.Cmd, bool) {
	v := a.card.view
	switch id {
	case "view.start", "view.find":
		if v.reader == nil {
			v.reader = cards.NewReader(v, a.keys)
		}
		v.reader.Start(id == "view.find")
		return nil, true
	}
	if !v.reading() {
		return nil, false
	}
	if id == "view.open" {
		row := v.reader.Cursor()
		if block, part, found := v.partAt(row); found {
			return a.clickPart(block, part, row, 0), true
		}
		return nil, true
	}
	if cmd, taken := v.reader.Action(id); taken || strings.HasPrefix(id, "view.") || strings.HasPrefix(id, "search.") || strings.HasPrefix(id, "text.") {
		return cmd, true
	}
	return nil, false
}

var _ wm.SweepScroller = (*sessionCard)(nil)

// Rows of the session window the transcript fills, what a sweep held past them scrolls
func (c *sessionCard) SweepArea() (int, int) { return c.viewTop, c.viewTop + c.view.height }

// Scrolls the transcript under a sweep held past its edge, returning the rows it moved
func (c *sessionCard) SweepScroll(rows int) int {
	before := c.view.top()
	c.view.scrollBy(rows)
	return c.view.top() - before
}

// Plain text of the transcript rows a sweep covers, counted from the first row of the session window, rows scrolled out of view included
func (c *sessionCard) SweepRows(from, to int) []string {
	out := make([]string, 0, max(to-from+1, 0))
	for row := from; row <= to; row++ {
		out = append(out, c.view.ReadRow(c.view.top()+row-c.viewTop))
	}
	return out
}

// Part of the session window a dialog shows in, the rows above the prompt, so what is typed while the dialog holds its keys back stays in view
func (c *sessionCard) SheetArea(w, h int) wm.Box {
	top := c.composerTop
	if top <= 0 || top > h {
		top = h
	}
	return wm.Box{W: w, H: top}
}

// Terminal rows at or below which the session window spends its rows on the transcript rather than on spare prompt rows
const smallScreenRows = 24
