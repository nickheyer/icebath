package tty

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/term"
)

// Renderer wired to icebath's emulator, answers flowing back the way Bubble Tea would deliver them
type rig struct {
	t       *testing.T
	r       *Renderer
	emu     *term.Emulator
	replies bytes.Buffer
	out     bytes.Buffer
	now     time.Time
	written int
	frames  int
	track   func([]byte)
}

// Rig of a w by h terminal
func newRig(t *testing.T, w, h int) *rig {
	g := &rig{t: t, now: time.Unix(1000, 0)}
	g.emu = term.New(w, h, term.Options{Reply: &g.replies})
	g.r = NewRenderer(&g.out, w, h)
	return g
}

// Feeds terminal answers back until the renderer has nothing more to write
func (g *rig) settle() {
	for i := 0; i < 12; i++ {
		g.now = g.now.Add(time.Millisecond)
		g.r.Flush(g.now)
		b := g.out.Bytes()
		g.written += len(b)
		if len(b) > 0 {
			if g.track != nil {
				g.track(b)
			}
			_, _ = g.emu.Write(b)
		}
		g.out.Reset()
		answered := g.answer()
		if len(b) == 0 && !answered {
			return
		}
	}
}

// Lets the exchange finish starting from a given time
func (g *rig) settleAt(at time.Time) {
	g.now = at
	g.settle()
}

// Delivers queued terminal answers as Bubble Tea messages, true when there were any
func (g *rig) answer() bool {
	buf := append([]byte(nil), g.replies.Bytes()...)
	g.replies.Reset()
	any := len(buf) > 0
	var dec uv.EventDecoder
	for len(buf) > 0 {
		n, ev := dec.Decode(buf)
		if n <= 0 {
			break
		}
		buf = buf[n:]
		g.r.Reply(teaMsg(ev))
	}
	return any
}

// Bubble Tea message of a decoded terminal event, as Program.translateInputEvent gives it
func teaMsg(ev uv.Event) tea.Msg {
	switch e := ev.(type) {
	case uv.CursorPositionEvent:
		return tea.CursorPositionMsg(e)
	case uv.ModeReportEvent:
		return tea.ModeReportMsg(e)
	case uv.KeyboardEnhancementsEvent:
		return tea.KeyboardEnhancementsMsg(e)
	}
	return ev
}

// Shows a view and lets the exchange with the terminal finish
func (g *rig) show(v tea.View) {
	g.frames++
	g.r.Render(v)
	g.settle()
}

// Frame the toolkit's own parser makes of content, the independent reference the screen must equal
func reference(content string, w, h int) *uv.Buffer {
	buf := uv.NewBuffer(w, h)
	scr := refScreen{buf}
	uv.NewStyledString(content).Draw(scr, scr.Bounds())
	return buf
}

// Buffer measuring with grapheme widths like the window manager's screen
type refScreen struct{ *uv.Buffer }

func (refScreen) WidthMethod() uv.WidthMethod { return uv.WidthMethod(ansi.GraphemeWidth) }

// Cell of the emulator or reference in tty's form
func fromUV(c *uv.Cell, links *linkTable) Cell {
	if c == nil {
		return blankCell
	}
	out := Cell{Text: c.Content, Width: uint8(c.Width), Link: links.id(c.Link.URL, c.Link.Params)}
	out.Style = Style{Fg: ColorOf(c.Style.Fg), Bg: ColorOf(c.Style.Bg), Ul: ColorOf(c.Style.UnderlineColor), Under: uint8(c.Style.Underline), Attrs: c.Style.Attrs}
	if out.Text == "" && out.Width == 1 {
		out.Text = " "
	}
	return out
}

// Differences between the emulator screen and the reference, as readable lines
func (g *rig) diff(ref *uv.Buffer) []string {
	var out []string
	links := newLinkTable()
	w, h := g.emu.Size()
	base := g.emu.Base()
	for y := 0; y < h; y++ {
		line, _ := g.emu.Row(base + uint64(y))
		for x := 0; x < w; x++ {
			var ec Cell
			if x < len(line.Cells) {
				ec = fromUV(&line.Cells[x], links)
			} else {
				ec = blankCell
			}
			rc := fromUV(ref.CellAt(x, y), links)
			if !sameCell(&ec, &rc) {
				out = append(out, fmt.Sprintf("(%d,%d) screen %q w%d %+v want %q w%d %+v", x, y, ec.Text, ec.Width, ec.shown().Style, rc.Text, rc.Width, rc.shown().Style))
			}
		}
	}
	return out
}

// Fails the test when the screen differs from the reference of content
func (g *rig) check(content string, label string) {
	g.t.Helper()
	w, h := g.emu.Size()
	if d := g.diff(reference(content, w, h)); len(d) > 0 {
		g.t.Fatalf("%s: %d cells differ, first %s", label, len(d), strings.Join(d[:min(len(d), 6)], "\n"))
	}
}

// Alternate screen view of content with the cursor at x, y
func screenView(content string, x, y int) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	v.Cursor = tea.NewCursor(x, y)
	return v
}
