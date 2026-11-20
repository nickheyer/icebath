package cliui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Left margin of the lines a live prompt draws
const promptMargin = 2

// Rows below the cursor that a prompt repaints in place and finally replaces with lasting lines
type liveRegion struct {
	c      *Console
	out    io.Writer
	closed bool
}

// Opens a region at the cursor with the cursor hidden
func (c *Console) openRegion() *liveRegion {
	r := &liveRegion{c: c, out: c.Out}
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
	return max(rows-1, 1)
}

// Draws the frame over the previous one from the region top, where the cursor rests between frames
func (r *liveRegion) paint(lines []string) {
	rows, cols := r.size()
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
	for _, line := range final {
		b.WriteString(r.c.clip(line, cols-1))
		b.WriteString("\r\n")
	}
	io.WriteString(r.out, b.String())
	r.c.leaveMode("cursor")
}

// Columns a live prompt draws its text in, the terminal width less the margin and the spare last column
func bodyWidth(cols int) int { return max(cols-5, 10) }

// Lines a finished prompt leaves behind, its title dimmed before the answer and a faint note, wrapped under the title
func (c *Console) answerLines(title, answer string, tone textStyle, note string) []string {
	g, t := c.glyphs(), promptTheme
	_, cols, err := c.termSize()
	if err != nil || cols <= 0 {
		cols = 80
	}
	spans := []span{{title, textStyle{color: t.Muted}}, {"  ", textStyle{}}, {answer, tone}}
	if note != "" {
		spans = append(spans, span{" ", textStyle{}}, span{note, textStyle{color: t.Faint}})
	}
	margin := strings.Repeat(" ", promptMargin)
	return hung(c.spanRows(measureSpans(spans), spans, bodyWidth(cols)), margin+c.paint(g.answered, textStyle{color: t.Faint})+" ", margin+"  ")
}

// Title rows of a live prompt, the mark on the first and a long title wrapped under its text
func (c *Console) titleLines(title string, body int) []string {
	g, t := c.glyphs(), promptTheme
	mark := strings.Repeat(" ", promptMargin) + c.paint(g.active, textStyle{color: t.Accent}) + " "
	return hung(c.textRows(title, textStyle{bold: true}, body), mark, "    ")
}

// Key hint rows under a prompt, joined by the separator, a hint starting a new row when one is full
func (c *Console) hintRows(parts []string, body int, tone string) []string {
	sep := "  " + c.glyphs().sep + "  "
	var rows []string
	for _, part := range parts {
		for _, piece := range measure(part).rows(body) {
			if n := len(rows); n > 0 && ansi.StringWidth(rows[n-1]+sep+piece) <= body {
				rows[n-1] += sep + piece
			} else {
				rows = append(rows, piece)
			}
		}
	}
	for i, row := range rows {
		rows[i] = "    " + c.paint(row, textStyle{color: tone})
	}
	return rows
}

// Prompt title as shown, without surrounding space, color codes or a trailing colon
func cleanTitle(title string) string {
	text := strings.TrimSpace(ansi.Strip(title))
	text = strings.TrimSpace(strings.TrimSuffix(text, ":"))
	return strings.Join(strings.Fields(text), " ")
}
