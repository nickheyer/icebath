// Package gfx shows pixel images over terminal cells through the graphics protocol a terminal is verified to draw
package gfx

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Way a terminal takes an image
type Protocol int

const (
	None Protocol = iota
	KittyCells
	KittyPlaced
	Sixel
	ITerm
)

// Name of the protocol as settings and status lines show it
func (p Protocol) String() string {
	switch p {
	case KittyCells:
		return "kitty"
	case KittyPlaced:
		return "kitty placed"
	case Sixel:
		return "sixel"
	case ITerm:
		return "iterm2"
	}
	return "none"
}

const (
	queryID      = 31
	kittyQuery   = "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\"
	sizeQueries  = "\x1b[16t\x1b[14t"
	sixelQueries = "\x1b[?1;1;0S\x1b[?2;1;0S"
	assumedCellW = 9
	assumedCellH = 18
	firstID      = 0x2c0000
)

// What the identity of a terminal lets it be asked and offered before it answers anything
type traits struct {
	kittyQuery, placeholders, shm, itermSure, itermMaybe, sixelQuery, sizeOnly bool
}

// Traits of every terminal named by identity, any other getting the zero traits
var known = map[string]traits{
	"kitty":     {kittyQuery: true, placeholders: true, shm: true},
	"ghostty":   {kittyQuery: true, placeholders: true, shm: true},
	"wezterm":   {kittyQuery: true, itermMaybe: true, sixelQuery: true},
	"konsole":   {kittyQuery: true, sixelQuery: true},
	"rio":       {kittyQuery: true},
	"vscode":    {itermMaybe: true, sixelQuery: true},
	"iterm":     {itermSure: true, itermMaybe: true},
	"mintty":    {itermMaybe: true},
	"hyper":     {itermMaybe: true},
	"foot":      {sixelQuery: true},
	"xterm":     {sixelQuery: true},
	"mlterm":    {sixelQuery: true},
	"alacritty": {sizeOnly: true},
	"linux":     {sizeOnly: true},
	"dumb":      {sizeOnly: true},
}

// Terminal as named by the environment and as it answered the probe
type Terminal struct {
	mu                        sync.Mutex
	program                   string
	traits                    traits
	tmux                      bool
	kitty                     bool
	sixel                     bool
	shm                       bool
	registers, sixelW, sixelH int
	cellW, cellH, pixW, pixH  int
	cols, rows                int
	broken                    [ITerm + 1]bool
	nextID                    uint32
	surfaces                  map[uint32]*Surface
	clock                     func() time.Time
}

// Terminal read from the process environment
func New() *Terminal { return NewWith(os.Getenv) }

// Terminal read from an environment lookup, for tests
func NewWith(getenv func(string) string) *Terminal {
	t := &Terminal{program: identify(getenv), nextID: firstID, surfaces: map[uint32]*Surface{}, clock: time.Now}
	t.traits = known[t.program]
	t.tmux = getenv("TMUX") != ""
	ssh := getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" || getenv("SSH_CLIENT") != ""
	t.shm = t.traits.shm && !t.tmux && !ssh && shmWorks()
	if t.shm {
		shmReap()
	}
	t.kitty = t.tmux && t.traits.placeholders
	return t
}

// Names the terminal by a specific TERM, then by markers of terminals keeping TERM generic, then by TERM_PROGRAM
func identify(getenv func(string) string) string {
	term := strings.ToLower(getenv("TERM"))
	prog := strings.ToLower(getenv("TERM_PROGRAM"))
	switch {
	case strings.HasPrefix(term, "xterm-kitty"):
		return "kitty"
	case term == "xterm-ghostty":
		return "ghostty"
	case term == "foot" || strings.HasPrefix(term, "foot-"):
		return "foot"
	case strings.HasPrefix(term, "alacritty"):
		return "alacritty"
	case strings.HasPrefix(term, "mlterm"):
		return "mlterm"
	case term == "wezterm", term == "rio", term == "linux", term == "dumb":
		return term
	case getenv("KONSOLE_VERSION") != "":
		return "konsole"
	case getenv("XTERM_VERSION") != "":
		return "xterm"
	case prog == "ghostty", prog == "wezterm", prog == "vscode", prog == "rio", prog == "mintty", prog == "hyper":
		return prog
	case strings.Contains(prog, "iterm"):
		return "iterm"
	case getenv("KITTY_WINDOW_ID") != "":
		return "kitty"
	case getenv("GHOSTTY_RESOURCES_DIR") != "":
		return "ghostty"
	case strings.EqualFold(getenv("LC_TERMINAL"), "iTerm2"):
		return "iterm"
	case getenv("ALACRITTY_WINDOW_ID") != "", getenv("ALACRITTY_SOCKET") != "":
		return "alacritty"
	case getenv("MLTERM") != "":
		return "mlterm"
	}
	return term
}

// Queries worth sending at start, nil when none is
func (t *Terminal) Probe() tea.Cmd {
	if q := t.probe(); q != "" {
		return tea.Raw(q)
	}
	return nil
}

// Kitty query by identity, cell and window pixel sizes, sixel limits by identity and device attributes last
func (t *Terminal) probe() string {
	if t.tmux {
		return ""
	}
	var b strings.Builder
	if t.traits.kittyQuery {
		b.WriteString(kittyQuery)
	}
	b.WriteString(sizeQueries)
	if t.traits.sizeOnly {
		return b.String()
	}
	if t.traits.sixelQuery {
		b.WriteString(sixelQueries)
	}
	b.WriteString(ansi.RequestPrimaryDeviceAttributes)
	return b.String()
}

// Cell and pixel size queries again, after a resize
func (t *Terminal) Measure() tea.Cmd {
	if t.tmux {
		return nil
	}
	return tea.Raw(sizeQueries)
}

// What a card repaints for when it changes
type look struct {
	proto    Protocol
	cw, ch   int
	measured bool
	shm      bool
}

func (t *Terminal) lookLocked() look {
	w, h, m := t.cellLocked()
	return look{t.protocolLocked(), w, h, m, t.shm}
}

// Takes a terminal answer, reporting whether anything changed that a card should repaint for
func (t *Terminal) Update(msg tea.Msg) bool {
	if m, ok := msg.(uv.KittyGraphicsEvent); ok {
		return t.graphicsAnswer(m)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := t.lookLocked()
	switch m := msg.(type) {
	case uv.CellSizeEvent:
		if m.Width > 0 && m.Height > 0 {
			t.cellW, t.cellH = m.Width, m.Height
		}
	case uv.PixelSizeEvent:
		if m.Width > 0 && m.Height > 0 {
			t.pixW, t.pixH = m.Width, m.Height
		}
	case tea.WindowSizeMsg:
		t.cols, t.rows = m.Width, m.Height
	case uv.PrimaryDeviceAttributesEvent:
		for _, a := range m {
			t.sixel = t.sixel || a == 4
		}
	case uv.UnknownCsiEvent:
		t.sixelLimit(string(m))
	default:
		return false
	}
	return t.lookLocked() != before
}

// Reads an XTSMGRAPHICS answer giving the color registers or the largest sixel image
func (t *Terminal) sixelLimit(seq string) {
	if !strings.HasPrefix(seq, "\x1b[?") || !strings.HasSuffix(seq, "S") {
		return
	}
	var v []int
	for _, f := range strings.Split(seq[3:len(seq)-1], ";") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return
		}
		v = append(v, n)
	}
	switch {
	case len(v) == 3 && v[0] == 1 && v[1] == 0 && v[2] > 0:
		t.registers = v[2]
	case len(v) == 4 && v[0] == 2 && v[1] == 0 && v[2] > 0 && v[3] > 0:
		t.sixelW, t.sixelH = v[2], v[3]
	}
}

// Routes a kitty graphics answer to the query or to the surface whose image it names
func (t *Terminal) graphicsAnswer(m uv.KittyGraphicsEvent) bool {
	ok := bytes.HasPrefix(m.Payload, []byte("OK"))
	t.mu.Lock()
	if m.Options.ID == queryID {
		before := t.lookLocked()
		t.kitty = t.kitty || ok
		changed := t.lookLocked() != before
		t.mu.Unlock()
		return changed
	}
	if m.Options.ID <= 0 {
		t.mu.Unlock()
		return false
	}
	s := t.surfaces[uint32(m.Options.ID)]
	now := t.clock()
	t.mu.Unlock()
	if s == nil {
		return false
	}
	r := s.answer(uint32(m.Options.ID), ok, now)
	if !r.refused && !r.broken {
		return r.repaint
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := t.lookLocked()
	if r.refused {
		t.shm = false
	}
	if r.broken {
		t.broken[s.proto] = true
	}
	return r.repaint || t.lookLocked() != before
}

// Best protocol the terminal has been verified to draw, None when none is
func (t *Terminal) Protocol() Protocol {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.protocolLocked()
}

func (t *Terminal) protocolLocked() Protocol {
	_, _, measured := t.cellLocked()
	switch {
	case t.kitty && t.traits.placeholders && !t.broken[KittyCells]:
		return KittyCells
	case t.tmux:
		return None
	case t.kitty && !t.broken[KittyPlaced]:
		return KittyPlaced
	case t.traits.itermSure && !t.broken[ITerm]:
		return ITerm
	case t.sixel && measured && !t.broken[Sixel]:
		return Sixel
	}
	return None
}

// Best protocol worth trying when the person forces images, the verified one else iTerm2 where it is plausible
func (t *Terminal) Guess() Protocol {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.protocolLocked(); p != None {
		return p
	}
	if !t.tmux && t.traits.itermMaybe && !t.broken[ITerm] {
		return ITerm
	}
	return None
}

// Pixels across and down one cell, measured or assumed
func (t *Terminal) Cell() (w, h int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, h, _ = t.cellLocked()
	return w, h
}

// Reports whether the cell size came from the terminal rather than the assumption
func (t *Terminal) Measured() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _, m := t.cellLocked()
	return m
}

// Cell size from the cell report, else the window in pixels over its cells, else assumed
func (t *Terminal) cellLocked() (w, h int, measured bool) {
	switch {
	case t.cellW > 0 && t.cellH > 0:
		return t.cellW, t.cellH, true
	case t.pixW > 0 && t.pixH > 0 && t.cols > 0 && t.rows > 0:
		return max(t.pixW/t.cols, 1), max(t.pixH/t.rows, 1), true
	}
	return assumedCellW, assumedCellH, false
}

// Identity of the terminal, a known name or the lower cased TERM
func (t *Terminal) Program() string { return t.program }

// Reports whether the terminal multiplexer wraps the session
func (t *Terminal) Tmux() bool { return t.tmux }

// One line of what was found, for a status line
func (t *Terminal) Describe() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	name := t.program
	if name == "" {
		name = "unknown terminal"
	}
	proto := t.protocolLocked()
	parts := []string{name, proto.String()}
	if t.shm && (proto == KittyCells || proto == KittyPlaced) {
		parts = append(parts, "shared memory")
	}
	w, h, measured := t.cellLocked()
	cells := fmt.Sprintf("%dx%d cells", w, h)
	if !measured {
		cells += " assumed"
	}
	parts = append(parts, cells)
	if t.tmux {
		parts = append(parts, "tmux")
	}
	return strings.Join(parts, ", ")
}

// Reports whether kitty transmissions go through shared memory now
func (t *Terminal) shared() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.shm
}

// Stops shared memory for good after this machine failed to make an object
func (t *Terminal) dropShm() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.shm = false
}

// Color registers and largest sixel image, zero for whatever went unreported
func (t *Terminal) sixelLimits() (registers, w, h int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.registers, t.sixelW, t.sixelH
}

// Two fresh image ids for a surface
func (t *Terminal) allocIDs() [2]uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.nextID
	t.nextID += 2
	if t.nextID < firstID {
		t.nextID = firstID
	}
	return [2]uint32{a, a + 1}
}

// Routes kitty answers naming the images of the surface to it
func (t *Terminal) register(s *Surface) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.surfaces[s.ids[0]], t.surfaces[s.ids[1]] = s, s
}

// Stops routing kitty answers to the surface
func (t *Terminal) unregister(s *Surface) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, id := range s.ids {
		if t.surfaces[id] == s {
			delete(t.surfaces, id)
		}
	}
}

// Sequence wrapped for tmux to pass to the terminal under it, unchanged outside tmux
func (t *Terminal) wrap(seq string) string {
	if !t.tmux {
		return seq
	}
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}
