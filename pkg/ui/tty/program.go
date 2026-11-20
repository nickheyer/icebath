package tty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/term"
)

// Bubble Tea's default frame rate, the pace frames are written at
const frameRate = 60

// Longest a model may take to end the program after a stop request or the loss of the terminal before the program is ended for it
const endGrace = 5 * time.Second

// Gap between two looks at whether the terminal is still there
const goneProbeEvery = time.Second

// Failure of a program whose terminal went away under it
var ErrTerminalGone = errors.New("tty: the terminal is gone")

// Bubble Tea program whose terminal bytes, modes, size, job control and restore tty owns
type Program struct {
	ctx     context.Context
	model   tea.Model
	tea     *tea.Program
	r       *Renderer
	in      *os.File
	out     *os.File
	opened  []*os.File
	unadopt func() error
	tty     *ttyState
	guard   *guard
	last    atomic.Pointer[tea.View]
	mu      sync.Mutex
	held    bool
	execing bool
	conts   int
	ended   syscall.Signal
	fault   error
	done    chan struct{}
	ready   chan struct{}
	onFrame func(FrameStat)
	onPanic func(any)
	lost    sync.Once
}

// Program for the model, run on the process's terminal until it quits or ctx ends
func NewProgram(ctx context.Context, model tea.Model) *Program {
	p := &Program{ctx: ctx, model: model, done: make(chan struct{}), ready: make(chan struct{})}
	return p
}

// Has fn told the timing of every frame written to the terminal, set before Run
func (p *Program) OnFrame(fn func(FrameStat)) { p.onFrame = fn }

// Has fn told the value of a panic in the model before the program reports it and ends, set before Run
func (p *Program) OnPanic(fn func(any)) { p.onPanic = fn }

// Sends a message to the model, waiting until the program runs and dropping it once the program ended
func (p *Program) Send(msg tea.Msg) {
	select {
	case <-p.ready:
	case <-p.done:
		return
	case <-p.ctx.Done():
		return
	}
	p.tea.Send(msg)
}

// Model wrapper that hands each view to the renderer and runs exec requests on the event loop
type host struct {
	inner tea.Model
	p     *Program
}

func (h *host) Init() tea.Cmd {
	defer h.p.caught()
	return h.inner.Init()
}

// Tells the panic listener the value of a panic on its way out of the model, the panic going on to Bubble Tea's own report and restore
func (p *Program) caught() {
	r := recover()
	if r == nil {
		return
	}
	if p.onPanic != nil {
		p.onPanic(r)
	}
	panic(r)
}

func (h *host) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer h.p.caught()
	switch req := msg.(type) {
	case execRequest:
		h.p.runExec(req)
		return h, nil
	case suspendRequest:
		if h.p.suspend(req.line) {
			return h, nil
		}
		msg = SuspendRefusedMsg{}
	}
	in, cmd := h.inner.Update(msg)
	h.inner = in
	return h, cmd
}

func (h *host) View() tea.View {
	defer h.p.caught()
	began := time.Now()
	v := h.inner.View()
	h.p.last.Store(&v)
	h.p.r.render(v, time.Since(began))
	return tea.View{}
}

// Runs the program until it quits, the terminal restored before anything it wrote to stderr is shown
func (p *Program) Run() (model tea.Model, err error) {
	if err := p.open(); err != nil {
		close(p.done)
		return p.model, err
	}
	h := &host{inner: p.model, p: p}
	w, hgt := p.size()
	profile := colorprofile.Detect(p.out, os.Environ())
	p.r = NewRenderer(p.out, w, hgt)
	p.r.SetProfile(profile)
	p.r.onRestore = p.guard.restore
	p.r.onFrame, p.r.onDead = p.onFrame, p.gone
	// The stop signals are tty's own, so the model always hears of one and ends the session itself
	p.tea = tea.NewProgram(h, tea.WithContext(p.ctx), tea.WithInput(p.in), tea.WithOutput(p.r), tea.WithoutRenderer(), tea.WithoutSignalHandler(),
		tea.WithFilter(p.filter), tea.WithColorProfile(profile), tea.WithWindowSize(w, hgt))
	close(p.ready)
	stopSignals := p.signals()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		p.r.Loop(time.Second/frameRate, p.done)
	}()
	go p.watch()
	final, err := p.tea.Run()
	model = h.inner
	if hm, ok := final.(*host); ok {
		model = hm.inner
	}
	if err == nil {
		p.r.Flush(time.Now())
	}
	close(p.done)
	<-loopDone
	stopSignals()
	closeErr := p.close()
	p.mu.Lock()
	fault := p.fault
	p.mu.Unlock()
	if fault != nil {
		return model, errors.Join(fault, p.r.Err(), closeErr)
	}
	return model, errors.Join(err, p.r.Err(), closeErr)
}

// Opens the terminal ends, the tty itself for an end that is not one, raw mode on input, and the restore guard
func (p *Program) open() error {
	p.in, p.out = os.Stdin, os.Stdout
	inTerm, outTerm := term.IsTerminal(p.in.Fd()), term.IsTerminal(p.out.Fd())
	if !inTerm || !outTerm {
		in, out, err := tea.OpenTTY()
		if err != nil {
			return fmt.Errorf("tty: no terminal: %w", err)
		}
		if !inTerm {
			p.in = in
		}
		if !outTerm {
			p.out = out
		}
		ends := []*os.File{in}
		if out != in {
			ends = append(ends, out)
		}
		var unusedErr error
		for _, f := range ends {
			if f == p.in || f == p.out {
				p.opened = append(p.opened, f)
			} else {
				unusedErr = errors.Join(unusedErr, f.Close())
			}
		}
		if unusedErr != nil {
			return p.closeEnds(fmt.Errorf("tty: closing an unused terminal end: %w", unusedErr))
		}
	}
	unadopt, err := adoptInput(p.in)
	if err != nil {
		return p.closeEnds(fmt.Errorf("tty: console input: %w", err))
	}
	p.unadopt = unadopt
	st, err := makeRaw(p.in, p.out)
	if err != nil {
		return p.closeEnds(fmt.Errorf("tty: raw mode: %w", err))
	}
	p.tty = st
	if p.guard, err = startGuard(p.in, p.out, st); err != nil {
		return p.closeEnds(errors.Join(fmt.Errorf("tty: guard: %w", err), st.restore()))
	}
	return nil
}

// Puts the standard input back and closes the terminal ends tty opened, joining any failure to err
func (p *Program) closeEnds(err error) error {
	if p.unadopt != nil {
		err = errors.Join(err, p.unadopt())
	}
	for _, f := range p.opened {
		err = errors.Join(err, f.Close())
	}
	return err
}

// Size of the terminal, COLUMNS and LINES then 80 by 24 standing in for what it does not report
func (p *Program) size() (int, int) {
	w, h, err := term.GetSize(p.out.Fd())
	if err != nil || w <= 0 || h <= 0 {
		w, h = envSize("COLUMNS", 80), envSize("LINES", 24)
	}
	return w, h
}

// Restores the terminal at a clean exit, ends the guard and closes the terminal ends tty opened
func (p *Program) close() error {
	p.r.Release()
	return p.closeEnds(errors.Join(p.tty.restore(), p.guard.finish()))
}

// Messages tty answers or acts on before the model sees them
func (p *Program) filter(_ tea.Model, msg tea.Msg) tea.Msg {
	switch m := msg.(type) {
	case tea.CursorPositionMsg, tea.ModeReportMsg, uv.PrimaryDeviceAttributesEvent:
		if p.r.Reply(msg) {
			return nil
		}
	case tea.WindowSizeMsg:
		p.r.Resize(m.Width, m.Height)
	case tea.ColorProfileMsg:
		p.r.SetProfile(m.Profile)
	case tea.SuspendMsg:
		if p.suspend("") {
			return nil
		}
		return SuspendRefusedMsg{}
	case tea.ResumeMsg:
		p.release()
		p.reclaim()
	case tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseWheelMsg, tea.MouseMotionMsg:
		if v := p.last.Load(); v != nil && v.OnMouse != nil {
			if cmd := v.OnMouse(m.(tea.MouseMsg)); cmd != nil {
				go p.tea.Send(cmd())
			}
		}
	}
	if msg == tea.ClearScreen() {
		p.r.Clear()
	}
	return msg
}

// Gives the terminal back to the shell, raw mode, modes and the alternate screen undone, input reading paused
func (p *Program) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held {
		return
	}
	p.held = true
	p.r.Hold()
	if err := p.tea.ReleaseTerminal(); err != nil {
		p.failLocked(fmt.Errorf("tty: releasing input: %w", err))
	}
	p.r.Release()
	if err := p.tty.restore(); err != nil {
		p.failLocked(fmt.Errorf("tty: restoring the terminal modes: %w", err))
	}
}

// Takes the terminal again after a release, everything repainted at the next frame
func (p *Program) reclaim() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.held {
		return
	}
	p.held = false
	if err := p.tty.raw(); err != nil {
		p.failLocked(fmt.Errorf("tty: raw mode after a release: %w", err))
		return
	}
	if err := p.tea.RestoreTerminal(); err != nil {
		p.failLocked(fmt.Errorf("tty: input after a release: %w", err))
		return
	}
	w, h := p.size()
	p.r.Resize(w, h)
	p.r.Unhold()
	go p.tea.Send(tea.WindowSizeMsg{Width: w, Height: h})
}

// Keeps the first failure to own the terminal and stops the program, Run returning that failure
func (p *Program) failLocked(err error) {
	if p.fault == nil {
		p.fault = err
	}
	go p.tea.Kill()
}

// Stop request of the model with the line written to the terminal it gave back
type suspendRequest struct{ line string }

// Gives the terminal back, writes the line and stops the process group for the shell, refused without job control
func Suspend(line string) tea.Cmd {
	return func() tea.Msg { return suspendRequest{line: line} }
}

// Answer to a suspend the program refused with the terminal untouched, no shell job control holding its process group
type SuspendRefusedMsg struct{}

// Stop request of the operating system, an interrupt, a terminate or a hangup, that the model answers by ending the program
type TerminateMsg struct{ Signal syscall.Signal }

// Records the first stop request of the operating system and hands it to the model, ending the program for a model that does not in time
func (p *Program) terminate(s syscall.Signal) {
	p.mu.Lock()
	first := p.ended == 0
	if first {
		p.ended = s
	}
	p.mu.Unlock()
	if first {
		go p.tea.Send(TerminateMsg{Signal: s})
		time.AfterFunc(endGrace, func() { p.end(nil) })
	}
}

// Loss of the terminal without a hangup signal, every write to it failing, that the model answers by ending the program
type GoneMsg struct{ Err error }

// Tells the model once that the terminal is gone, ending the program with that failure for a model that does not in time
func (p *Program) gone(err error) {
	p.lost.Do(func() {
		go p.tea.Send(GoneMsg{Err: err})
		time.AfterFunc(endGrace, func() { p.end(fmt.Errorf("%w: %w", ErrTerminalGone, err)) })
	})
}

// Ends a program still running, Run returning the failure when there is one
func (p *Program) end(fault error) {
	select {
	case <-p.done:
		return
	default:
	}
	p.mu.Lock()
	if fault != nil && p.fault == nil {
		p.fault = fault
	}
	p.mu.Unlock()
	p.tea.Kill()
}

// Looks every second whether the terminal is still there, since one that went away without a hangup signal ends the input in silence and fails
// no write while nothing is drawn
func (p *Program) watch() {
	tick := time.NewTicker(goneProbeEvery)
	defer tick.Stop()
	misses := 0
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
		p.mu.Lock()
		held := p.held
		p.mu.Unlock()
		err := hungUp(p.out)
		if held || err == nil {
			misses = 0
			continue
		}
		if misses++; misses == deadWriteLimit {
			p.gone(err)
			return
		}
	}
}

// Signal of the operating system that ended the program, zero when it ended on its own
func (p *Program) Terminated() syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ended
}

// Command handed to the program to run on its terminal
type execRequest struct {
	cmd *exec.Cmd
	fn  func(error) tea.Msg
}

// Runs a command on the terminal, the program paused and its screen given back, fn's message delivered after
func Exec(cmd *exec.Cmd, fn func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg { return execRequest{cmd: cmd, fn: fn} }
}

// Runs an exec request with the terminal released, the child writing to the real terminal and stderr
func (p *Program) runExec(req execRequest) {
	p.mu.Lock()
	p.execing = true
	p.mu.Unlock()
	p.release()
	c := req.cmd
	if c.Stdin == nil {
		c.Stdin = p.in
	}
	if c.Stdout == nil {
		c.Stdout = p.out
	}
	if c.Stderr == nil {
		c.Stderr = p.guard.stderr()
	}
	err := c.Run()
	p.mu.Lock()
	p.execing = false
	p.mu.Unlock()
	p.reclaim()
	if req.fn != nil {
		msg := req.fn(err)
		go p.tea.Send(msg)
	}
}
