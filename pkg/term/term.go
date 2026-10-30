package term

import (
	"image/color"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Shape the program asked the cursor to take through DECSCUSR
type CursorShape int

const (
	CursorBlock CursorShape = iota
	CursorUnderline
	CursorBar
)

// Something the program did that the host shows or acts on outside the grid
type EventKind int

const (
	EventBell EventKind = iota
	EventTitle
	EventClipboard
	EventNotify
	EventCwd
	EventProgress
)

// One queued host event with the text it carries
type Event struct {
	Kind EventKind
	Text string
	Body string
}

// Settings an emulator starts with
type Options struct {
	// Rows kept above the screen, none when zero
	Scrollback int
	// Receives every answer the program asked for, such as cursor position reports
	Reply io.Writer
	// Colors reported for the default text, background and cursor
	Fg, Bg, Cursor color.Color
	// Name and version answered to XTVERSION
	Version string
}

// Longest a synchronized update may hold the picture before frames show again
const syncLimit = time.Second

// Mode flags the program sets and resets
type modes struct {
	cursorKeys     bool
	keypadApp      bool
	insert         bool
	newline        bool
	autowrap       bool
	reverse        bool
	showCursor     bool
	blinkCursor    bool
	reverseWrap    bool
	backspaceBS    bool
	margins        bool
	x10Mouse       bool
	mouseNormal    bool
	mouseButton    bool
	mouseAny       bool
	focus          bool
	mouseUTF8      bool
	mouseSGR       bool
	mouseURXVT     bool
	mousePixels    bool
	altScroll      bool
	bracketedPaste bool
	sync           bool
	lockKeyboard   bool
	noEcho         bool
	wide           bool
	alt            bool
	allowWide      bool
	urgentBell     bool
}

// Terminal emulator holding a main grid with scrollback and an alternate grid, safe for one writer and many readers
type Emulator struct {
	mu     sync.Mutex
	parser *ansi.Parser
	w, h   int
	main   *screen
	alt    *screen
	scr    *screen
	back   ring
	// Absolute number of the oldest row the scrollback keeps
	first uint64
	modes modes
	// Level of xterm modifyOtherKeys
	modifyKeys int
	title      string
	icon       string
	titles     []string
	icons      []string
	cwd        string
	host       string
	// Palette entries programs changed through OSC 4
	palette [256]color.Color
	// Default colors programs set through OSC 10, 11 and 12
	fg, bg, cursorColor color.Color
	opts                Options
	shape               CursorShape
	// Last printed rune joins the next one into its cell
	joiner bool
	events []Event
	gen    uint64
	syncAt time.Time
	// Progress the program reported through OSC 9;4
	progress struct {
		state int
		value int
	}
	// Selective erase protection the pen writes with
	protect bool
}

// Builds an emulator of w columns by h rows
func New(w, h int, o Options) *Emulator {
	w, h = max(w, 1), max(h, 1)
	e := &Emulator{w: w, h: h, opts: o}
	e.main = newScreen(w, h)
	e.alt = newScreen(w, h)
	e.scr = e.main
	e.back = newRing(max(o.Scrollback, 0))
	e.modes = defaultModes()
	e.parser = ansi.NewParser()
	e.parser.SetParamsSize(64)
	e.parser.SetDataSize(1 << 20)
	e.parser.SetHandler(ansi.Handler{
		Print:     e.print,
		Execute:   e.execute,
		HandleCsi: e.csi,
		HandleEsc: e.esc,
		HandleDcs: e.dcs,
		HandleOsc: e.osc,
	})
	return e
}

func defaultModes() modes {
	return modes{autowrap: true, showCursor: true, blinkCursor: true, noEcho: true, allowWide: true}
}

// Feeds program output through the parser
func (e *Emulator) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.parser.Parse(p)
	e.gen++
	return len(p), nil
}

// Feeds program output given as a string
func (e *Emulator) WriteString(s string) (int, error) { return e.Write([]byte(s)) }

// Counter that moves whenever output arrived, so a host knows when to draw again
func (e *Emulator) Generation() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gen
}

// Columns and rows of the grid
func (e *Emulator) Size() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w, e.h
}

// Changes the grid to w by h, rewrapping the main screen and its scrollback to the new width
func (e *Emulator) Resize(w, h int) {
	w, h = max(w, 1), max(h, 1)
	e.mu.Lock()
	defer e.mu.Unlock()
	if w == e.w && h == e.h {
		return
	}
	e.reflow(w, h)
	e.alt.resize(w, h)
	e.w, e.h = w, h
	e.gen++
}

// Replaces the rows the scrollback keeps
func (e *Emulator) SetScrollback(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dropped := e.back.setCap(max(n, 0))
	e.first += uint64(dropped)
	e.gen++
}

// Replaces the colors answered for the default text, background and cursor
func (e *Emulator) SetDefaults(fg, bg, cursor color.Color) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts.Fg, e.opts.Bg, e.opts.Cursor = fg, bg, cursor
}

// Forgets every row above the screen
func (e *Emulator) ClearScrollback() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.first += uint64(e.back.n)
	e.back.clear()
	e.gen++
}

// Rows above the screen
func (e *Emulator) ScrollbackLen() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.back.n
}

// Absolute number of the top row of the screen, growing by one for every row pushed above it
func (e *Emulator) Base() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.base()
}

func (e *Emulator) base() uint64 { return e.first + uint64(e.back.n) }

// Takes every event queued since the last call
func (e *Emulator) Events() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.events
	e.events = nil
	return out
}

func (e *Emulator) emit(ev Event) { e.events = append(e.events, ev) }

// Title the program set, empty when it set none
func (e *Emulator) Title() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.title
}

// Working directory the program reported through OSC 7, empty when it reported none or one on another host
func (e *Emulator) Cwd() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !localHost(e.host) {
		return ""
	}
	return e.cwd
}

// Reports whether a file URL host names this machine, an empty host counting as local
func localHost(host string) bool {
	switch host {
	case "", "localhost", "127.0.0.1", "[::1]":
		return true
	}
	name, err := os.Hostname()
	if err != nil {
		return false
	}
	short, _, _ := strings.Cut(name, ".")
	return strings.EqualFold(host, name) || strings.EqualFold(host, short)
}

// Reports whether the alternate screen is shown
func (e *Emulator) AltScreen() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.modes.alt
}

// Reports whether the program tracks the mouse in any mode
func (e *Emulator) MouseTracking() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tracking()
}

func (e *Emulator) tracking() bool {
	return e.modes.x10Mouse || e.modes.mouseNormal || e.modes.mouseButton || e.modes.mouseAny
}

// Reports whether the program asked for bracketed paste
func (e *Emulator) BracketedPaste() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.modes.bracketedPaste
}

// Reports whether the program asked to hear about focus changes
func (e *Emulator) FocusEvents() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.modes.focus
}

// Kitty keyboard flags in force on the shown screen
func (e *Emulator) KittyFlags() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kittyFlags()
}

func (e *Emulator) kittyFlags() int {
	if n := len(e.scr.kitty); n > 0 {
		return e.scr.kitty[n-1]
	}
	return 0
}

// Reports whether a synchronized update holds the picture, which it may for a second at most
func (e *Emulator) Synchronized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.modes.sync && time.Since(e.syncAt) > syncLimit {
		e.modes.sync = false
	}
	return e.modes.sync
}

// Progress the program reported through OSC 9;4, state zero when none is shown
func (e *Emulator) Progress() (state, value int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.progress.state, e.progress.value
}

// Where the cursor is, whether it shows, its shape and blink, and the color the program gave it
type Cursor struct {
	X, Y    int
	Visible bool
	Shape   CursorShape
	Blink   bool
	Color   color.Color
}

// Cursor as the program left it
func (e *Emulator) Cursor() Cursor {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.scr.cur
	return Cursor{X: min(c.x, e.w-1), Y: c.y, Visible: e.modes.showCursor, Shape: e.shape, Blink: e.modes.blinkCursor, Color: e.cursorColor}
}

// Returns everything to its starting state, keeping the scrollback
func (e *Emulator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reset()
	e.gen++
}

func (e *Emulator) reset() {
	e.main.reset(e.w, e.h)
	e.alt.reset(e.w, e.h)
	e.scr = e.main
	e.modes = defaultModes()
	e.modifyKeys = 0
	e.title, e.icon, e.titles, e.icons = "", "", nil, nil
	e.palette = [256]color.Color{}
	e.fg, e.bg, e.cursorColor = nil, nil, nil
	e.shape = CursorBlock
	e.joiner = false
	e.protect = false
	e.progress.state, e.progress.value = 0, 0
	e.emit(Event{Kind: EventTitle})
	e.emit(Event{Kind: EventProgress})
}

// Row at an absolute number, scrollback or screen, false when it is gone or past the bottom
func (e *Emulator) row(abs uint64) (*Line, bool) {
	if abs < e.first {
		return nil, false
	}
	i := int(abs - e.first)
	if i < e.back.n {
		return e.back.at(i), true
	}
	i -= e.back.n
	if i < len(e.scr.lines) {
		return &e.scr.lines[i], true
	}
	return nil, false
}

// Copy of the row at an absolute number, false when it is gone or past the bottom
func (e *Emulator) Row(abs uint64) (Line, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.row(abs)
	if !ok {
		return Line{}, false
	}
	return l.clone(), true
}

// Total rows that ever scrolled above the screen, the dropped ones included
func (e *Emulator) Scrolled() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.first + uint64(e.back.n)
}

// Moves the bottom row of the scroll region into the scrollback when the region spans the whole screen
func (e *Emulator) pushBack(l Line) {
	if e.scr != e.main {
		return
	}
	if e.back.push(l) {
		e.first++
	}
}

// Ring of rows keeping the newest cap rows
type ring struct {
	buf   []Line
	start int
	n     int
}

func newRing(capacity int) ring { return ring{buf: make([]Line, capacity)} }

// Adds a row, reporting whether the oldest was dropped to make room
func (r *ring) push(l Line) bool {
	if len(r.buf) == 0 {
		return true
	}
	if r.n == len(r.buf) {
		r.buf[r.start] = l
		r.start = (r.start + 1) % len(r.buf)
		return true
	}
	r.buf[(r.start+r.n)%len(r.buf)] = l
	r.n++
	return false
}

// Row i counted from the oldest
func (r *ring) at(i int) *Line { return &r.buf[(r.start+i)%len(r.buf)] }

func (r *ring) clear() { r.start, r.n = 0, 0 }

// Rows oldest first
func (r *ring) all() []Line {
	out := make([]Line, 0, r.n)
	for i := 0; i < r.n; i++ {
		out = append(out, *r.at(i))
	}
	return out
}

// Replaces the capacity keeping the newest rows, reporting how many were dropped
func (r *ring) setCap(capacity int) int {
	rows := r.all()
	dropped := 0
	if len(rows) > capacity {
		dropped = len(rows) - capacity
		rows = rows[dropped:]
	}
	r.buf = make([]Line, capacity)
	r.start, r.n = 0, 0
	for _, l := range rows {
		r.push(l)
	}
	return dropped
}

// Replaces every row, newest last, dropping the oldest beyond the capacity
func (r *ring) replace(rows []Line) int {
	r.start, r.n = 0, 0
	dropped := 0
	for _, l := range rows {
		if r.push(l) {
			dropped++
		}
	}
	return dropped
}

// What a host paints the grid with
type View struct {
	// Rows scrolled back into the scrollback, zero for the live screen
	Scroll int
	// Theme colors for the sixteen ANSI colors, a nil entry leaving the terminal default
	Colors [16]color.Color
	// Colors cells take when the program swaps them through reverse video or a selection covers them
	Fg, Bg color.Color
	// Highlight colors of selected cells
	SelFg, SelBg color.Color
	Selection    *Selection
	// Spans lit as search matches, by absolute row
	Matches map[uint64][]Span
	MatchFg color.Color
	MatchBg color.Color
	// Span lit as the current match
	Current *Match
	CurFg   color.Color
	CurBg   color.Color
}

// Columns from through to inclusive on one row
type Span struct {
	From, To int
}

// Paints the rows in view into the buffer, the scrollback above the screen when scrolled
func (e *Emulator) Draw(buf *uv.Buffer, v View) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, h := buf.Width(), buf.Height()
	scroll := v.Scroll
	if e.modes.alt {
		scroll = 0
	}
	scroll = min(max(scroll, 0), e.back.n)
	base := e.base()
	for y := 0; y < h; y++ {
		abs := base - uint64(scroll) + uint64(y)
		var l *Line
		if y < scroll {
			l = e.back.at(e.back.n - scroll + y)
		} else if y-scroll < len(e.scr.lines) {
			l = &e.scr.lines[y-scroll]
		}
		out := buf.Lines[y]
		for x := 0; x < w; x++ {
			if l == nil || x >= len(l.Cells) {
				out[x] = uv.EmptyCell
				continue
			}
			c := l.Cells[x]
			if c.Width > 0 {
				c.Style = e.resolve(c.Style, v)
			}
			if x+c.Width > w {
				c = uv.EmptyCell
			}
			out[x] = c
		}
		e.light(out, abs, l, v)
	}
}

// Paints selection and match highlights over one drawn row
func (e *Emulator) light(out uv.Line, abs uint64, l *Line, v View) {
	paint := func(from, to int, fg, bg color.Color) {
		for x := max(from, 0); x <= to && x < len(out); x++ {
			c := &out[x]
			if c.Width == 0 {
				continue
			}
			c.Style.Fg, c.Style.Bg = fg, bg
			c.Style.Attrs &^= uv.AttrReverse
		}
	}
	for _, sp := range v.Matches[abs] {
		paint(sp.From, sp.To, v.MatchFg, v.MatchBg)
	}
	if v.Current != nil && v.Current.Row == abs {
		paint(v.Current.From, v.Current.To, v.CurFg, v.CurBg)
	}
	if v.Selection != nil && l != nil {
		if from, to, ok := v.Selection.span(abs, len(out), l); ok {
			paint(from, to, v.SelFg, v.SelBg)
		}
	}
}

// Style with palette colors mapped to the theme, program palette changes and reverse video applied
func (e *Emulator) resolve(st uv.Style, v View) uv.Style {
	st.Fg = e.color(st.Fg, v)
	st.Bg = e.color(st.Bg, v)
	st.UnderlineColor = e.color(st.UnderlineColor, v)
	if e.fg != nil && st.Fg == nil {
		st.Fg = e.fg
	}
	if e.bg != nil && st.Bg == nil {
		st.Bg = e.bg
	}
	if e.modes.reverse {
		fg, bg := st.Fg, st.Bg
		if fg == nil {
			fg = v.Fg
		}
		if bg == nil {
			bg = v.Bg
		}
		st.Fg, st.Bg = bg, fg
	}
	return st
}

// Concrete color of a cell color, an indexed one through the program palette then the theme
func (e *Emulator) color(c color.Color, v View) color.Color {
	var i int
	switch c := c.(type) {
	case nil:
		return nil
	case ansi.BasicColor:
		i = int(c)
	case ansi.IndexedColor:
		i = int(c)
	default:
		return c
	}
	if p := e.palette[i]; p != nil {
		return p
	}
	if i < 16 && v.Colors[i] != nil {
		return v.Colors[i]
	}
	return c
}

// Reply writer that drops answers when the host gave none
func (e *Emulator) reply(s string) {
	if e.opts.Reply == nil || s == "" {
		return
	}
	io.WriteString(e.opts.Reply, s)
}
