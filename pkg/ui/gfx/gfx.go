// Package gfx shows pixel images over the cells of a terminal through whichever graphics protocol it answers to
package gfx

import (
	"bytes"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Way a terminal takes an image
type Protocol int

const (
	None Protocol = iota
	// Kitty graphics shown through placeholder cells, which follow the cells wherever they go
	KittyCells
	// Kitty graphics placed at a cell position under the text
	KittyPlaced
	// Sixel bitmap placed at a cell position
	Sixel
	// iTerm2 inline image placed at a cell position
	ITerm
)

// Name of the protocol as the settings show it
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

// Image id the kitty query goes out under
const queryID = 31

// Pixels across and down of a cell taken when the terminal reports none
const assumedCellW, assumedCellH = 9, 18

// Terminal as probed, what it draws and how big its cells are
type Terminal struct {
	mu      sync.Mutex
	program string
	tmux    bool
	// What the terminal answered or its name says it draws
	kitty, placeholders, sixel, iterm bool
	// Cell and window size in pixels, zero while unreported
	cellW, cellH int
	pixW, pixH   int
	cols, rows   int
	nextID       uint32
	// Whether pixels go to a kitty terminal through shared memory, on the same machine and until it fails to read one
	shm bool
}

// First image id a surface takes, every lower id being the query
const firstID = 0x2c0000

// Terminal read from the environment, probed for the rest
func New() *Terminal {
	t := &Terminal{nextID: firstID}
	t.program = strings.ToLower(os.Getenv("TERM_PROGRAM"))
	if t.program == "" {
		t.program = strings.ToLower(os.Getenv("TERM"))
	}
	t.tmux = os.Getenv("TMUX") != ""
	if os.Getenv("KITTY_WINDOW_ID") != "" || strings.HasPrefix(t.program, "xterm-kitty") {
		t.program = "kitty"
	}
	if os.Getenv("GHOSTTY_RESOURCES_DIR") != "" {
		t.program = "ghostty"
	}
	if strings.EqualFold(os.Getenv("LC_TERMINAL"), "iTerm2") {
		t.program = "iterm.app"
	}
	switch {
	case strings.Contains(t.program, "iterm"), t.program == "wezterm", t.program == "vscode", t.program == "mintty", t.program == "hyper":
		t.iterm = true
	}
	switch t.program {
	case "kitty", "ghostty", "rio":
		t.placeholders = true
		if t.tmux {
			t.kitty = true
		}
	}
	t.shm = !t.tmux && os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == "" && os.Getenv("SSH_CLIENT") == "" && shmWorks()
	return t
}

// Asks the terminal whether it draws kitty graphics, how big its cells and window are in pixels, and what it is
func (t *Terminal) Probe() tea.Cmd {
	if t.tmux {
		return nil
	}
	q := ansi.KittyGraphics([]byte("AAAA"), "i=31", "s=1", "v=1", "a=q", "t=d", "f=24", "q=1") + ansi.WindowOp(16) + ansi.WindowOp(14) + ansi.RequestPrimaryDeviceAttributes
	return tea.Raw(q)
}

// Asks the terminal for its cell size again, after it was resized
func (t *Terminal) Measure() tea.Cmd {
	if t.tmux {
		return nil
	}
	return tea.Raw(ansi.WindowOp(16) + ansi.WindowOp(14))
}

// Takes in what the terminal answered, reporting whether anything it draws or measures changed
func (t *Terminal) Update(msg tea.Msg) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch m := msg.(type) {
	case uv.KittyGraphicsEvent:
		if m.Options.ID == queryID && bytes.HasPrefix(m.Payload, []byte("OK")) && !t.kitty {
			t.kitty = true
			return true
		}
		if m.Options.ID >= firstID && !bytes.HasPrefix(m.Payload, []byte("OK")) && t.shm {
			t.shm = false
			return true
		}
	case uv.CellSizeEvent:
		if m.Width > 0 && m.Height > 0 && (t.cellW != m.Width || t.cellH != m.Height) {
			t.cellW, t.cellH = m.Width, m.Height
			return true
		}
	case uv.PixelSizeEvent:
		if m.Width > 0 && m.Height > 0 && (t.pixW != m.Width || t.pixH != m.Height) {
			t.pixW, t.pixH = m.Width, m.Height
			return true
		}
	case tea.WindowSizeMsg:
		if t.cols != m.Width || t.rows != m.Height {
			t.cols, t.rows = m.Width, m.Height
			return true
		}
	case uv.PrimaryDeviceAttributesEvent:
		for _, a := range m {
			if a == 4 && !t.sixel {
				t.sixel = true
				return true
			}
		}
	}
	return false
}

// Protocol the terminal draws images with, none when it draws none or cannot be placed
func (t *Terminal) Protocol() Protocol {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _, measured := t.cell()
	switch {
	case t.kitty && t.placeholders:
		return KittyCells
	case t.tmux:
		return None
	case t.kitty:
		return KittyPlaced
	case t.sixel && measured:
		return Sixel
	case t.iterm:
		return ITerm
	}
	return None
}

// Pixels across and down of one cell, from the cell report, else the window in pixels over its cells, else assumed
func (t *Terminal) Cell() (w, h int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, h, _ = t.cell()
	return w, h
}

func (t *Terminal) cell() (w, h int, measured bool) {
	switch {
	case t.cellW > 0 && t.cellH > 0:
		return t.cellW, t.cellH, true
	case t.pixW > 0 && t.pixH > 0 && t.cols > 0 && t.rows > 0:
		return max(t.pixW/t.cols, 1), max(t.pixH/t.rows, 1), true
	}
	return assumedCellW, assumedCellH, false
}

// Frames per second and pixels per frame the protocol takes without holding up the terminal, shared memory carrying the most
func (t *Terminal) Budget() (fps, pixels int) {
	switch t.Protocol() {
	case KittyCells, KittyPlaced:
		if t.shared() {
			return 20, 1600 * 1000
		}
		return 10, 480 * 300
	case Sixel:
		return 8, 640 * 400
	case ITerm:
		return 6, 480 * 300
	}
	return 0, 0
}

// Name of the terminal program as the environment gives it
func (t *Terminal) Program() string { return t.program }

// Reports whether pixels go to the terminal through shared memory
func (t *Terminal) shared() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.shm
}

// Two fresh image ids for a surface
func (t *Terminal) ids() [2]uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.nextID
	t.nextID += 2
	return [2]uint32{a, a + 1}
}

// Sequence wrapped for tmux to pass to the terminal under it, unchanged outside tmux
func (t *Terminal) wrap(seq string) string {
	if !t.tmux || seq == "" {
		return seq
	}
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}
