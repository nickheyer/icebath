package gfx

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi/kitty"
)

// Environment lookup over pairs of names and values
func envOf(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

// Clock a test moves by hand
type clock struct{ now time.Time }

func (c *clock) add(d time.Duration) time.Time {
	c.now = c.now.Add(d)
	return c.now
}

// Terminal of the environment with its answers stamped by the test clock and shared memory off
func fake(kv ...string) (*Terminal, *clock) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	t := NewWith(envOf(kv...))
	t.clock = func() time.Time { return c.now }
	t.shm = false
	return t, c
}

// Text a command writes to the terminal, empty for nil
func rawText(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	raw, ok := cmd().(tea.RawMsg)
	if !ok {
		t.Fatalf("command gives %T", cmd())
	}
	return raw.Msg.(string)
}

// Frame of w by h pixels ramping red across and green down
func ramp(w, h int) []byte {
	out := make([]byte, w*h*3)
	for y := range h {
		for x := range w {
			i := (y*w + x) * 3
			out[i], out[i+1], out[i+2] = byte(x*255/max(w-1, 1)), byte(y*255/max(h-1, 1)), 40
		}
	}
	return out
}

// Frame of w by h pixels of a lit scene with grain, as a rendered floor compresses
func grainy(w, h int) []byte {
	out := make([]byte, w*h*3)
	seed := uint32(7)
	for y := range h {
		for x := range w {
			i := (y*w + x) * 3
			seed = seed*1664525 + 1013904223
			n := byte(seed >> 28)
			out[i], out[i+1], out[i+2] = byte(x*200/max(w-1, 1))+n, byte(y*160/max(h-1, 1))+n, 40+n
		}
	}
	return out
}

// Kitty graphics command with its control keys and payload
type gcmd struct {
	keys    map[string]string
	payload string
}

// Removes tmux passthrough wrapping, undoubling the escapes inside it
func untmux(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "\x1bPtmux;")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i+len("\x1bPtmux;"):]
		for len(s) > 0 {
			if s[0] == 0x1b && len(s) > 1 && s[1] == 0x1b {
				b.WriteByte(0x1b)
				s = s[2:]
				continue
			}
			if s[0] == 0x1b && len(s) > 1 && s[1] == '\\' {
				s = s[2:]
				break
			}
			b.WriteByte(s[0])
			s = s[1:]
		}
	}
}

// Every kitty graphics command in the text in order
func gcmds(out string) []gcmd {
	var cmds []gcmd
	for {
		i := strings.Index(out, "\x1b_G")
		if i < 0 {
			return cmds
		}
		j := strings.Index(out[i:], "\x1b\\")
		body := out[i+3 : i+j]
		ctrl, payload, _ := strings.Cut(body, ";")
		keys := map[string]string{}
		for _, kv := range strings.Split(ctrl, ",") {
			k, v, _ := strings.Cut(kv, "=")
			keys[k] = v
		}
		cmds = append(cmds, gcmd{keys, payload})
		out = out[i+j+2:]
	}
}

// Pixels a direct kitty transmission carries, checking the chunk keys as konsole and kitty read them
func directPixels(t *testing.T, cmds []gcmd) []byte {
	t.Helper()
	var payload strings.Builder
	for i, c := range cmds {
		if i == 0 {
			if c.keys["a"] != "T" || c.keys["t"] != "d" || c.keys["o"] != "z" || c.keys["q"] != "0" {
				t.Fatalf("first chunk keys %v", c.keys)
			}
			if len(cmds) > 1 && c.keys["m"] != "1" {
				t.Fatalf("first of %d chunks lacks m=1, keys %v", len(cmds), c.keys)
			}
		} else {
			want := "1"
			if i == len(cmds)-1 {
				want = "0"
			}
			if len(c.keys) != 1 || c.keys["m"] != want {
				t.Fatalf("chunk %d of %d keys %v", i, len(cmds), c.keys)
			}
		}
		if len(c.payload) > 4096 || (i < len(cmds)-1 && len(c.payload)%4 != 0) {
			t.Fatalf("chunk %d carries %d base64 bytes", i, len(c.payload))
		}
		payload.WriteString(c.payload)
	}
	z, err := base64.StdEncoding.DecodeString(payload.String())
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	px, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return px
}

// Transmission commands of an emit, leaving out deletes
func transmissions(cmds []gcmd) []gcmd {
	var out []gcmd
	for _, c := range cmds {
		if c.keys["a"] != "d" {
			out = append(out, c)
		}
	}
	return out
}

// Kitty answer to an image id
func answer(id uint32, payload string) uv.KittyGraphicsEvent {
	return uv.KittyGraphicsEvent{Options: kitty.Options{ID: int(id)}, Payload: []byte(payload)}
}

// Image id a transmission went to
func idOf(t *testing.T, cmds []gcmd) uint32 {
	t.Helper()
	tx := transmissions(cmds)
	if len(tx) == 0 {
		t.Fatal("no transmission")
	}
	id, err := strconv.Atoi(tx[0].keys["i"])
	if err != nil {
		t.Fatal(err)
	}
	return uint32(id)
}

// Every terminal is named by its environment, with leaked variables of a parent terminal losing to fresher ones
func TestIdentity(t *testing.T) {
	cases := []struct {
		env  []string
		want string
	}{
		{[]string{"KITTY_WINDOW_ID", "1", "TERM", "xterm-kitty"}, "kitty"},
		{[]string{"TERM", "xterm-kitty"}, "kitty"},
		{[]string{"KITTY_WINDOW_ID", "4", "TERM", "xterm-256color"}, "kitty"},
		{[]string{"GHOSTTY_RESOURCES_DIR", "/usr/share/ghostty", "TERM", "xterm-256color"}, "ghostty"},
		{[]string{"TERM", "xterm-ghostty"}, "ghostty"},
		{[]string{"TERM_PROGRAM", "ghostty"}, "ghostty"},
		{[]string{"TERM_PROGRAM", "WezTerm", "TERM", "xterm-256color"}, "wezterm"},
		{[]string{"TERM_PROGRAM", "wezterm"}, "wezterm"},
		{[]string{"KONSOLE_VERSION", "260801", "TERM", "xterm-256color"}, "konsole"},
		{[]string{"TERM_PROGRAM", "rio"}, "rio"},
		{[]string{"TERM_PROGRAM", "vscode", "TERM", "xterm-256color"}, "vscode"},
		{[]string{"TERM_PROGRAM", "iTerm.app"}, "iterm"},
		{[]string{"LC_TERMINAL", "iTerm2", "TERM", "xterm-256color"}, "iterm"},
		{[]string{"TERM_PROGRAM", "mintty"}, "mintty"},
		{[]string{"TERM_PROGRAM", "Hyper"}, "hyper"},
		{[]string{"TERM", "foot"}, "foot"},
		{[]string{"TERM", "foot-extra"}, "foot"},
		{[]string{"XTERM_VERSION", "XTerm(411)", "TERM", "xterm"}, "xterm"},
		{[]string{"TERM", "mlterm"}, "mlterm"},
		{[]string{"TERM", "alacritty"}, "alacritty"},
		{[]string{"ALACRITTY_WINDOW_ID", "9", "TERM", "xterm-256color"}, "alacritty"},
		{[]string{"TERM", "linux"}, "linux"},
		{[]string{"TERM", "dumb"}, "dumb"},
		{[]string{"TERM", "St-256color"}, "st-256color"},
		{[]string{"TERM_PROGRAM", "vscode", "KONSOLE_VERSION", "260801", "TERM", "xterm-256color"}, "konsole"},
		{[]string{"TERM_PROGRAM", "vscode", "XTERM_VERSION", "XTerm(411)", "TERM", "xterm"}, "xterm"},
		{[]string{"KITTY_WINDOW_ID", "2", "KONSOLE_VERSION", "260801", "TERM", "xterm-256color"}, "konsole"},
		{[]string{"KITTY_WINDOW_ID", "2", "TERM_PROGRAM", "WezTerm", "TERM", "xterm-256color"}, "wezterm"},
		{[]string{"GHOSTTY_RESOURCES_DIR", "/g", "TERM_PROGRAM", "WezTerm"}, "wezterm"},
		{[]string{"KONSOLE_VERSION", "260801", "TERM", "xterm-kitty"}, "kitty"},
		{[]string{"KITTY_WINDOW_ID", "1", "TMUX", "/tmp/tmux-1000/default,1,0", "TERM", "tmux-256color", "TERM_PROGRAM", "tmux"}, "kitty"},
	}
	for _, c := range cases {
		term, _ := fake(c.env...)
		if term.Program() != c.want {
			t.Errorf("%v named %q, want %q", c.env, term.Program(), c.want)
		}
	}
}

// Shared memory follows identity, refused under tmux and over ssh
func TestSharedByIdentity(t *testing.T) {
	cases := []struct {
		env  []string
		want bool
	}{
		{[]string{"TERM", "xterm-kitty"}, shmWorks()},
		{[]string{"TERM", "xterm-ghostty"}, shmWorks()},
		{[]string{"KONSOLE_VERSION", "1"}, false},
		{[]string{"TERM_PROGRAM", "WezTerm"}, false},
		{[]string{"TERM", "xterm-kitty", "SSH_CONNECTION", "1 2 3 4"}, false},
		{[]string{"TERM", "xterm-kitty", "SSH_TTY", "/dev/pts/3"}, false},
		{[]string{"TERM", "xterm-kitty", "SSH_CLIENT", "1 2 3"}, false},
		{[]string{"KITTY_WINDOW_ID", "1", "TMUX", "x"}, false},
	}
	for _, c := range cases {
		if got := NewWith(envOf(c.env...)).shared(); got != c.want {
			t.Errorf("%v shares memory %v, want %v", c.env, got, c.want)
		}
	}
}

// Each identity is asked what its traits allow, device attributes last, nothing through tmux
func TestProbeText(t *testing.T) {
	const da1 = "\x1b[c"
	cases := []struct {
		env  []string
		want string
	}{
		{[]string{"TERM", "xterm-kitty"}, kittyQuery + sizeQueries + da1},
		{[]string{"TERM", "xterm-ghostty"}, kittyQuery + sizeQueries + da1},
		{[]string{"TERM_PROGRAM", "WezTerm"}, kittyQuery + sizeQueries + sixelQueries + da1},
		{[]string{"KONSOLE_VERSION", "260801"}, kittyQuery + sizeQueries + sixelQueries + da1},
		{[]string{"TERM_PROGRAM", "rio"}, kittyQuery + sizeQueries + da1},
		{[]string{"TERM_PROGRAM", "vscode"}, sizeQueries + sixelQueries + da1},
		{[]string{"TERM_PROGRAM", "iTerm.app"}, sizeQueries + da1},
		{[]string{"TERM_PROGRAM", "mintty"}, sizeQueries + da1},
		{[]string{"TERM_PROGRAM", "Hyper"}, sizeQueries + da1},
		{[]string{"TERM", "foot"}, sizeQueries + sixelQueries + da1},
		{[]string{"XTERM_VERSION", "XTerm(411)"}, sizeQueries + sixelQueries + da1},
		{[]string{"TERM", "mlterm"}, sizeQueries + sixelQueries + da1},
		{[]string{"TERM", "st-256color"}, sizeQueries + da1},
		{[]string{"TERM", "alacritty"}, sizeQueries},
		{[]string{"TERM", "linux"}, sizeQueries},
		{[]string{"TERM", "dumb"}, sizeQueries},
		{[]string{"TERM", "xterm-kitty", "TMUX", "x"}, ""},
		{[]string{"KONSOLE_VERSION", "1", "TMUX", "x"}, ""},
	}
	for _, c := range cases {
		term, _ := fake(c.env...)
		if got := rawText(t, term.Probe()); got != c.want {
			t.Errorf("%v probes %q, want %q", c.env, got, c.want)
		}
		want := sizeQueries
		if term.Tmux() {
			want = ""
		}
		if got := rawText(t, term.Measure()); got != want {
			t.Errorf("%v measures %q, want %q", c.env, got, want)
		}
	}
	if kittyQuery != "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\" || sizeQueries != "\x1b[16t\x1b[14t" || sixelQueries != "\x1b[?1;1;0S\x1b[?2;1;0S" {
		t.Fatal("query text changed")
	}
}

// Decodes terminal bytes as the program reads them
func decode(t *testing.T, seq string) uv.Event {
	t.Helper()
	var d uv.EventDecoder
	n, ev := d.Decode([]byte(seq))
	if n != len(seq) {
		t.Fatalf("decoded %d of %d bytes of %q", n, len(seq), seq)
	}
	return ev
}

// An OK answer to the query verifies kitty graphics, placeholders by identity choosing the way
func TestKittyAnswerVerifies(t *testing.T) {
	kon, _ := fake("KONSOLE_VERSION", "260801")
	if kon.Protocol() != None || kon.Guess() != None {
		t.Fatalf("konsole offers %s before answering", kon.Protocol())
	}
	if kon.Update(decode(t, "\x1b_Gi=31;EINVAL:bad\x1b\\")) || kon.Protocol() != None {
		t.Fatal("an error answer verified kitty graphics")
	}
	if !kon.Update(decode(t, "\x1b_Gi=31;OK\x1b\\")) || kon.Protocol() != KittyPlaced || kon.Guess() != KittyPlaced {
		t.Fatalf("konsole draws %s after the OK", kon.Protocol())
	}
	if kon.Update(decode(t, "\x1b_Gi=31;OK\x1b\\")) {
		t.Fatal("a second OK reported a change")
	}
	kit, _ := fake("TERM", "xterm-kitty")
	kit.Update(answer(queryID, "OK"))
	if kit.Protocol() != KittyCells {
		t.Fatalf("kitty draws %s", kit.Protocol())
	}
	wez, _ := fake("TERM_PROGRAM", "WezTerm")
	if wez.Guess() != ITerm {
		t.Fatalf("wezterm guesses %s before answering", wez.Guess())
	}
	wez.Update(answer(queryID, "OK"))
	if wez.Protocol() != KittyPlaced || wez.Guess() != KittyPlaced {
		t.Fatalf("wezterm draws %s", wez.Protocol())
	}
}

// Sixel needs device attributes listing it and a measured cell, iTerm2 is verified by identity alone
func TestSixelAndITermProtocols(t *testing.T) {
	xt, _ := fake("XTERM_VERSION", "XTerm(411)")
	xt.Update(decode(t, "\x1b[?63;1;2;4;6;9;15;22c"))
	if xt.Protocol() != None || xt.Measured() {
		t.Fatal("sixel offered before the cell size is known")
	}
	if !xt.Update(decode(t, "\x1b[6;20;10t")) || xt.Protocol() != Sixel || !xt.Measured() {
		t.Fatalf("xterm draws %s after the cell report", xt.Protocol())
	}
	if w, h := xt.Cell(); w != 10 || h != 20 {
		t.Fatalf("cell %dx%d", w, h)
	}
	vs, _ := fake("TERM_PROGRAM", "vscode")
	if vs.Protocol() != None || vs.Guess() != ITerm {
		t.Fatalf("vscode draws %s and guesses %s", vs.Protocol(), vs.Guess())
	}
	vs.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	vs.Update(uv.PixelSizeEvent{Width: 900, Height: 1000})
	vs.Update(uv.PrimaryDeviceAttributesEvent{62, 4})
	if vs.Protocol() != Sixel {
		t.Fatalf("vscode with sixel draws %s", vs.Protocol())
	}
	if w, h := vs.Cell(); w != 9 || h != 20 {
		t.Fatalf("cell from window pixels %dx%d", w, h)
	}
	it, _ := fake("TERM_PROGRAM", "iTerm.app")
	if it.Protocol() != ITerm {
		t.Fatalf("iterm draws %s", it.Protocol())
	}
	for _, env := range [][]string{{"TERM_PROGRAM", "mintty"}, {"TERM_PROGRAM", "Hyper"}} {
		m, _ := fake(env...)
		if m.Protocol() != None || m.Guess() != ITerm {
			t.Fatalf("%v draws %s and guesses %s", env, m.Protocol(), m.Guess())
		}
	}
	al, _ := fake("TERM", "alacritty")
	al.Update(uv.PrimaryDeviceAttributesEvent{62, 22})
	al.Update(uv.CellSizeEvent{Width: 9, Height: 18})
	if al.Protocol() != None || al.Guess() != None {
		t.Fatal("alacritty offered images")
	}
}

// XTSMGRAPHICS answers set the color registers and largest sixel image, reaching the program as unknown CSI events
func TestSixelLimitAnswers(t *testing.T) {
	xt, _ := fake("XTERM_VERSION", "XTerm(411)")
	for _, seq := range []string{"\x1b[?1;0;16S", "\x1b[?2;0;1000;800S", "\x1b[?1;3;0S"} {
		ev := decode(t, seq)
		if _, ok := ev.(uv.UnknownCsiEvent); !ok {
			t.Fatalf("%q decoded as %T", seq, ev)
		}
		xt.Update(ev)
	}
	if r, w, h := xt.sixelLimits(); r != 16 || w != 1000 || h != 800 {
		t.Fatalf("limits %d registers %dx%d", r, w, h)
	}
}

// A terminal update reports a change only when what a card paints from changes
func TestUpdateReportsChanges(t *testing.T) {
	term, _ := fake("KONSOLE_VERSION", "1")
	if !term.Update(uv.CellSizeEvent{Width: 10, Height: 20}) {
		t.Fatal("a new cell size went unreported")
	}
	if term.Update(uv.CellSizeEvent{Width: 10, Height: 20}) {
		t.Fatal("the same cell size reported a change")
	}
	if term.Update(tea.WindowSizeMsg{Width: 80, Height: 24}) {
		t.Fatal("a window size reported a change while the cell report holds")
	}
	if term.Update(tea.KeyPressMsg{Code: 'a'}) {
		t.Fatal("a key reported a change")
	}
}

// Status line text names the terminal, the protocol, any shared memory, the cell size and tmux
func TestDescribe(t *testing.T) {
	kon, _ := fake("KONSOLE_VERSION", "1")
	kon.Update(answer(queryID, "OK"))
	kon.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	if got := kon.Describe(); got != "konsole, kitty placed, 10x20 cells" {
		t.Fatalf("describe %q", got)
	}
	tm, _ := fake("TERM", "xterm-kitty", "TMUX", "x")
	if got := tm.Describe(); got != "kitty, kitty, 9x18 cells assumed, tmux" {
		t.Fatalf("describe %q", got)
	}
	kit, _ := fake("TERM", "xterm-kitty")
	kit.shm = true
	kit.Update(answer(queryID, "OK"))
	kit.Update(uv.CellSizeEvent{Width: 8, Height: 16})
	if got := kit.Describe(); got != "kitty, kitty, shared memory, 8x16 cells" {
		t.Fatalf("describe %q", got)
	}
	none, _ := fake()
	if got := none.Describe(); got != "unknown terminal, none, 9x18 cells assumed" {
		t.Fatalf("describe %q", got)
	}
}

// Terminal of the identity verified for kitty graphics with a measured cell
func kittyTerm(kv ...string) (*Terminal, *clock) {
	term, c := fake(kv...)
	term.Update(answer(queryID, "OK"))
	term.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	return term, c
}

// A frame waits for its acknowledgement, goes on after it or after the timeout, and paces by interval once silent
func TestAckPacing(t *testing.T) {
	term, c := kittyTerm("TERM", "xterm-kitty")
	s := term.NewSurface(term.Protocol())
	if !s.Ready(c.now) {
		t.Fatal("not ready before any frame")
	}
	px := ramp(32, 16)
	out := s.Emit(s.Pack(px, 32, 16, 4, 2), 0, 0, c.now)
	id := idOf(t, gcmds(out))
	if s.Ready(c.add(100 * time.Millisecond)) {
		t.Fatal("ready while the frame is in flight")
	}
	c.add(20 * time.Millisecond)
	if !term.Update(answer(id, "OK")) {
		t.Fatal("the acknowledgement asked for no repaint of placeholder cells")
	}
	if !s.Ready(c.now) {
		t.Fatal("not ready after the OK")
	}
	if st := s.Stats(); st.Latency != 120*time.Millisecond || st.Acked != 1 || st.Frames != 1 {
		t.Fatalf("stats %+v", st)
	}
	s.Emit(s.Pack(px, 32, 16, 4, 2), 0, 0, c.now)
	if s.Ready(c.add(ackTimeout - time.Millisecond)) {
		t.Fatal("ready before the timeout")
	}
	if !s.Ready(c.add(time.Millisecond)) || s.Stats().Timeouts != 1 {
		t.Fatalf("not ready after the timeout, stats %+v", s.Stats())
	}
	for range 2 {
		s.Emit(s.Pack(px, 32, 16, 4, 2), 0, 0, c.now)
		if !s.Ready(c.add(ackTimeout)) {
			t.Fatal("not ready after the timeout")
		}
	}
	if s.Stats().Timeouts != 3 {
		t.Fatalf("timeouts %d", s.Stats().Timeouts)
	}
	s.SetRate(20)
	s.Emit(s.Pack(px, 32, 16, 4, 2), 0, 0, c.now)
	if s.Ready(c.add(49 * time.Millisecond)) {
		t.Fatal("silent surface ready before the interval")
	}
	if !s.Ready(c.add(time.Millisecond)) || s.Stats().Timeouts != 3 {
		t.Fatalf("silent surface not ready after the interval, stats %+v", s.Stats())
	}
}

// Sixel and iTerm2 never answer, so the interval of the rate alone paces them
func TestIntervalPacing(t *testing.T) {
	xt, c := fake("XTERM_VERSION", "XTerm(411)")
	xt.Update(uv.PrimaryDeviceAttributesEvent{62, 4})
	xt.Update(uv.CellSizeEvent{Width: 4, Height: 6})
	it, _ := fake("TERM_PROGRAM", "iTerm.app")
	for _, tc := range []struct {
		s    *Surface
		gap  time.Duration
		fast int
	}{{xt.NewSurface(Sixel), time.Second / 8, 50}, {it.NewSurface(ITerm), time.Second / 6, 60}} {
		s := tc.s
		s.SetRate(tc.fast)
		if !s.Ready(c.now) {
			t.Fatalf("%s not ready at first", s.Protocol())
		}
		s.Emit(s.Pack(ramp(8, 6), 8, 6, 2, 1), 0, 0, c.now)
		if s.Ready(c.add(tc.gap - time.Millisecond)) {
			t.Fatalf("%s ready before its clamped interval", s.Protocol())
		}
		if !s.Ready(c.add(time.Millisecond)) {
			t.Fatalf("%s not ready after its interval", s.Protocol())
		}
		if st := s.Stats(); st.Latency != 0 || st.Divisor != 1 || st.Frames != 1 {
			t.Fatalf("%s stats %+v", s.Protocol(), st)
		}
	}
}

// Slow answers raise the divisor by a quarter up to eight, eight fast answers in a row lower it
func TestDivisorAdapts(t *testing.T) {
	term, c := kittyTerm("KONSOLE_VERSION", "1")
	s := term.NewSurface(KittyPlaced)
	s.SetRate(20)
	frame := func(lat time.Duration) {
		out := s.Emit(s.Pack(ramp(16, 8), 16, 8, 2, 1), 0, 0, c.now)
		c.add(lat)
		term.Update(answer(idOf(t, gcmds(out)), "OK"))
	}
	if d := s.Stats().Divisor; d != 2 {
		t.Fatalf("direct transmission starts at divisor %v", d)
	}
	frame(62 * time.Millisecond)
	if d := s.Stats().Divisor; d != 2 {
		t.Fatalf("an answer within 1.25 of the target moved the divisor to %v", d)
	}
	frame(63 * time.Millisecond)
	if d := s.Stats().Divisor; d != 2.5 {
		t.Fatalf("a slow answer left divisor %v", d)
	}
	for range 10 {
		frame(300 * time.Millisecond)
	}
	if d := s.Stats().Divisor; d != 8 {
		t.Fatalf("slow answers left divisor %v, want the cap", d)
	}
	for range 7 {
		frame(24 * time.Millisecond)
	}
	if d := s.Stats().Divisor; d != 8 {
		t.Fatalf("seven fast answers moved the divisor to %v", d)
	}
	frame(24 * time.Millisecond)
	if d := s.Stats().Divisor; d != 8*0.85 {
		t.Fatalf("eight fast answers left divisor %v", d)
	}
	for range 7 {
		frame(24 * time.Millisecond)
	}
	frame(30 * time.Millisecond)
	if d := s.Stats().Divisor; d != 8*0.85 {
		t.Fatalf("a middling answer did not break the run, divisor %v", d)
	}
	for range 400 {
		frame(time.Millisecond)
	}
	if d := s.Stats().Divisor; d != 1 {
		t.Fatalf("fast answers left divisor %v, want the floor", d)
	}
}

// A rise that undoes a lowering doubles the fast run the next lowering takes, so the divisor stops hunting between two sizes
func TestDivisorStopsHunting(t *testing.T) {
	term, c := kittyTerm("KONSOLE_VERSION", "1")
	s := term.NewSurface(KittyPlaced)
	s.SetRate(20)
	frame := func(lat time.Duration) {
		out := s.Emit(s.Pack(ramp(16, 8), 16, 8, 2, 1), 0, 0, c.now)
		c.add(lat)
		term.Update(answer(idOf(t, gcmds(out)), "OK"))
	}
	lower := func(want int) {
		before := s.Stats().Divisor
		for range want - 1 {
			frame(time.Millisecond)
		}
		if d := s.Stats().Divisor; d != before {
			t.Fatalf("%d fast answers moved the divisor from %v to %v", want-1, before, d)
		}
		frame(time.Millisecond)
		if d := s.Stats().Divisor; d != max(before*0.85, 1) {
			t.Fatalf("%d fast answers left the divisor at %v from %v", want, d, before)
		}
	}
	frame(300 * time.Millisecond)
	frame(300 * time.Millisecond)
	lower(8)
	frame(300 * time.Millisecond)
	lower(16)
	frame(300 * time.Millisecond)
	lower(32)
	lower(32)
	for range 10 {
		frame(300 * time.Millisecond)
	}
	lower(64)
	for range 400 {
		frame(300 * time.Millisecond)
	}
	lower(8 << 4)
	for _, run := range []int{8 << 5, 8 << 6, 8 << 6} {
		frame(300 * time.Millisecond)
		lower(run)
	}
}

// Frame size divides native size, keeps aspect, stays within the pixel cap and native size, floors at 64 by 32
func TestBudget(t *testing.T) {
	term, _ := kittyTerm("KONSOLE_VERSION", "1")
	term.Update(uv.PrimaryDeviceAttributesEvent{4})
	direct := term.NewSurface(KittyPlaced)
	if w, h := direct.Budget(100, 30); w != 500 || h != 300 {
		t.Fatalf("direct budget at divisor 2 is %dx%d", w, h)
	}
	direct.divisor = 1
	w, h := direct.Budget(200, 60)
	if float64(w*h) > 0.8e6 || float64(w*h) < 0.79e6 {
		t.Fatalf("direct budget %dx%d misses the cap", w, h)
	}
	if a := float64(w) / float64(h); a < 1.66 || a > 1.67 {
		t.Fatalf("direct budget %dx%d lost the aspect", w, h)
	}
	if w, h := direct.Budget(4, 1); w != 40 || h != 20 {
		t.Fatalf("tiny box budget %dx%d, want the native size", w, h)
	}
	direct.divisor = 8
	if w, h := direct.Budget(20, 4); w != 80 || h != 32 {
		t.Fatalf("floored budget %dx%d", w, h)
	}
	kit, _ := kittyTerm("TERM", "xterm-kitty")
	kit.shm = true
	shared := kit.NewSurface(KittyCells)
	if d := shared.Stats().Divisor; d != 1 {
		t.Fatalf("shared memory starts at divisor %v", d)
	}
	if w, h := shared.Budget(200, 60); float64(w*h) > 2.0e6 || float64(w*h) < 1.99e6 {
		t.Fatalf("shared budget %dx%d misses the cap", w, h)
	}
	if w, h := shared.Budget(100, 50); w != 1000 || h != 1000 {
		t.Fatalf("shared budget %dx%d under the cap is not native", w, h)
	}
	for _, p := range []Protocol{Sixel, ITerm} {
		s := term.NewSurface(p)
		w, h := s.Budget(200, 60)
		if float64(w*h) > 0.3e6 || float64(w*h) < 0.29e6 || s.Stats().Divisor != 1 {
			t.Fatalf("%s budget %dx%d divisor %v", p, w, h, s.Stats().Divisor)
		}
	}
	if w, h := term.NewSurface(None).Budget(10, 10); w != 0 || h != 0 {
		t.Fatalf("no protocol budgets %dx%d", w, h)
	}
}

// Image id a placeholder cell names through its foreground color
func cellID(c *uv.Cell) uint32 {
	r, g, b, _ := c.Style.Fg.RGBA()
	return uint32(r>>8)<<16 | uint32(g>>8)<<8 | uint32(b>>8)
}

// Placeholder cells name only an acknowledged image, and an id is deleted before it is reused
func TestKittyCellsNameAcknowledged(t *testing.T) {
	term, c := kittyTerm("TERM", "xterm-kitty")
	s := term.NewSurface(KittyCells)
	bg := color.RGBA{R: 1, G: 2, B: 3, A: 255}
	buf := uv.NewBuffer(12, 6)
	paint := func() { s.Paint(buf, 2, 1, 8, 3, bg) }
	px := ramp(40, 30)
	out := s.Emit(s.Pack(px, 40, 30, 8, 3), 2, 1, c.now)
	first := gcmds(out)
	a := idOf(t, first)
	if k := first[0].keys; k["U"] != "1" || k["c"] != "8" || k["r"] != "3" || k["s"] != "40" || k["v"] != "30" || k["q"] != "0" {
		t.Fatalf("keys %v", k)
	}
	if strings.Contains(out, "\x1b7") || strings.Contains(out, "a=d") {
		t.Fatalf("first transmission %q", out[:min(len(out), 60)])
	}
	if got := directPixels(t, first); !bytes.Equal(got, px) {
		t.Fatal("pixels did not survive the transmission")
	}
	paint()
	if cell := buf.CellAt(3, 2); Placeholder(cell) || cell.Content != " " || cell.Style.Bg != bg {
		t.Fatalf("cell %q before any acknowledgement", cell.Content)
	}
	term.Update(answer(a, "OK"))
	paint()
	cell := buf.CellAt(3, 2)
	want := string(kitty.Placeholder) + string(kitty.Diacritic(1)) + string(kitty.Diacritic(1))
	if !Placeholder(cell) || cell.Content != want || cellID(cell) != a || cell.Style.Bg != bg || cell.Width != 1 {
		t.Fatalf("cell %q names %d", cell.Content, cellID(cell))
	}
	if outside := buf.CellAt(10, 2); Placeholder(outside) {
		t.Fatal("placeholder painted outside the box")
	}
	out = s.Emit(s.Pack(px, 40, 30, 8, 3), 2, 1, c.add(10*time.Millisecond))
	b := idOf(t, gcmds(out))
	if b == a || strings.Contains(out, "a=d") {
		t.Fatalf("second frame went to %d over %d with %q", b, a, out[:60])
	}
	paint()
	if id := cellID(buf.CellAt(2, 1)); id != a {
		t.Fatalf("cells name %d while %d is in flight", id, b)
	}
	term.Update(answer(b, "OK"))
	paint()
	if id := cellID(buf.CellAt(9, 3)); id != b {
		t.Fatalf("cells name %d after %d was acknowledged", id, b)
	}
	out = s.Emit(s.Pack(px, 40, 30, 8, 3), 2, 1, c.add(10*time.Millisecond))
	cmds := gcmds(out)
	if cmds[0].keys["a"] != "d" || cmds[0].keys["d"] != "I" || cmds[0].keys["i"] != fmt.Sprint(a) || cmds[0].keys["q"] != "2" || idOf(t, cmds) != a {
		t.Fatalf("reuse of %d without its delete first, %v", a, cmds[0].keys)
	}
	paint()
	if id := cellID(buf.CellAt(2, 1)); id != b {
		t.Fatalf("cells name %d while %d is being replaced", id, a)
	}
	term.Update(answer(a, "ENOENT:gone"))
	paint()
	if id := cellID(buf.CellAt(2, 1)); id != b {
		t.Fatalf("cells name %d after the frame into %d failed", id, a)
	}
}

// Placeholder cells past the diacritic table drop the column so the terminal counts on from the left cell
func TestPlaceholderColumnsPastTable(t *testing.T) {
	term, c := kittyTerm("TERM", "xterm-kitty")
	s := term.NewSurface(KittyCells)
	cols := diacritics + 3
	out := s.Emit(s.Pack(ramp(cols, 2), cols, 2, cols, 1), 0, 0, c.now)
	term.Update(answer(idOf(t, gcmds(out)), "OK"))
	buf := uv.NewBuffer(cols, 1)
	s.Paint(buf, 0, 0, cols, 1, nil)
	last := buf.CellAt(diacritics-1, 0).Content
	past := buf.CellAt(diacritics+1, 0).Content
	if last != string(kitty.Placeholder)+string(kitty.Diacritic(0))+string(kitty.Diacritic(diacritics-1)) || past != string(kitty.Placeholder)+string(kitty.Diacritic(0)) {
		t.Fatalf("cells %q and %q", last, past)
	}
}

// Placed images sit under the text at the box, each id deleted before it is placed again
func TestKittyPlacedNeverPilesUp(t *testing.T) {
	term, c := kittyTerm("KONSOLE_VERSION", "260801")
	s := term.NewSurface(term.Protocol())
	px := ramp(24, 20)
	placed := map[string]int{}
	shown := ""
	for f := range 8 {
		out := s.Emit(s.Pack(px, 24, 20, 6, 2), 5, 4, c.add(40*time.Millisecond))
		if !strings.HasPrefix(out, "\x1b7\x1b[5;6H") || !strings.HasSuffix(out, "\x1b8") {
			t.Fatalf("frame %d is not placed at the box %q", f, out[:20])
		}
		cmds := gcmds(out)
		tx := transmissions(cmds)
		k := tx[0].keys
		if k["a"] != "T" || k["p"] != "1" || k["C"] != "1" || k["z"] != "-1" || k["c"] != "6" || k["r"] != "2" || k["q"] != "0" || k["U"] != "" {
			t.Fatalf("frame %d keys %v", f, k)
		}
		id := k["i"]
		for _, cmd := range cmds {
			if cmd.keys["a"] == "d" {
				if cmd.keys["i"] == shown {
					t.Fatalf("frame %d deletes the image on screen", f)
				}
				delete(placed, cmd.keys["i"])
			}
		}
		if f >= 2 && len(cmds) == len(tx) {
			t.Fatalf("frame %d reuses %s without deleting it", f, id)
		}
		placed[id]++
		if placed[id] > 1 || len(placed) > 2 {
			t.Fatalf("frame %d leaves placements %v", f, placed)
		}
		c.add(15 * time.Millisecond)
		term.Update(answer(idOf(t, cmds), "OK"))
		shown = id
	}
	buf := uv.NewBuffer(12, 8)
	s.Paint(buf, 5, 4, 6, 2, color.Black)
	if cell := buf.CellAt(6, 5); cell.Content != " " || cell.Style.Bg != nil || Placeholder(cell) {
		t.Fatalf("cell under a placed image %q background %v", cell.Content, cell.Style.Bg)
	}
}

// Three refused frames in a row mark the protocol broken so the terminal stops offering it
func TestThreeErrorsBreakProtocol(t *testing.T) {
	term, c := kittyTerm("KONSOLE_VERSION", "260801")
	term.Update(uv.PrimaryDeviceAttributesEvent{62, 4})
	s := term.NewSurface(term.Protocol())
	refuse := func(payload string) bool {
		out := s.Emit(s.Pack(ramp(8, 8), 8, 8, 1, 1), 0, 0, c.add(10*time.Millisecond))
		return term.Update(answer(idOf(t, gcmds(out)), payload))
	}
	refuse("EINVAL:bad")
	refuse("EINVAL:bad")
	refuse("OK")
	refuse("EINVAL:bad")
	if refuse("EINVAL:bad") || term.Protocol() != KittyPlaced {
		t.Fatal("an OK between errors did not restart the count")
	}
	if !refuse("ENODATA:short") {
		t.Fatal("the third error in a row asked for no repaint")
	}
	if term.Protocol() != Sixel || term.Guess() != Sixel || s.Stats().Failed != 5 {
		t.Fatalf("after three errors the terminal offers %s, stats %+v", term.Protocol(), s.Stats())
	}
	kit, kc := kittyTerm("TERM", "xterm-kitty")
	cs := kit.NewSurface(KittyCells)
	for range 3 {
		out := cs.Emit(cs.Pack(ramp(8, 8), 8, 8, 1, 1), 0, 0, kc.add(10*time.Millisecond))
		kit.Update(answer(idOf(t, gcmds(out)), "EINVAL:bad"))
	}
	if kit.Protocol() != KittyPlaced {
		t.Fatalf("kitty without placeholders offers %s", kit.Protocol())
	}
	ps := kit.NewSurface(KittyPlaced)
	for range 3 {
		out := ps.Emit(ps.Pack(ramp(8, 8), 8, 8, 1, 1), 0, 0, kc.add(10*time.Millisecond))
		kit.Update(answer(idOf(t, gcmds(out)), "EINVAL:bad"))
	}
	if kit.Protocol() != None || kit.Guess() != None {
		t.Fatalf("kitty with both ways broken offers %s", kit.Protocol())
	}
}

// Every frame is packed into chunks of whole base64 quads with the keys on the first only
func TestDirectChunks(t *testing.T) {
	term, c := kittyTerm("KONSOLE_VERSION", "1")
	s := term.NewSurface(KittyPlaced)
	px := grainy(300, 200)
	cmds := transmissions(gcmds(s.Emit(s.Pack(px, 300, 200, 30, 10), 0, 0, c.now)))
	if len(cmds) < 3 {
		t.Fatalf("%d chunks for a grainy frame", len(cmds))
	}
	if got := directPixels(t, cmds); !bytes.Equal(got, px) {
		t.Fatal("pixels did not survive the chunks")
	}
	small := transmissions(gcmds(s.Emit(s.Pack(make([]byte, 12), 2, 2, 1, 1), 0, 0, c.add(time.Second))))
	if len(small) != 1 || small[0].keys["m"] != "" {
		t.Fatalf("a one chunk frame carries %d chunks keys %v", len(small), small[0].keys)
	}
}

// Under tmux kitty cells come from identity and every escape goes through passthrough one sequence at a time
func TestTmuxWrapping(t *testing.T) {
	term, c := fake("KITTY_WINDOW_ID", "1", "TMUX", "/tmp/tmux-1000/default,1,0", "TERM", "tmux-256color")
	if term.Protocol() != KittyCells || term.Probe() != nil || term.Measure() != nil {
		t.Fatalf("tmux under kitty draws %s", term.Protocol())
	}
	s := term.NewSurface(KittyCells)
	px := grainy(200, 100)
	out := s.Emit(s.Pack(px, 200, 100, 20, 5), 0, 0, c.now)
	if !strings.HasPrefix(out, "\x1bPtmux;\x1b\x1b_G") || !strings.HasSuffix(out, "\x1b\x1b\\\x1b\\") {
		t.Fatalf("wrap %q", out[:40])
	}
	seqs := strings.Count(out, "\x1bPtmux;")
	cmds := gcmds(untmux(out))
	if seqs != len(cmds) || seqs < 2 {
		t.Fatalf("%d passthrough sequences for %d commands", seqs, len(cmds))
	}
	if strings.Contains(strings.ReplaceAll(out, "\x1b\x1b", ""), "\x1b_G") {
		t.Fatal("a graphics command escaped the passthrough")
	}
	if got := directPixels(t, cmds); !bytes.Equal(got, px) {
		t.Fatal("pixels did not survive tmux wrapping")
	}
	if rel := s.Release(); strings.Count(rel, "\x1bPtmux;") != 2 || !strings.Contains(rel, "\x1b\x1b_Ga=d,d=I") {
		t.Fatalf("release %q", rel)
	}
	for _, env := range [][]string{{"XTERM_VERSION", "1", "TMUX", "x"}, {"KONSOLE_VERSION", "1", "TMUX", "x"}, {"TERM_PROGRAM", "vscode", "TMUX", "x"}} {
		other, _ := fake(env...)
		other.Update(uv.PrimaryDeviceAttributesEvent{4})
		other.Update(uv.CellSizeEvent{Width: 8, Height: 16})
		other.Update(answer(queryID, "OK"))
		if other.Protocol() != None || other.Guess() != None {
			t.Fatalf("%v under tmux offers %s and guesses %s", env, other.Protocol(), other.Guess())
		}
	}
}

// Picture decoded from sixel text, with how often each pixel was painted
type sixelImage struct {
	w, h    int
	palette map[int][3]int
	idx     []int
	hits    []int
}

// Decodes one sixel DCS, failing on anything out of shape
func decodeSixel(t *testing.T, dcs string) sixelImage {
	t.Helper()
	body, ok := strings.CutPrefix(dcs, "\x1bP0;1;q\"1;1;")
	if !ok || !strings.HasSuffix(body, "\x1b\\") {
		t.Fatalf("sixel framing %q", dcs[:min(len(dcs), 30)])
	}
	body = strings.TrimSuffix(body, "\x1b\\")
	num := func() int {
		i := 0
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
		}
		n, err := strconv.Atoi(body[:i])
		if err != nil {
			t.Fatalf("number at %q", body[:min(len(body), 10)])
		}
		body = body[i:]
		return n
	}
	img := sixelImage{palette: map[int][3]int{}}
	img.w = num()
	body = body[1:]
	img.h = num()
	img.idx, img.hits = make([]int, img.w*img.h), make([]int, img.w*img.h)
	x, y, col := 0, 0, -1
	put := func(bits byte, n int) {
		for range n {
			for b := range 6 {
				if bits&(1<<b) != 0 {
					if x >= img.w || y+b >= img.h || col < 0 {
						t.Fatalf("sixel paints %d,%d outside %dx%d", x, y+b, img.w, img.h)
					}
					img.idx[(y+b)*img.w+x] = col
					img.hits[(y+b)*img.w+x]++
				}
			}
			x++
		}
	}
	for len(body) > 0 {
		c := body[0]
		body = body[1:]
		switch {
		case c == '#':
			n := num()
			if strings.HasPrefix(body, ";2;") {
				body = body[3:]
				r := num()
				body = body[1:]
				g := num()
				body = body[1:]
				b := num()
				img.palette[n] = [3]int{r, g, b}
				continue
			}
			if _, ok := img.palette[n]; !ok {
				t.Fatalf("color %d used before it is defined", n)
			}
			col = n
		case c == '!':
			n := num()
			ch := body[0]
			body = body[1:]
			put(ch-63, n)
		case c == '$':
			x = 0
		case c == '-':
			x, y = 0, y+6
		case c >= 63 && c <= 126:
			put(c-63, 1)
		default:
			t.Fatalf("stray byte %q in sixel", c)
		}
	}
	return img
}

// Checks every pixel is painted once with a palette color within one level step of the frame under it
func checkSixel(t *testing.T, img sixelImage, src []byte, w, h int, levels [3]int) {
	t.Helper()
	for Y := range img.h {
		for X := range img.w {
			i := Y*img.w + X
			if img.hits[i] != 1 {
				t.Fatalf("pixel %d,%d painted %d times", X, Y, img.hits[i])
			}
			p := img.palette[img.idx[i]]
			s := ((Y*h/img.h)*w + X*w/img.w) * 3
			for ch := range 3 {
				got := p[ch] * 255 / 100
				step := 255/(levels[ch]-1) + 3
				if d := got - int(src[s+ch]); d > step || d < -step {
					t.Fatalf("pixel %d,%d channel %d is %d for %d", X, Y, ch, got, src[s+ch])
				}
			}
		}
	}
}

// Sixel terminal of the given color registers and largest image, cells of 10 by 20
func sixelTerm(registers, gw, gh int) (*Terminal, *clock) {
	term, c := fake("XTERM_VERSION", "XTerm(411)")
	term.Update(uv.PrimaryDeviceAttributesEvent{63, 4})
	term.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	if registers > 0 {
		term.Update(uv.UnknownCsiEvent(fmt.Sprintf("\x1b[?1;0;%dS", registers)))
	}
	if gw > 0 {
		term.Update(uv.UnknownCsiEvent(fmt.Sprintf("\x1b[?2;0;%d;%dS", gw, gh)))
	}
	return term, c
}

// Sixel images cover the box at the native cell size from the cursor, beside which the cursor stays
func TestSixelShape(t *testing.T) {
	term, c := sixelTerm(0, 0, 0)
	s := term.NewSurface(term.Protocol())
	px := ramp(13, 7)
	out := s.Emit(s.Pack(px, 13, 7, 2, 1), 3, 2, c.now)
	pre := "\x1b7\x1b[?8452h\x1b[3;4H"
	suf := "\x1b[?8452l\x1b8"
	if !strings.HasPrefix(out, pre) || !strings.HasSuffix(out, suf) {
		t.Fatalf("sixel placement %q", out[:min(len(out), 40)])
	}
	dcs := strings.TrimSuffix(strings.TrimPrefix(out, pre), suf)
	if !strings.HasPrefix(dcs, "\x1bP0;1;q\"1;1;20;20#0;2;0;0;0#1;2;0;0;20") {
		t.Fatalf("sixel head %q", dcs[:40])
	}
	img := decodeSixel(t, dcs)
	if img.w != 20 || img.h != 20 || len(img.palette) != 216 || strings.Count(dcs, "-") != 3 {
		t.Fatalf("sixel %dx%d with %d colors and %d band breaks", img.w, img.h, len(img.palette), strings.Count(dcs, "-"))
	}
	checkSixel(t, img, px, 13, 7, [3]int{6, 6, 6})
	if rel := s.Release(); rel != "\x1b7\x1b[0m\x1b[3;4H  \x1b8" {
		t.Fatalf("release %q", rel)
	}
	if rel := s.Release(); rel != "" {
		t.Fatalf("second release %q", rel)
	}
}

// Sixel palettes shrink to the registers the terminal reports and images split into tiles it can take
func TestSixelLimits(t *testing.T) {
	term, c := sixelTerm(16, 0, 0)
	s := term.NewSurface(Sixel)
	px := grainy(30, 40)
	out := s.Emit(s.Pack(px, 30, 40, 3, 2), 0, 0, c.now)
	dcs := out[strings.Index(out, "\x1bP"):strings.Index(out, "\x1b[?8452l")]
	img := decodeSixel(t, dcs)
	if len(img.palette) != 16 {
		t.Fatalf("%d colors for 16 registers", len(img.palette))
	}
	for i := range img.palette {
		if i >= 16 {
			t.Fatalf("color %d past the registers", i)
		}
	}
	checkSixel(t, img, px, 30, 40, [3]int{2, 4, 2})
	gray, gc := sixelTerm(4, 0, 0)
	gs := gray.NewSurface(Sixel)
	gout := gs.Emit(gs.Pack(px, 30, 40, 1, 1), 0, 0, gc.now)
	gimg := decodeSixel(t, gout[strings.Index(gout, "\x1bP"):strings.Index(gout, "\x1b[?8452l")])
	if len(gimg.palette) != 4 {
		t.Fatalf("%d grays for 4 registers", len(gimg.palette))
	}
	for _, p := range gimg.palette {
		if p[0] != p[1] || p[1] != p[2] {
			t.Fatalf("gray palette holds %v", p)
		}
	}
	tiled, tc := sixelTerm(0, 100, 50)
	ts := tiled.NewSurface(Sixel)
	tout := ts.Emit(ts.Pack(px, 30, 40, 25, 3), 1, 1, tc.now)
	for _, cup := range []string{"\x1b[2;2H\x1bP", "\x1b[2;12H\x1bP", "\x1b[2;22H\x1bP", "\x1b[4;2H\x1bP", "\x1b[4;12H\x1bP", "\x1b[4;22H\x1bP"} {
		if !strings.Contains(tout, cup) {
			t.Fatalf("tile %q missing", cup)
		}
	}
	if n := strings.Count(tout, "\x1bP0;1;q"); n != 6 {
		t.Fatalf("%d tiles, want 6", n)
	}
	if !strings.Contains(tout, "\"1;1;100;40") || !strings.Contains(tout, "\"1;1;50;40") || !strings.Contains(tout, "\"1;1;100;20") {
		t.Fatal("tiles of the wrong size")
	}
}

// iTerm2 images are PNGs scaled over the box in cells that leave the cursor where it was
func TestITermShape(t *testing.T) {
	term, c := fake("TERM_PROGRAM", "iTerm.app")
	s := term.NewSurface(term.Protocol())
	px := grainy(37, 23)
	out := s.Emit(s.Pack(px, 37, 23, 9, 4), 2, 5, c.now)
	pre := "\x1b7\x1b[6;3H\x1b]1337;File=inline=1;size="
	if !strings.HasPrefix(out, pre) || !strings.HasSuffix(out, "\a\x1b8") {
		t.Fatalf("iterm %q", out[:min(len(out), 60)])
	}
	head, data, ok := strings.Cut(strings.TrimPrefix(out, pre), ":")
	size, rest, _ := strings.Cut(head, ";")
	if !ok || rest != "width=9;height=4;preserveAspectRatio=0;doNotMoveCursor=1" {
		t.Fatalf("iterm arguments %q", head)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(data, "\a\x1b8"))
	if err != nil || strconv.Itoa(len(raw)) != size {
		t.Fatalf("png of %d bytes announced as %s, %v", len(raw), size, err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for y := range 23 {
		for x := range 37 {
			r, g, b, a := img.At(x, y).RGBA()
			i := (y*37 + x) * 3
			if byte(r>>8) != px[i] || byte(g>>8) != px[i+1] || byte(b>>8) != px[i+2] || a != 0xffff {
				t.Fatalf("pixel %d,%d differs", x, y)
			}
		}
	}
	buf := uv.NewBuffer(12, 10)
	s.Paint(buf, 2, 5, 9, 4, color.White)
	if cell := buf.CellAt(3, 6); cell.Content != " " || cell.Style.Bg != color.White {
		t.Fatalf("cell under the image %q background %v", cell.Content, cell.Style.Bg)
	}
	if rel := s.Release(); !strings.HasPrefix(rel, "\x1b7\x1b[0m\x1b[6;3H         \x1b[7;3H") || !strings.HasSuffix(rel, "\x1b8") {
		t.Fatalf("release %q", rel)
	}
}

// Release deletes both images, forgets what flew and refuses packets made before it
func TestRelease(t *testing.T) {
	term, c := kittyTerm("TERM", "xterm-kitty")
	s := term.NewSurface(KittyCells)
	before := s.Pack(ramp(8, 8), 8, 8, 1, 1)
	out := s.Emit(s.Pack(ramp(8, 8), 8, 8, 1, 1), 0, 0, c.now)
	id := idOf(t, gcmds(out))
	rel := s.Release()
	want := fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", s.ids[0], s.ids[1])
	if rel != want {
		t.Fatalf("release %q, want %q", rel, want)
	}
	if !s.Ready(c.now) {
		t.Fatal("not ready after release")
	}
	if term.Update(answer(id, "OK")) {
		t.Fatal("an answer after release reached the surface")
	}
	buf := uv.NewBuffer(2, 2)
	s.Paint(buf, 0, 0, 1, 1, nil)
	if Placeholder(buf.CellAt(0, 0)) {
		t.Fatal("cells name an image after release")
	}
	if s.Emit(before, 0, 0, c.now) != "" {
		t.Fatal("a packet made before release was shown")
	}
	again := s.Emit(s.Pack(ramp(8, 8), 8, 8, 1, 1), 0, 0, c.now)
	if cmds := gcmds(again); len(cmds) != 1 || cmds[0].keys["a"] != "T" {
		t.Fatalf("first frame after release deletes or misses, %d commands", len(cmds))
	}
	if !term.Update(answer(idOf(t, gcmds(again)), "OK")) {
		t.Fatal("answers stopped reaching the surface after it emitted again")
	}
	if term.NewSurface(None).Release() != "" {
		t.Fatal("a surface of no protocol released text")
	}
}

// Older packets never follow newer ones, the newest may show again, packets of other surfaces never show
func TestPacketOrder(t *testing.T) {
	term, c := kittyTerm("KONSOLE_VERSION", "1")
	s := term.NewSurface(KittyPlaced)
	old := s.Pack(ramp(8, 8), 8, 8, 1, 1)
	cur := s.Pack(ramp(8, 8), 8, 8, 1, 1)
	if s.Emit(cur, 0, 0, c.now) == "" {
		t.Fatal("the newest packet did not show")
	}
	if s.Emit(old, 0, 0, c.now) != "" {
		t.Fatal("an older packet showed after a newer one")
	}
	if again := s.Emit(cur, 3, 3, c.add(time.Second)); !strings.HasPrefix(again, "\x1b7\x1b[4;4H") {
		t.Fatalf("the newest packet did not show again at a new place %q", again[:min(len(again), 20)])
	}
	other := term.NewSurface(KittyPlaced)
	if s.Emit(other.Pack(ramp(8, 8), 8, 8, 1, 1), 0, 0, c.now) != "" {
		t.Fatal("a packet of another surface showed")
	}
	if s.Emit(nil, 0, 0, c.now) != "" || s.Pack(ramp(8, 8), 9, 8, 1, 1) != nil || term.NewSurface(None).Pack(ramp(8, 8), 8, 8, 1, 1) != nil {
		t.Fatal("a frame without pixels or protocol packed or showed")
	}
}

// Rate asked of a surface is clamped to what its protocol takes
func TestRateClamp(t *testing.T) {
	term, _ := kittyTerm("KONSOLE_VERSION", "1")
	for _, tc := range []struct {
		p   Protocol
		ask int
		gap time.Duration
	}{{KittyPlaced, 1000, time.Second / 30}, {Sixel, 1000, time.Second / 8}, {ITerm, 1000, time.Second / 6}, {Sixel, 0, time.Second}, {KittyPlaced, 12, time.Second / 12}} {
		s := term.NewSurface(tc.p)
		s.SetRate(tc.ask)
		s.mu.Lock()
		gap := s.interval()
		s.mu.Unlock()
		if gap != tc.gap {
			t.Fatalf("%s asked %d frames runs every %v", tc.p, tc.ask, gap)
		}
	}
}

func benchPack(b *testing.B, s *Surface) {
	w, h := 1360, 960
	rgb := grainy(w, h)
	b.ReportAllocs()
	var p *Packet
	for b.Loop() {
		p = s.Pack(rgb, w, h, 136, 48)
	}
	n := len(p.first) + len(p.rest)
	for _, tl := range p.tiles {
		n += len(tl.data)
	}
	b.ReportMetric(float64(n)/1e6, "MB/frame")
}

func BenchmarkPackKittyDirect(b *testing.B) {
	term, _ := kittyTerm("KONSOLE_VERSION", "1")
	benchPack(b, term.NewSurface(KittyPlaced))
}

func BenchmarkPackSixel(b *testing.B) {
	term, _ := sixelTerm(0, 0, 0)
	benchPack(b, term.NewSurface(Sixel))
}

func BenchmarkPackITerm(b *testing.B) {
	term, _ := fake("TERM_PROGRAM", "iTerm.app")
	benchPack(b, term.NewSurface(ITerm))
}

// Packing on workers while the program emits, paints, answers and reads the pace keeps every surface consistent
func TestConcurrentUse(t *testing.T) {
	term, _ := kittyTerm("TERM", "xterm-kitty")
	s := term.NewSurface(KittyCells)
	px := grainy(64, 32)
	packets := make(chan *Packet, 64)
	done := make(chan struct{})
	for range 4 {
		go func() {
			for range 40 {
				s.Ready(time.Now())
				s.Budget(8, 4)
				packets <- s.Pack(px, 64, 32, 8, 4)
			}
			done <- struct{}{}
		}()
	}
	go func() {
		for range 4 {
			<-done
		}
		close(packets)
	}()
	buf := uv.NewBuffer(8, 4)
	for p := range packets {
		if out := s.Emit(p, 0, 0, time.Now()); out != "" {
			tx := transmissions(gcmds(out))
			if !bytes.Equal(directPixels(t, tx), px) {
				t.Fatal("a frame packed among others lost its pixels")
			}
			term.Update(answer(idOf(t, tx), "OK"))
		}
		s.Paint(buf, 0, 0, 8, 4, nil)
		s.Stats()
	}
	if s.Stats().Frames == 0 {
		t.Fatal("no frame was emitted")
	}
}
