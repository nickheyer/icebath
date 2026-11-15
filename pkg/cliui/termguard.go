package cliui

import (
	"errors"
	"io"
	"os"
	"runtime"
	"sync"

	xterm "github.com/charmbracelet/x/term"
)

// Screen mode a prompt switched on, with the sequences that set and clear it
type screenMode struct {
	id, on, off string
	out         io.Writer
}

// Raw mode and screen modes held on one terminal, all cleared together when the last hold ends
type termGuard struct {
	mu       sync.Mutex
	file     *os.File
	pristine *xterm.State
	holds    int
	quiet    int
	modes    []screenMode
	stopSig  func()
}

var (
	guardsMu sync.Mutex
	guards   = map[uintptr]*termGuard{}
)

// Guard of the input terminal, one per descriptor so every console over it shares the saved state
func (c *Console) termGuard() *termGuard {
	if f, ok := c.In.(*os.File); ok && xterm.IsTerminal(f.Fd()) {
		guardsMu.Lock()
		defer guardsMu.Unlock()
		g, found := guards[f.Fd()]
		if !found {
			g = &termGuard{file: f}
			guards[f.Fd()] = g
		}
		g.adopt(f)
		return g
	}
	c.guardOnce.Do(func() { c.guard = &termGuard{} })
	return c.guard
}

// Follows the descriptor to the file that now owns it once no hold remains on the earlier one
func (g *termGuard) adopt(f *os.File) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.file != f && g.holds == 0 && g.quiet == 0 {
		g.file, g.pristine, g.modes = f, nil, nil
	}
}

// Puts the input terminal in raw mode until the returned release, nested holds sharing one saved state
func (c *Console) holdRaw() (func(), error) {
	g := c.termGuard()
	if g.file == nil && !c.stdinTTY() {
		return nil, errors.New("input is not a terminal")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.save(); err != nil {
		return nil, err
	}
	g.holds++
	if g.holds == 1 && g.file != nil {
		if _, err := xterm.MakeRaw(g.file.Fd()); err != nil {
			g.holds--
			g.settle()
			return nil, err
		}
	}
	var once sync.Once
	return func() { once.Do(func() { g.release(&g.holds) }) }, nil
}

// Stops the terminal echoing typed keys until the returned release, interrupts still reaching the process
func (c *Console) holdQuiet() func() {
	g := c.termGuard()
	if g.file == nil {
		return func() {}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.save() != nil {
		return func() {}
	}
	g.quiet++
	if g.quiet == 1 && g.holds == 0 {
		muteEcho(g.file)
	}
	var once sync.Once
	return func() { once.Do(func() { g.release(&g.quiet) }) }
}

// Saves the terminal state before the first hold of either kind and starts the termination watch
func (g *termGuard) save() error {
	if g.file == nil || g.pristine != nil {
		return nil
	}
	state, err := xterm.GetState(g.file.Fd())
	if err != nil {
		return err
	}
	g.pristine = state
	g.stopSig = watchTermination(g)
	return nil
}

// Ends one hold of a kind and puts the terminal in the mode the remaining holds call for
func (g *termGuard) release(count *int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if *count == 0 {
		return
	}
	*count--
	g.settle()
}

// Applies the mode the current holds call for, the saved state once none remain
func (g *termGuard) settle() {
	switch {
	case g.holds > 0:
	case g.quiet > 0:
		if g.file != nil && g.pristine != nil {
			xterm.Restore(g.file.Fd(), g.pristine)
			muteEcho(g.file)
		}
	default:
		g.restoreLocked()
	}
}

// Reports whether a prompt holds raw mode, when a keyboard interrupt arrives as a key and not as a signal
func (g *termGuard) rawHeld() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holds > 0
}

// Clears every screen mode newest first, then restores the terminal state saved by the first hold
func (g *termGuard) restoreLocked() {
	for i := len(g.modes) - 1; i >= 0; i-- {
		io.WriteString(g.modes[i].out, g.modes[i].off)
	}
	g.modes = nil
	if g.file != nil && g.pristine != nil {
		xterm.Restore(g.file.Fd(), g.pristine)
	}
	g.pristine = nil
	if g.stopSig != nil {
		g.stopSig()
		g.stopSig = nil
	}
}

// Switches a screen mode on once and remembers the sequence that clears it
func (c *Console) enterMode(id, on, off string) {
	g := c.termGuard()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.modes {
		if m.id == id {
			return
		}
	}
	out := rawWriter(c.Out)
	io.WriteString(out, on)
	g.modes = append(g.modes, screenMode{id: id, on: on, off: off, out: out})
}

// Clears a screen mode that is on
func (c *Console) leaveMode(id string) {
	g := c.termGuard()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, m := range g.modes {
		if m.id == id {
			io.WriteString(m.out, m.off)
			g.modes = append(g.modes[:i], g.modes[i+1:]...)
			return
		}
	}
}

// RestoreTerminal clears the screen modes and raw mode of every terminal a prompt holds
func RestoreTerminal() {
	guardsMu.Lock()
	all := make([]*termGuard, 0, len(guards))
	for _, g := range guards {
		all = append(all, g)
	}
	guardsMu.Unlock()
	for _, g := range all {
		g.mu.Lock()
		if g.holds > 0 || g.quiet > 0 {
			g.holds, g.quiet = 0, 0
			g.restoreLocked()
		}
		g.mu.Unlock()
	}
}

// Reports a terminal on both streams that understands cursor movement and erase sequences
func (c *Console) interactive() bool {
	if !c.stdinTTY() || !c.stdoutTTY() {
		return false
	}
	name, set := c.getenv("TERM")
	if name == "dumb" {
		return false
	}
	return (set && name != "") || runtime.GOOS == "windows"
}

// Interactive reports whether prompts draw their live form on this console
func (c *Console) Interactive() bool { return c.interactive() }
