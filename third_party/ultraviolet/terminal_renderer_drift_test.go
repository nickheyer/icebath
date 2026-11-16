package uv

import (
	"bytes"
	"strings"
	"testing"
)

// Renders two frames on one renderer and returns what the second frame wrote
func driftFrames(t *testing.T, fullscreen, grapheme bool, w, h int, first, second func(ScreenBuffer)) string {
	t.Helper()
	var buf bytes.Buffer
	r := NewTerminalRenderer(&buf, []string{"TERM=xterm-256color"})
	r.SetFullscreen(fullscreen)
	r.SetGraphemeWidth(grapheme)
	r.Resize(w, h)
	scr := NewScreenBuffer(w, h)
	first(scr)
	r.Render(scr.RenderBuffer)
	if err := r.Flush(); err != nil {
		t.Fatalf("flush %v", err)
	}
	buf.Reset()
	second(scr)
	r.Render(scr.RenderBuffer)
	if err := r.Flush(); err != nil {
		t.Fatalf("flush %v", err)
	}
	return buf.String()
}

// Draws a status row whose face holds a wide cell, then the given timer text after it
func drawStatus(b ScreenBuffer, timer string) {
	NewStyledString("(｡•́︿•̀｡) ruminating " + timer).Draw(b, Rect(0, 0, b.Width(), 1))
}

func TestDriftRowUnchangedIsNotRewritten(t *testing.T) {
	for _, grapheme := range []bool{false, true} {
		out := driftFrames(t, true, grapheme, 40, 3, func(b ScreenBuffer) {
			drawStatus(b, "6.0s")
			NewStyledString("hello").Draw(b, Rect(0, 1, 40, 1))
		}, func(b ScreenBuffer) {
			b.TouchLine(0, 0, 40)
			NewStyledString("world").Draw(b, Rect(0, 1, 40, 1))
		})
		if strings.Contains(out, "ruminating") || strings.Contains(out, "︿") {
			t.Errorf("grapheme=%v unchanged drift row rewritten %q", grapheme, out)
		}
		if !strings.Contains(out, "world") {
			t.Errorf("grapheme=%v changed row missing %q", grapheme, out)
		}
	}
}

func TestDriftRowRepaintsFromFirstChangeWhenWidthsAgree(t *testing.T) {
	out := driftFrames(t, true, true, 40, 3, func(b ScreenBuffer) {
		drawStatus(b, "6.0s")
	}, func(b ScreenBuffer) {
		drawStatus(b, "7.0s")
	})
	if strings.Contains(out, "ruminating") || !strings.Contains(out, "7.0s") {
		t.Errorf("grapheme row should repaint only the changed tail %q", out)
	}
}

func TestDriftRowRepaintsWholeWhenPrefixMayDrift(t *testing.T) {
	out := driftFrames(t, true, false, 40, 3, func(b ScreenBuffer) {
		drawStatus(b, "6.0s")
	}, func(b ScreenBuffer) {
		drawStatus(b, "7.0s")
	})
	if !strings.Contains(out, "ruminating 7.0s") || !strings.Contains(out, "︿") {
		t.Errorf("row after a cell the terminal may measure differently should repaint whole %q", out)
	}
}

func TestDriftRowPlainPrefixRepaintsFromFirstChange(t *testing.T) {
	out := driftFrames(t, true, false, 40, 3, func(b ScreenBuffer) {
		NewStyledString("plain prefix 6.0s 世界").Draw(b, Rect(0, 0, 40, 1))
	}, func(b ScreenBuffer) {
		NewStyledString("plain prefix 7.0s 世界").Draw(b, Rect(0, 0, 40, 1))
	})
	if strings.Contains(out, "plain prefix") || !strings.Contains(out, "7.0s 世界") {
		t.Errorf("row with a plain prefix should repaint from the first change %q", out)
	}
}

func TestDriftRowRepaintsAfterInlineWidthChange(t *testing.T) {
	var buf bytes.Buffer
	r := NewTerminalRenderer(&buf, []string{"TERM=xterm-256color"})
	r.Resize(80, 2)
	wide := NewScreenBuffer(20, 1)
	NewStyledString("世界").Draw(wide, wide.Bounds())
	r.Render(wide.RenderBuffer)
	if err := r.Flush(); err != nil {
		t.Fatalf("flush %v", err)
	}
	buf.Reset()
	narrow := NewScreenBuffer(10, 1)
	NewStyledString("世界").Draw(narrow, narrow.Bounds())
	r.Render(narrow.RenderBuffer)
	if err := r.Flush(); err != nil {
		t.Fatalf("flush %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "世界") {
		t.Errorf("drift row must repaint after the frame width changed %q", out)
	}
}

func TestPlainRowSkipsUnchangedRunBetweenChanges(t *testing.T) {
	out := driftFrames(t, true, false, 60, 2, func(b ScreenBuffer) {
		NewStyledString("a unchanged middle stretch of plain text b |").Draw(b, Rect(0, 0, 60, 1))
	}, func(b ScreenBuffer) {
		NewStyledString("A unchanged middle stretch of plain text B |").Draw(b, Rect(0, 0, 60, 1))
	})
	if strings.Contains(out, "middle") || !strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Errorf("unchanged run between two changes rewritten %q", out)
	}
}
