package tty

import (
	"io"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// How long the first paint waits for the terminal to answer the handshake before painting with what it knows
const handshakeLimit = time.Second

// Shortest wait for a width probe before its cells are painted provisionally, raised to four round trips once measured
const probeFloor = 250 * time.Millisecond

// Width probe or capability probe awaiting its cursor report
type probe struct {
	key    probeKey
	x, y   int
	what   int
	sentAt time.Time
}

// What a cursor report answers
const (
	probeWidth = iota
	probeREP
	probeSized
	probeForeign
)

// Turns views into terminal bytes for one terminal, measuring the widths it draws before trusting them
type Renderer struct {
	mu  sync.Mutex
	out io.Writer

	parse  *parser
	scr    *screen
	pl     planner
	widths *widths
	frame  Frame
	tgt    []want
	prev   []want

	view       *tea.View
	content    string
	parsed     bool
	shown      *modes
	w, h       int
	profile    colorprofile.Profile
	entered    bool
	repaint    bool
	visibleCur bool

	shaking        bool
	shakeAt        time.Time
	answered       bool
	silent         bool
	syncOK         bool
	unsetGraphemes bool
	rtt            time.Duration

	probes []probe
	da1    []bool
	pass   []byte

	wake      chan struct{}
	flush     []byte
	err       error
	held      bool
	awaitView bool
	restore   []byte
	onRestore func([]byte)
	// Failed writes since the last one the terminal took
	fails int
	// Told once when the writes failed often enough in a row for the terminal to count as gone
	onDead func(error)
	// Told the timing of every frame written, nil when nobody measures
	onFrame func(FrameStat)
	// Time the model took for the view waiting to be written
	viewTook time.Duration
}

// Renderer writing to the terminal, sized w by h
func NewRenderer(out io.Writer, w, h int) *Renderer {
	links := newLinkTable()
	r := &Renderer{out: out, parse: newParser(links), widths: newWidths(), w: w, h: h,
		profile: colorprofile.TrueColor, wake: make(chan struct{}, 1), rtt: -1}
	r.scr = newScreen(w, h, links)
	return r
}

// Signals the frame loop that there is something to write
func (r *Renderer) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Takes the view to show next, the latest one winning
func (r *Renderer) Render(v tea.View) { r.render(v, 0) }

// Takes the view to show next with the time the model took to build it
func (r *Renderer) render(v tea.View, took time.Duration) {
	r.mu.Lock()
	r.view = &v
	r.viewTook = took
	r.awaitView = false
	if v.Content != r.content {
		r.content, r.parsed = v.Content, false
	}
	r.mu.Unlock()
	r.poke()
}

// Takes bytes another writer meant for the terminal, written ahead of the next frame, noting the queries among them
func (r *Renderer) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.pass = append(r.pass, p...)
	r.noteQueries(p)
	r.mu.Unlock()
	r.poke()
	return len(p), nil
}

// Adds the cursor and device attribute queries of foreign bytes to the reply queues in the order the terminal answers
func (r *Renderer) noteQueries(p []byte) {
	for i := 0; i+2 < len(p); i++ {
		if p[i] != 0x1b || p[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(p) && (p[j] >= '0' && p[j] <= '9' || p[j] == ';' || p[j] == '?') {
			j++
		}
		if j >= len(p) {
			return
		}
		body := string(p[i+2 : j])
		switch {
		case p[j] == 'n' && (body == "6" || body == "?6"):
			r.probes = append(r.probes, probe{what: probeForeign})
		case p[j] == 'c' && (body == "" || body == "0"):
			r.da1 = append(r.da1, false)
		}
	}
}

// Sets the color profile cells are mapped into
func (r *Renderer) SetProfile(p colorprofile.Profile) {
	r.mu.Lock()
	if p != r.profile {
		r.profile = p
		r.parse.profile = p
		clear(r.parse.colors)
		r.parsed = false
	}
	r.mu.Unlock()
	r.poke()
}

// Takes a new terminal size, repainting everything at the next frame
func (r *Renderer) Resize(w, h int) {
	r.mu.Lock()
	if w != r.w || h != r.h {
		r.w, r.h = w, h
		r.scr.resize(w, h)
		r.prev = nil
		r.parsed = false
		r.awaitView = true
	}
	r.repaint = true
	r.mu.Unlock()
	r.poke()
}

// Stops writing frames until Unhold, the terminal belonging to someone else meanwhile
func (r *Renderer) Hold() {
	r.mu.Lock()
	r.held = true
	r.mu.Unlock()
}

// Lets frames flow again, the session entered anew at the next write
func (r *Renderer) Unhold() {
	r.mu.Lock()
	r.held = false
	r.repaint = true
	r.mu.Unlock()
	r.poke()
}

// Clears the terminal and repaints the whole frame at the next write
func (r *Renderer) Clear() {
	r.mu.Lock()
	r.repaint = true
	r.mu.Unlock()
	r.poke()
}

// Takes a terminal answer, reporting whether it was one tty asked for and so is no business of the program
func (r *Renderer) Reply(msg tea.Msg) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch m := msg.(type) {
	case tea.CursorPositionMsg:
		return r.cursorReport(m.X, m.Y)
	case tea.ModeReportMsg:
		switch m.Mode {
		case ansi.ModeSynchronizedOutput:
			r.syncOK = m.Value == ansi.ModeSet || m.Value == ansi.ModeReset
			r.heard()
			return true
		case ansi.ModeUnicodeCore:
			r.unsetGraphemes = m.Value == ansi.ModeReset
			r.heard()
			return true
		}
	case uv.PrimaryDeviceAttributesEvent:
		return r.deviceAttributes()
	}
	return false
}

// Notes that the terminal answers, a silent one measured from then on and painted anew
func (r *Renderer) heard() {
	r.answered = true
	if r.silent {
		r.silent, r.repaint = false, true
	}
}

// Takes a cursor report, learning the width a probe measured
func (r *Renderer) cursorReport(x, y int) bool {
	if len(r.probes) == 0 {
		return false
	}
	p := r.probes[0]
	r.probes = r.probes[1:]
	if p.what == probeForeign {
		return false
	}
	r.heard()
	if r.rtt < 0 || time.Since(p.sentAt) < r.rtt {
		r.rtt = time.Since(p.sentAt)
	}
	n := x - p.x
	if y != p.y {
		n = -1
	}
	switch p.what {
	case probeREP:
		r.pl.rep = n == 3
	case probeSized:
		r.widths.sized = n == 2
	default:
		if n >= 0 {
			r.widths.learn(p.key.text, p.key.enc, n)
		} else {
			delete(r.widths.pending, p.key)
		}
	}
	defer r.poke()
	return true
}

// Takes a device attributes answer, the end of the handshake when it answers tty's query
func (r *Renderer) deviceAttributes() bool {
	if len(r.da1) == 0 {
		return false
	}
	ours := r.da1[0]
	r.da1 = r.da1[1:]
	if ours {
		r.shaking = false
		r.heard()
		r.poke()
	}
	return ours
}

// Bytes that open the session, the handshake queries, alternate screen and autowrap off
func (r *Renderer) handshake(b []byte, now time.Time) []byte {
	b = append(b, ansi.RequestModeSynchronizedOutput...)
	b = append(b, ansi.RequestModeUnicodeCore...)
	b = append(b, ansi.SetModeUnicodeCore...)
	y := r.scratchRow()
	x := r.scratchCol()
	b = cup(b, x, y)
	b = append(b, "x\x1b[2b\x1b[6n"...)
	r.probes = append(r.probes, probe{x: x, y: y, what: probeREP, sentAt: now})
	b = cup(b, x, y)
	b = append(b, "\x1b]66;w=2; \a\x1b[6n"...)
	r.probes = append(r.probes, probe{x: x, y: y, what: probeSized, sentAt: now})
	b = cup(b, x, y)
	b = append(b, "\x1b[K"...)
	b = append(b, ansi.RequestPrimaryDeviceAttributes...)
	r.da1 = append(r.da1, true)
	r.scr.xok, r.scr.yok = false, false
	r.shaking, r.shakeAt = true, now
	return b
}

// Row probes are written in, the last so a report never reads as a modified F3 key
func (r *Renderer) scratchRow() int { return max(r.h-1, 0) }

// Column probes start at, past the F3 ambiguity when the screen has a single row
func (r *Renderer) scratchCol() int {
	if r.h > 1 {
		return 0
	}
	return min(16, max(r.w-1, 0))
}

// How long a width probe may stay unanswered before its cells are painted provisionally
func (r *Renderer) probeLimit() time.Duration {
	if r.rtt < 0 {
		return probeFloor
	}
	return max(probeFloor, 4*r.rtt)
}
