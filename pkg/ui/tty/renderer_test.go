package tty

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

func TestHandshakeLearnsWhatTheTerminalDoes(t *testing.T) {
	g := newRig(t, 40, 10)
	g.show(screenView("hello", 0, 0))
	if !g.r.pl.rep || !g.r.syncOK || g.r.unsetGraphemes || g.r.widths.sized || g.r.shaking {
		t.Fatalf("rep %v sync %v unset graphemes %v sized %v shaking %v", g.r.pl.rep, g.r.syncOK, g.r.unsetGraphemes, g.r.widths.sized, g.r.shaking)
	}
	g.check("hello", "after the handshake")
}

func TestFirstPaintWaitsForTheHandshake(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, 20, 4)
	r.Render(screenView("waiting", 0, 0))
	start := time.Unix(5, 0)
	r.Flush(start)
	if bytes.Contains(out.Bytes(), []byte("waiting")) {
		t.Fatalf("painted before the terminal answered")
	}
	out.Reset()
	r.Flush(start.Add(handshakeLimit))
	if !bytes.Contains(out.Bytes(), []byte("waiting")) {
		t.Fatalf("did not paint once the handshake ran out, wrote %q", out.Bytes())
	}
}

func TestForeignQueriesKeepTheirAnswers(t *testing.T) {
	g := newRig(t, 20, 4)
	g.show(screenView("x", 0, 0))
	g.r.Write([]byte("\x1b[6n\x1b[c"))
	g.r.Flush(g.now.Add(time.Second))
	_, _ = g.emu.Write(g.out.Bytes())
	g.out.Reset()
	if g.r.cursorReport(0, 0) {
		t.Fatalf("a cursor report asked by someone else was swallowed")
	}
	if g.r.deviceAttributes() {
		t.Fatalf("device attributes asked by someone else were swallowed")
	}
}

func TestPassthroughGoesAheadOfTheFrame(t *testing.T) {
	g := newRig(t, 20, 4)
	g.show(screenView("one", 0, 0))
	g.r.Write([]byte("\x1b_Gi=9,a=q;AAAA\x1b\\"))
	g.r.Render(screenView("two", 0, 0))
	g.r.Flush(g.now.Add(time.Second))
	b := g.out.String()
	if i, j := strings.Index(b, "\x1b_G"), strings.Index(b, "two"); i < 0 || j < 0 || i > j {
		t.Fatalf("passthrough not ahead of the frame in %q", b)
	}
}

func TestPassthroughEffects(t *testing.T) {
	for s, want := range map[string]effect{
		"\a": {}, "\x1b_Ga=T,C=1;x\x1b\\": {}, "\x1b]52;c;eA==\a": {}, "\x1b[16t\x1b[14t\x1b[c": {},
		"\x1bPtmux;\x1b\x1b_Ga=T,C=1;x\x1b\x1b\\\x1b\\": {},
		"\x1b_Ga=T;x\x1b\\":                             {cursor: true},
		"\x1b7\x1b[5;5H\x1bPq#0~\x1b\\\x1b8":            {pixels: true},
		"\x1b]1337;File=:AAA\a":                         {cursor: true, pixels: true},
		"\x1b]8;;x\a":                                   {sticky: true},
		"\x1b[5;5H":                                     {cursor: true},
		"\x1b7\x1b[H":                                   {cursor: true},
		"x":                                             {cursor: true, edits: true},
		"\x1b7\x1b[0m\x1b[3;3H   \x1b8":                 {edits: true},
		"\x1b[?25l":                                     {shown: -1},
	} {
		if got := classify([]byte(s)); got != want {
			t.Errorf("classify(%q) = %+v, want %+v", s, got, want)
		}
	}
}

func TestProfileMapsCellColors(t *testing.T) {
	g := newRig(t, 20, 2)
	g.r.SetProfile(colorprofile.ANSI256)
	g.show(screenView("\x1b[38;2;255;0;0mred\x1b[m", 0, 0))
	g.check("\x1b[38;5;196mred\x1b[m", "ansi256")
	g.r.SetProfile(colorprofile.ASCII)
	g.show(screenView("\x1b[38;2;255;0;0;1mred\x1b[m", 0, 0))
	g.check("\x1b[1mred\x1b[m", "ascii")
}

func TestReleaseUndoesTheSession(t *testing.T) {
	g := newRig(t, 20, 4)
	v := screenView("x", 0, 0)
	v.MouseMode, v.ReportFocus, v.Cursor = tea.MouseModeAllMotion, true, nil
	g.show(v)
	g.r.Release()
	b := g.out.String()
	_, _ = g.emu.Write(g.out.Bytes())
	for _, part := range []string{ansi.ResetModifyOtherKeys, ansi.PopKittyKeyboard(1), "\x1b[?7h", ansi.ResetModeAltScreenSaveCursor, ansi.SetModeTextCursorEnable, ansi.ResetModeFocusEvent, ansi.ResetModeMouseAnyEvent} {
		if !strings.Contains(b, part) {
			t.Errorf("release %q lacks %q", b, part)
		}
	}
	if g.emu.AltScreen() {
		t.Fatalf("still on the alternate screen")
	}
}

func TestResizeWaitsForTheNewView(t *testing.T) {
	g := newRig(t, 30, 6)
	g.show(screenView("before", 0, 0))
	g.emu.Resize(20, 5)
	g.r.Resize(20, 5)
	g.r.Flush(g.now.Add(time.Second))
	if g.out.Len() != 0 {
		t.Fatalf("painted the old view at the new size, %q", g.out.Bytes())
	}
	g.show(screenView("after", 0, 0))
	g.check("after", "resized")
}

// Flushes at the given time and returns the bytes written
func flushAt(r *Renderer, out *bytes.Buffer, at time.Time) string {
	out.Reset()
	r.Flush(at)
	return out.String()
}

func TestTerminalThatAnswersNothingIsPaintedOnceInLayoutWidths(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out, 24, 4)
	content := "┌──────┐\n│ 漢字 │\n└──────┘"
	r.Render(screenView(content, 0, 0))
	start := time.Unix(5, 0)
	flushAt(r, &out, start)
	first := flushAt(r, &out, start.Add(handshakeLimit))
	if !r.silent || !strings.Contains(first, "┌──────┐") || !strings.Contains(first, "漢字") {
		t.Fatalf("silent %v, first paint %q", r.silent, first)
	}
	if strings.Contains(first, "\x1b[6n") {
		t.Fatalf("a width probe went to a terminal that answers nothing: %q", first)
	}
	for i := 1; i <= 5; i++ {
		r.Render(screenView(content, 0, 0))
		if again := flushAt(r, &out, start.Add(handshakeLimit+time.Duration(i)*time.Second)); again != "" {
			t.Fatalf("unchanged frame %d wrote %q", i, again)
		}
	}
	r.Render(screenView(strings.Replace(content, "漢字", "漢语", 1), 0, 0))
	changed := flushAt(r, &out, start.Add(handshakeLimit+10*time.Second))
	if !strings.Contains(changed, "语") || strings.Contains(changed, "┌") || strings.Contains(changed, "\x1b[6n") {
		t.Fatalf("changed frame wrote %q", changed)
	}
}

func TestSilentTerminalIsMeasuredFromItsFirstAnswer(t *testing.T) {
	g := newRig(t, 24, 4)
	content := "│ 漢字 │"
	g.r.Render(screenView(content, 0, 0))
	start := g.now
	_, _ = g.emu.Write([]byte(flushAt(g.r, &g.out, start)))
	late := flushAt(g.r, &g.out, start.Add(handshakeLimit))
	_, _ = g.emu.Write([]byte(late))
	g.out.Reset()
	if !g.r.silent || !strings.Contains(late, "漢字") {
		t.Fatalf("silent %v, paint %q", g.r.silent, late)
	}
	if !g.answer() {
		t.Fatal("the emulator had no answers to deliver")
	}
	if g.r.silent || g.r.shaking || !g.r.repaint {
		t.Fatalf("after the answers silent %v shaking %v repaint %v", g.r.silent, g.r.shaking, g.r.repaint)
	}
	g.settleAt(start.Add(handshakeLimit + time.Second))
	if n, known := g.r.widths.of("漢", encText); !known || n != 2 {
		t.Fatalf("width of a wide cluster after the terminal answered %d known %v", n, known)
	}
	g.check(content, "after the terminal began answering")
}
