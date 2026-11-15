package cliui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Rows below the cursor that a prompt repaints in place and finally replaces with lasting lines
type liveRegion struct {
	c      *Console
	out    io.Writer
	flow   *flowWriter
	before lineKind
	lead   bool
	closed bool
}

// Opens a region at the cursor with the cursor hidden, a blank row leading it after other output
func (c *Console) openRegion() *liveRegion {
	r := &liveRegion{c: c, out: rawWriter(c.Out), flow: c.flow()}
	if r.flow != nil {
		r.flow.stopBusy()
		r.before = r.flow.settle()
	}
	r.lead = r.before == kindText || r.before == kindAnswer
	c.enterMode("cursor", "\x1b[?25l", "\x1b[?25h")
	return r
}

// Terminal rows and columns, eighty by twenty four when the size is unknown
func (r *liveRegion) size() (int, int) {
	rows, cols, err := r.c.termSize()
	if err != nil || rows <= 0 || cols <= 0 {
		return 24, 80
	}
	return rows, cols
}

// Rows a frame may use so the region never scrolls its own top off the screen
func (r *liveRegion) maxRows() int {
	rows, _ := r.size()
	budget := rows - 1
	if r.lead {
		budget--
	}
	return max(budget, 1)
}

// Draws the frame over the previous one from the region top, where the cursor rests between frames
func (r *liveRegion) paint(lines []string) {
	rows, cols := r.size()
	if r.lead {
		lines = append([]string{""}, lines...)
	}
	if len(lines) > rows-1 {
		lines = lines[:max(rows-1, 1)]
	}
	var b strings.Builder
	b.WriteString("\x1b[?2026h\r")
	for i, line := range lines {
		b.WriteString("\x1b[2K")
		b.WriteString(r.c.clip(line, cols-1))
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[0J")
	if up := len(lines) - 1; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	b.WriteString("\r\x1b[?2026l")
	io.WriteString(r.out, b.String())
}

// Erases the region, prints the lasting lines in its place and shows the cursor again
func (r *liveRegion) close(final []string) {
	if r.closed {
		return
	}
	r.closed = true
	_, cols := r.size()
	var b strings.Builder
	b.WriteString("\r\x1b[0J")
	if len(final) > 0 && r.before == kindText {
		b.WriteString("\r\n")
	}
	for _, line := range final {
		b.WriteString(r.c.clip(line, cols-1))
		b.WriteString("\r\n")
	}
	io.WriteString(r.out, b.String())
	r.c.leaveMode("cursor")
	if r.flow != nil && len(final) > 0 {
		r.flow.note(kindAnswer)
	}
}

// Line a finished prompt leaves behind, its title dimmed beside the answer, both cut to share the row
func (c *Console) answerLine(title, answer string, tone textStyle) string {
	g, t := c.glyphs(), activeTheme()
	_, cols, err := c.termSize()
	if err != nil || cols <= 0 {
		cols = 80
	}
	room := max(cols-1-flowMargin-4, 16)
	title = c.clip(title, max(room-min(ansi.StringWidth(answer), room/2), 8))
	answer = c.clip(answer, max(room-ansi.StringWidth(title), 4))
	return strings.Repeat(" ", flowMargin) + c.paint(g.answered, textStyle{color: t.Faint}) + " " +
		c.paint(title, textStyle{color: t.Muted}) + "  " + c.paint(answer, tone)
}

// Title rows of a live prompt, the mark on the first and a long title wrapped under its text
func (c *Console) titleLines(title string, body int) []string {
	g, t := c.glyphs(), activeTheme()
	var lines []string
	for i, part := range wrapWords(title, max(body, 12)) {
		lead := "    "
		if i == 0 {
			lead = strings.Repeat(" ", flowMargin) + c.paint(g.active, textStyle{color: t.Accent}) + " "
		}
		lines = append(lines, lead+c.paint(c.clip(part, body), textStyle{bold: true}))
	}
	return lines
}

// Prompt title as shown, without surrounding space, color codes or a trailing colon
func cleanTitle(title string) string {
	text := strings.TrimSpace(ansi.Strip(title))
	text = strings.TrimSpace(strings.TrimSuffix(text, ":"))
	return strings.Join(strings.Fields(text), " ")
}
