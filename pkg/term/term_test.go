package term

import (
	"bytes"
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Emulator of w by h with its answers collected
func emu(w, h int) (*Emulator, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return New(w, h, Options{Scrollback: 100, Reply: out, Fg: color.White, Bg: color.Black, Cursor: color.White}), out
}

// Text of screen row y with trailing blanks dropped
func row(e *Emulator, y int) string {
	buf := uv.NewBuffer(e.w, e.h)
	e.Draw(buf, View{})
	var sb strings.Builder
	for _, c := range buf.Lines[y] {
		if c.Width > 0 {
			sb.WriteString(c.Content)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

func rows(e *Emulator) []string {
	out := make([]string, e.h)
	for y := range out {
		out[y] = row(e, y)
	}
	return out
}

func cursorAt(t *testing.T, e *Emulator, x, y int) {
	t.Helper()
	c := e.Cursor()
	if c.X != x || c.Y != y {
		t.Fatalf("cursor at %d,%d want %d,%d", c.X, c.Y, x, y)
	}
}

func TestPrintWrapsAndScrolls(t *testing.T) {
	e, _ := emu(5, 3)
	e.WriteString("abcdefg\r\nhij")
	if got := rows(e); got[0] != "abcde" || got[1] != "fg" || got[2] != "hij" {
		t.Fatalf("after wrap: %q", got)
	}
	e.WriteString("\r\nklm")
	if got := rows(e); got[0] != "fg" || got[1] != "hij" || got[2] != "klm" {
		t.Fatalf("after scroll: %q", got)
	}
	if e.ScrollbackLen() != 1 {
		t.Fatalf("scrollback %d want 1", e.ScrollbackLen())
	}
	l, _ := e.Row(0)
	if !l.Wrapped || strings.TrimRight(uv.Line(l.Cells).String(), " ") != "abcde" {
		t.Fatalf("first scrollback row %q wrapped %v", uv.Line(l.Cells).String(), l.Wrapped)
	}
}

func TestPendingWrapOverwrites(t *testing.T) {
	e, _ := emu(3, 2)
	e.WriteString("abc")
	cursorAt(t, e, 2, 0)
	e.WriteString("\x1b[?7l")
	e.WriteString("xyz")
	if got := row(e, 0); got != "abz" {
		t.Fatalf("without autowrap %q", got)
	}
	if got := row(e, 1); got != "" {
		t.Fatalf("second row %q", got)
	}
}

func TestWideAndCombining(t *testing.T) {
	e, _ := emu(4, 2)
	e.WriteString("a日本")
	if got := row(e, 0); got != "a日" {
		t.Fatalf("wide at the edge %q", got)
	}
	if got := row(e, 1); got != "本" {
		t.Fatalf("wide wrapped %q", got)
	}
	e, _ = emu(6, 1)
	e.WriteString("éx")
	buf := uv.NewBuffer(6, 1)
	e.Draw(buf, View{})
	if buf.Lines[0][0].Content != "é" || buf.Lines[0][1].Content != "x" {
		t.Fatalf("combining mark not joined: %q %q", buf.Lines[0][0].Content, buf.Lines[0][1].Content)
	}
	e, _ = emu(6, 1)
	e.WriteString("\U0001F469‍\U0001F4BB!")
	e.Draw(buf, View{})
	if buf.Lines[0][0].Content != "\U0001F469‍\U0001F4BB" || buf.Lines[0][0].Width != 2 || buf.Lines[0][2].Content != "!" {
		t.Fatalf("zwj sequence: %q width %d then %q", buf.Lines[0][0].Content, buf.Lines[0][0].Width, buf.Lines[0][2].Content)
	}
}

func TestCursorMovesAndRegions(t *testing.T) {
	e, _ := emu(10, 5)
	e.WriteString("\x1b[2;4r")
	cursorAt(t, e, 0, 0)
	e.WriteString("\x1b[4;1H")
	cursorAt(t, e, 0, 3)
	e.WriteString("a\r\nb\r\nc")
	if got := rows(e); got[0] != "" || got[1] != "a" || got[2] != "b" || got[3] != "c" || got[4] != "" {
		t.Fatalf("region scroll %q", got)
	}
	if e.ScrollbackLen() != 0 {
		t.Fatal("regions below the top row must not feed the scrollback")
	}
	e.WriteString("\x1b[?6h\x1b[1;1H")
	cursorAt(t, e, 0, 1)
	e.WriteString("\x1b[6n")
}

func TestEraseInsertDelete(t *testing.T) {
	e, _ := emu(8, 3)
	e.WriteString("abcdefgh\x1b[1;3H\x1b[2P")
	if got := row(e, 0); got != "abefgh" {
		t.Fatalf("DCH %q", got)
	}
	e.WriteString("\x1b[2@")
	if got := row(e, 0); got != "ab  efgh" {
		t.Fatalf("ICH %q", got)
	}
	e.WriteString("\x1b[3X")
	if got := row(e, 0); got != "ab   fgh" {
		t.Fatalf("ECH %q", got)
	}
	e.WriteString("\x1b[1K")
	if got := row(e, 0); got != "     fgh" {
		t.Fatalf("EL 1 %q", got)
	}
	e.WriteString("\x1b[2;1Hrow2\x1b[3;1Hrow3\x1b[2;1H\x1b[L")
	if got := rows(e); got[1] != "" || got[2] != "row2" {
		t.Fatalf("IL %q", got)
	}
	e.WriteString("\x1b[M")
	if got := rows(e); got[1] != "row2" || got[2] != "" {
		t.Fatalf("DL %q", got)
	}
	e.WriteString("\x1b[2J")
	if got := rows(e); got[0] != "" || got[1] != "" {
		t.Fatalf("ED 2 %q", got)
	}
}

func TestEraseKeepsBackground(t *testing.T) {
	e, _ := emu(4, 1)
	e.WriteString("\x1b[44m\x1b[2K")
	buf := uv.NewBuffer(4, 1)
	e.Draw(buf, View{})
	if c := buf.Lines[0][3]; c.Style.Bg != ansi.BasicColor(4) {
		t.Fatalf("erase without the pen background: %#v", c.Style.Bg)
	}
}

func TestSGR(t *testing.T) {
	e, _ := emu(10, 1)
	e.WriteString("\x1b[1;3;4:3;38;5;200;48;2;1;2;3;58:2::9:8:7mx")
	buf := uv.NewBuffer(10, 1)
	e.Draw(buf, View{})
	st := buf.Lines[0][0].Style
	if st.Attrs&uv.AttrBold == 0 || st.Attrs&uv.AttrItalic == 0 || st.Underline != uv.UnderlineCurly {
		t.Fatalf("attrs %#v", st)
	}
	if st.Fg != ansi.IndexedColor(200) {
		t.Fatalf("fg %#v", st.Fg)
	}
	if bg, ok := st.Bg.(color.RGBA); !ok || bg != (color.RGBA{1, 2, 3, 0xff}) {
		t.Fatalf("bg %#v", st.Bg)
	}
	if ul, ok := st.UnderlineColor.(color.RGBA); !ok || ul != (color.RGBA{9, 8, 7, 0xff}) {
		t.Fatalf("underline color %#v", st.UnderlineColor)
	}
	e.WriteString("\x1b[0my")
	e.Draw(buf, View{})
	if !buf.Lines[0][1].Style.IsZero() {
		t.Fatalf("reset left %#v", buf.Lines[0][1].Style)
	}
}

func TestThemeColorsAndReverse(t *testing.T) {
	e, _ := emu(3, 1)
	e.WriteString("\x1b[31mr")
	var v View
	v.Colors[1] = color.RGBA{9, 9, 9, 0xff}
	buf := uv.NewBuffer(3, 1)
	e.Draw(buf, v)
	if buf.Lines[0][0].Style.Fg != (color.RGBA{9, 9, 9, 0xff}) {
		t.Fatalf("theme color not applied: %#v", buf.Lines[0][0].Style.Fg)
	}
	e.WriteString("\x1b]4;1;#010203\x1b\\")
	e.Draw(buf, v)
	if buf.Lines[0][0].Style.Fg != (color.RGBA{1, 2, 3, 0xff}) {
		t.Fatalf("OSC 4 override not applied: %#v", buf.Lines[0][0].Style.Fg)
	}
	e.WriteString("\x1b[?5h")
	v.Fg, v.Bg = color.White, color.Black
	e.Draw(buf, v)
	if buf.Lines[0][1].Style.Fg != color.Black || buf.Lines[0][1].Style.Bg != color.White {
		t.Fatalf("reverse video %#v", buf.Lines[0][1].Style)
	}
}

func TestAltScreenSavesCursor(t *testing.T) {
	e, _ := emu(10, 3)
	e.WriteString("main\x1b[?1049h")
	if !e.AltScreen() || row(e, 0) != "" {
		t.Fatalf("alt screen not clear: %q", rows(e))
	}
	e.WriteString("alt\x1b[?1049l")
	if e.AltScreen() || row(e, 0) != "main" {
		t.Fatalf("main screen lost: %q", rows(e))
	}
	cursorAt(t, e, 4, 0)
}

func TestReplies(t *testing.T) {
	e, out := emu(20, 5)
	e.WriteString("\x1b[c\x1b[>c\x1b[5n\x1b[3;4H\x1b[6n\x1b[?2004h\x1b[?2004$p\x1b[?1u\x1b[>1u\x1b[?u\x1b[18t")
	want := primaryDA + secondaryDA + "\x1b[0n\x1b[3;4R\x1b[?2004;1$y\x1b[?0u\x1b[?1u\x1b[8;5;20t"
	if out.String() != want {
		t.Fatalf("replies\n got %q\nwant %q", out.String(), want)
	}
	out.Reset()
	e.WriteString("\x1bP+q544e;6e616d65\x1b\\")
	if got := out.String(); got != "\x1bP1+r544e="+hexOf("xterm-256color")+"\x1b\\\x1bP1+r6e616d65="+hexOf("xterm-256color")+"\x1b\\" {
		t.Fatalf("termcap %q", got)
	}
	out.Reset()
	e.WriteString("\x1b[1;31m\x1bP$qm\x1b\\")
	if got := out.String(); got != "\x1bP1$r1;31m\x1b\\" {
		t.Fatalf("DECRQSS %q", got)
	}
	out.Reset()
	e.WriteString("\x1b]11;?\x1b\\")
	if got := out.String(); got != "\x1b]11;rgb:0000/0000/0000\x1b\\" {
		t.Fatalf("OSC 11 query %q", got)
	}
}

func TestOSC(t *testing.T) {
	e, _ := emu(20, 3)
	e.WriteString("\x1b]2;hello\x07\x1b]7;file://host/tmp/x%20y\x1b\\\x1b]8;;https://a.b\x1b\\link\x1b]8;;\x1b\\\x1b]52;c;aGk=\x1b\\\x1b]133;A\x1b\\$ \x1b]133;D;3\x1b\\")
	if e.Title() != "hello" || e.Cwd() != "/tmp/x y" {
		t.Fatalf("title %q cwd %q", e.Title(), e.Cwd())
	}
	buf := uv.NewBuffer(20, 3)
	e.Draw(buf, View{})
	if buf.Lines[0][0].Link.URL != "https://a.b" {
		t.Fatalf("hyperlink %#v", buf.Lines[0][0].Link)
	}
	var clip string
	for _, ev := range e.Events() {
		if ev.Kind == EventClipboard {
			clip = ev.Text
		}
	}
	if clip != "hi" {
		t.Fatalf("clipboard %q", clip)
	}
	l, _ := e.Row(e.Base())
	if l.Marks&MarkPrompt == 0 || l.Marks&MarkEnd == 0 || l.Exit != 3 {
		t.Fatalf("marks %b exit %d", l.Marks, l.Exit)
	}
}

func TestReflowRoundTrip(t *testing.T) {
	e, _ := emu(10, 3)
	e.WriteString("0123456789abcdef\r\nshort\r\n")
	e.WriteString("end")
	e.Resize(5, 3)
	var got []string
	for r := e.Base() - uint64(e.ScrollbackLen()); ; r++ {
		l, ok := e.Row(r)
		if !ok {
			break
		}
		got = append(got, strings.TrimRight(uv.Line(l.Cells).String(), " "))
	}
	joined := strings.Join(got, "|")
	if joined != "01234|56789|abcde|f|short|end" {
		t.Fatalf("narrow %q", joined)
	}
	cursorAt(t, e, 3, 2)
	e.Resize(20, 3)
	if got := rows(e); got[0] != "0123456789abcdef" || got[1] != "short" || got[2] != "end" {
		t.Fatalf("wide again %q", got)
	}
	cursorAt(t, e, 3, 2)
}

func TestHeightChangeMovesRows(t *testing.T) {
	e, _ := emu(5, 4)
	e.WriteString("a\r\nb\r\nc")
	e.Resize(5, 2)
	if got := rows(e); got[0] != "b" || got[1] != "c" {
		t.Fatalf("shrunk %q", got)
	}
	if e.ScrollbackLen() != 1 {
		t.Fatalf("scrollback %d", e.ScrollbackLen())
	}
	cursorAt(t, e, 1, 1)
	e.Resize(5, 4)
	if got := rows(e); got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("grown %q", got)
	}
	cursorAt(t, e, 1, 2)
}

func TestSelectionText(t *testing.T) {
	e, _ := emu(5, 4)
	e.WriteString("hello world\r\nfoo  ")
	base := e.Base()
	text := e.Text(Selection{Anchor: Pos{base, 0}, Head: Pos{base + 3, 2}})
	if text != "hello world\nfoo" {
		t.Fatalf("text %q", text)
	}
	from, to := e.WordAt(Pos{base + 1, 1})
	if from != (Pos{base + 1, 1}) || to != (Pos{base + 2, 0}) {
		t.Fatalf("word %v %v", from, to)
	}
	from, to = e.LineAt(Pos{base, 3})
	if from != (Pos{base, 0}) || to != (Pos{base + 2, 4}) {
		t.Fatalf("line %v %v", from, to)
	}
	block := e.Text(Selection{Anchor: Pos{base, 1}, Head: Pos{base + 3, 2}, Mode: SelBlock})
	if block != "el\nwo\n\noo" {
		t.Fatalf("block %q", block)
	}
}

func TestFind(t *testing.T) {
	e, _ := emu(5, 3)
	e.WriteString("abcabcab\r\nABC")
	base := e.Base()
	got := e.Find("abc")
	want := []Match{{base, 0, 2}, {base, 3, 4}, {base + 1, 0, 0}, {base + 2, 0, 2}}
	if len(got) != len(want) {
		t.Fatalf("matches %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("match %d %v want %v", i, got[i], want[i])
		}
	}
	if n := len(e.Find("ABC")); n != 1 {
		t.Fatalf("case sensitive matches %d", n)
	}
}

func TestLegacyKeys(t *testing.T) {
	st := keyState{}
	cases := []struct {
		k    Key
		want string
	}{
		{Key{Code: 'a', Text: "a"}, "a"},
		{Key{Code: 'a', Mod: uv.ModCtrl}, "\x01"},
		{Key{Code: 'a', Mod: uv.ModAlt, Text: "a"}, "\x1ba"},
		{Key{Code: uv.KeyUp}, "\x1b[A"},
		{Key{Code: uv.KeyUp, Mod: uv.ModShift}, "\x1b[1;2A"},
		{Key{Code: uv.KeyF5}, "\x1b[15~"},
		{Key{Code: uv.KeyF1, Mod: uv.ModCtrl}, "\x1b[1;5P"},
		{Key{Code: uv.KeyTab, Mod: uv.ModShift}, "\x1b[Z"},
		{Key{Code: uv.KeyBackspace}, "\x7f"},
		{Key{Code: uv.KeyBackspace, Mod: uv.ModCtrl}, "\x08"},
		{Key{Code: uv.KeyEnter}, "\r"},
		{Key{Code: uv.KeyEscape}, "\x1b"},
		{Key{Code: uv.KeySpace, Mod: uv.ModCtrl}, "\x00"},
		{Key{Code: uv.KeyDelete}, "\x1b[3~"},
	}
	for _, c := range cases {
		if got := string(encodeKey(c.k, st)); got != c.want {
			t.Errorf("%+v: %q want %q", c.k, got, c.want)
		}
	}
	app := keyState{cursorKeys: true, keypadApp: true}
	if got := string(encodeKey(Key{Code: uv.KeyUp}, app)); got != "\x1bOA" {
		t.Errorf("application cursor %q", got)
	}
	if got := string(encodeKey(Key{Code: uv.KeyKp1}, app)); got != "\x1bOq" {
		t.Errorf("application keypad %q", got)
	}
	other := keyState{modify: 2}
	if got := string(encodeKey(Key{Code: 'a', Mod: uv.ModCtrl}, other)); got != "\x1b[27;5;97~" {
		t.Errorf("modifyOtherKeys %q", got)
	}
}

func TestKittyKeys(t *testing.T) {
	dis := keyState{kitty: ansi.KittyDisambiguateEscapeCodes}
	cases := []struct {
		st   keyState
		k    Key
		want string
	}{
		{dis, Key{Code: 'a', Text: "a"}, "a"},
		{dis, Key{Code: uv.KeyEscape}, "\x1b[27u"},
		{dis, Key{Code: 'a', Mod: uv.ModCtrl}, "\x1b[97;5u"},
		{dis, Key{Code: uv.KeyEnter}, "\r"},
		{dis, Key{Code: uv.KeyEnter, Mod: uv.ModShift}, "\x1b[13;2u"},
		{dis, Key{Code: uv.KeyUp, Mod: uv.ModAlt}, "\x1b[1;3A"},
		{dis, Key{Code: 'a', Release: true}, ""},
		{keyState{kitty: ansi.KittyDisambiguateEscapeCodes | ansi.KittyReportEventTypes}, Key{Code: 'a', Release: true}, "\x1b[97;1:3u"},
		{keyState{kitty: ansi.KittyDisambiguateEscapeCodes | ansi.KittyReportEventTypes}, Key{Code: uv.KeyLeft, Repeat: true}, "\x1b[1;1:2D"},
		{keyState{kitty: ansi.KittyAllFlags}, Key{Code: 'a', Text: "a"}, "\x1b[97;;97u"},
		{keyState{kitty: ansi.KittyAllFlags}, Key{Code: 'a', Shifted: 'A', Text: "A", Mod: uv.ModShift}, "\x1b[97:65;2;65u"},
		{keyState{kitty: ansi.KittyAllFlags}, Key{Code: uv.KeyLeftShift}, "\x1b[57441u"},
		{dis, Key{Code: uv.KeyF3}, "\x1b[13~"},
	}
	for _, c := range cases {
		if got := string(encodeKey(c.k, c.st)); got != c.want {
			t.Errorf("%+v under %d: %q want %q", c.k, c.st.kitty, got, c.want)
		}
	}
}

func TestPasteAndFocus(t *testing.T) {
	e, _ := emu(5, 1)
	if got := string(e.Paste("a\nb\x1bc")); got != "a\rbc" {
		t.Fatalf("plain paste %q", got)
	}
	e.WriteString("\x1b[?2004h\x1b[?1004h")
	if got := string(e.Paste("a\n\x1b[201~b")); got != "\x1b[200~a\nb\x1b[201~" {
		t.Fatalf("bracketed paste %q", got)
	}
	if got := string(e.Focus(true)); got != "\x1b[I" {
		t.Fatalf("focus %q", got)
	}
}

func TestMouse(t *testing.T) {
	e, _ := emu(10, 5)
	if _, ok := e.Mouse(Mouse{X: 1, Y: 1, Button: uv.MouseLeft}); ok {
		t.Fatal("untracked mouse must stay with the host")
	}
	e.WriteString("\x1b[?1002h\x1b[?1006h")
	cases := []struct {
		m    Mouse
		want string
		ok   bool
	}{
		{Mouse{X: 2, Y: 3, Button: uv.MouseLeft, Kind: MousePress}, "\x1b[<0;3;4M", true},
		{Mouse{X: 2, Y: 3, Button: uv.MouseLeft, Kind: MouseMotion}, "\x1b[<32;3;4M", true},
		{Mouse{X: 2, Y: 3, Button: uv.MouseLeft, Kind: MouseRelease}, "\x1b[<0;3;4m", true},
		{Mouse{X: 2, Y: 3, Button: uv.MouseNone, Kind: MouseMotion}, "", false},
		{Mouse{X: 2, Y: 3, Button: uv.MouseWheelUp, Kind: MouseWheel, Mod: uv.ModCtrl}, "\x1b[<80;3;4M", true},
		{Mouse{X: 2, Y: 3, Button: uv.MouseLeft, Kind: MousePress, Mod: uv.ModShift}, "", false},
	}
	for _, c := range cases {
		got, ok := e.Mouse(c.m)
		if ok != c.ok || string(got) != c.want {
			t.Errorf("%+v: %q %v want %q %v", c.m, got, ok, c.want, c.ok)
		}
	}
	e.WriteString("\x1b[?1006l")
	got, _ := e.Mouse(Mouse{X: 2, Y: 3, Button: uv.MouseRight, Kind: MousePress})
	if string(got) != "\x1b[M\x22\x23\x24" {
		t.Fatalf("x10 encoding %q", got)
	}
}

func TestInsertModeAndRepeat(t *testing.T) {
	e, _ := emu(6, 1)
	e.WriteString("abc\x1b[1;1H\x1b[4hxy\x1b[4l\x1b[2b")
	if got := row(e, 0); got != "xyyyc" {
		t.Fatalf("insert then repeat %q", got)
	}
}

func TestTabsAndCharset(t *testing.T) {
	e, _ := emu(20, 1)
	e.WriteString("\x1b[3g\x1b[1;5H\x1bH\x1b[1;1Ha\tb")
	if got := row(e, 0); got != "a   b" {
		t.Fatalf("custom tab stop %q", got)
	}
	e.WriteString("\r\x1b[2K\x1b(0lqk\x1b(B|")
	if got := row(e, 0); got != "┌─┐|" {
		t.Fatalf("graphics charset %q", got)
	}
}

func TestSoftResetAndRIS(t *testing.T) {
	e, _ := emu(10, 3)
	e.WriteString("\x1b[2;3r\x1b[?6h\x1b[?25l\x1b[!p")
	if e.scr.top != 0 || e.scr.bot != 2 || e.scr.cur.origin || !e.modes.showCursor {
		t.Fatal("soft reset left state behind")
	}
	e.WriteString("text\x1bc")
	if row(e, 0) != "" || e.Title() != "" {
		t.Fatal("full reset left the screen")
	}
}

func TestScrollbackCap(t *testing.T) {
	e := New(4, 2, Options{Scrollback: 3})
	for i := 0; i < 10; i++ {
		e.WriteString("x\r\n")
	}
	if e.ScrollbackLen() != 3 || e.Scrolled() != 9 || e.Base() != 9 {
		t.Fatalf("len %d scrolled %d base %d", e.ScrollbackLen(), e.Scrolled(), e.Base())
	}
	if _, ok := e.Row(5); ok {
		t.Fatal("dropped rows must be gone")
	}
	if _, ok := e.Row(6); !ok {
		t.Fatal("kept rows must be reachable")
	}
}

func TestSynchronizedUpdate(t *testing.T) {
	e, _ := emu(4, 1)
	e.WriteString("\x1b[?2026h")
	if !e.Synchronized() {
		t.Fatal("sync not set")
	}
	e.WriteString("\x1b[?2026l")
	if e.Synchronized() {
		t.Fatal("sync not cleared")
	}
}

func TestMargins(t *testing.T) {
	e, _ := emu(8, 2)
	e.WriteString("abcdefgh\x1b[?69h\x1b[3;6s\x1b[1;3H\x1b[2'~")
	if got := row(e, 0); got != "abef  gh" {
		t.Fatalf("delete columns inside margins %q", got)
	}
}

func TestReflowKeepsWideCharacters(t *testing.T) {
	e, _ := emu(6, 2)
	e.WriteString("日本語")
	e.Resize(3, 2)
	if got := rows(e); got[0] != "本" || got[1] != "語" || e.ScrollbackLen() != 1 {
		t.Fatalf("narrow %q scrollback %d", got, e.ScrollbackLen())
	}
	cursorAt(t, e, 2, 1)
	e.Resize(8, 2)
	if got := row(e, 0); got != "日本語" {
		t.Fatalf("wide again %q", got)
	}
	cursorAt(t, e, 6, 0)
}

func TestTitleStackAndSingleShift(t *testing.T) {
	e, out := emu(10, 2)
	e.WriteString("\x1b]2;one\x1b\\\x1b[22;2t\x1b]2;two\x1b\\")
	if e.Title() != "two" {
		t.Fatalf("title %q", e.Title())
	}
	e.WriteString("\x1b[23;2t\x1b[21t")
	if e.Title() != "one" || out.String() != "\x1b]lone\x1b\\" {
		t.Fatalf("popped title %q answer %q", e.Title(), out.String())
	}
	e.WriteString("\x1b*0\x1bNqq")
	if got := row(e, 0); got != "─q" {
		t.Fatalf("single shift %q", got)
	}
}

func TestInsertColumnsAndOriginMargins(t *testing.T) {
	e, _ := emu(8, 2)
	e.WriteString("abcdefgh\r\n12345678\x1b[?69h\x1b[2;7s\x1b[1;2H\x1b[2'}")
	if got := rows(e); got[0] != "a  bcdeh" || got[1] != "1  23458" {
		t.Fatalf("insert columns %q", got)
	}
	e.WriteString("\x1b[?6h\x1b[1;1Hx")
	if got := row(e, 0); got != "ax bcdeh" {
		t.Fatalf("origin inside margins %q", got)
	}
}

func TestScrollbackDrawAndClear(t *testing.T) {
	e, _ := emu(4, 2)
	for i := 0; i < 5; i++ {
		e.WriteString(string(rune('a'+i)) + "\r\n")
	}
	buf := uv.NewBuffer(4, 2)
	e.Draw(buf, View{Scroll: 2})
	if got := strings.TrimSpace(uv.Line(buf.Lines[0]).String()); got != "c" {
		t.Fatalf("scrolled view %q", got)
	}
	e.WriteString("\x1b[3J")
	if e.ScrollbackLen() != 0 {
		t.Fatal("ED 3 left the scrollback")
	}
}
