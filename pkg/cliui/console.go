package cliui

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
)

// ErrInterrupt is the KeyboardInterrupt of a prompt or menu
var ErrInterrupt = errors.New("KeyboardInterrupt")

// ErrEOF is the EOFError of a prompt reading a closed input
var ErrEOF = errors.New("EOFError")

// Console is the terminal a prompt or menu reads from and draws on
type Console struct {
	In  io.Reader
	Out io.Writer
	// Process environment lookup for NO_COLOR, TERM and ESCDELAY
	Getenv func(string) (string, bool)
	// TTY checks standing in for isatty on the input and output streams
	StdinTTY  func() bool
	StdoutTTY func() bool
	// Terminal rows and columns of the output stream
	TermSize func() (int, int, error)
	// Masked secret reader used by password prompts
	MaskedSecretPrompt func(prompt string) (string, error)
	// Full screen opener standing in for curses.wrapper, an error routes menus to the numbered fallback
	OpenScreen func() (Screen, func(), error)

	pumpOnce sync.Once
	pump     *inputPump
}

var (
	defaultMu      sync.Mutex
	defaultConsole *Console
	secretPromptFn func(string) (string, error)
)

// SetMaskedSecretPrompt installs the masked secret reader password prompts use on every console
func SetMaskedSecretPrompt(fn func(prompt string) (string, error)) {
	defaultMu.Lock()
	secretPromptFn = fn
	defaultMu.Unlock()
}

// Default is the process console over standard input and output
func Default() *Console {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultConsole == nil {
		defaultConsole = &Console{In: os.Stdin, Out: os.Stdout}
	}
	return defaultConsole
}

func (c *Console) getenv(name string) (string, bool) {
	if c.Getenv != nil {
		return c.Getenv(name)
	}
	return os.LookupEnv(name)
}

// Reports isatty of a stream, the mirror wrapping a terminal included
func fileIsTerminal(v any) bool { return pysys.Isatty(v) }

func (c *Console) stdinTTY() bool {
	if c.StdinTTY != nil {
		return c.StdinTTY()
	}
	return fileIsTerminal(c.In)
}

// Reports whether the input stream is a terminal, the isatty of sys.stdin
func (c *Console) StdinIsTTY() bool { return c.stdinTTY() }

func (c *Console) stdoutTTY() bool {
	if c.StdoutTTY != nil {
		return c.StdoutTTY()
	}
	return fileIsTerminal(c.Out)
}

// Reports whether the output stream is a terminal, the isatty of sys.stdout
func (c *Console) StdoutIsTTY() bool { return c.stdoutTTY() }

// Size reports the terminal rows and columns of the output
func (c *Console) Size() (int, int, error) { return c.termSize() }

func (c *Console) termSize() (int, int, error) {
	if c.TermSize != nil {
		return c.TermSize()
	}
	fd, ok := pysys.Fileno(c.Out)
	if !ok {
		return 0, 0, errors.New("output is not a terminal")
	}
	w, h, err := xterm.GetSize(fd)
	return h, w, err
}

func (c *Console) secretPrompt() func(string) (string, error) {
	if c.MaskedSecretPrompt != nil {
		return c.MaskedSecretPrompt
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return secretPromptFn
}

// MaskedSecret reads one hidden value through the masked secret prompt
func (c *Console) MaskedSecret(prompt string) (string, error) { return c.secretPrompt()(prompt) }

// Single reader of one input stream shared by every console over it, reading only while a console waits for input
type inputPump struct {
	bytesCh chan byte
	errCh   chan struct{}
	errMu   sync.Mutex
	readErr error
	mu      sync.Mutex
	arrived *sync.Cond
	waiters int
	gate    readGate
}

// Wait for a stream to become readable that ends early once no console waits on it any more
type readGate interface {
	// True when the stream is readable, false when the last waiting console left first
	wait() bool
	// Ends a wait in progress
	release()
}

var (
	pumpsMu sync.Mutex
	pumps   = map[io.Reader]*inputPump{}
)

// Counts one console waiting for input and wakes the reader goroutine
func (p *inputPump) enter() {
	p.mu.Lock()
	p.waiters++
	p.arrived.Broadcast()
	p.mu.Unlock()
}

// Drops one waiting console, ending the readable wait when it was the last
func (p *inputPump) leave() {
	p.mu.Lock()
	p.waiters--
	idle := p.waiters == 0
	p.mu.Unlock()
	if idle && p.gate != nil {
		p.gate.release()
	}
}

// Blocks until a console waits for input
func (p *inputPump) awaitReader() {
	p.mu.Lock()
	for p.waiters == 0 {
		p.arrived.Wait()
	}
	p.mu.Unlock()
}

func (p *inputPump) wanted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waiters > 0
}

// Pump of a reader, one goroutine per stream taking a byte only while a console waits for input
func pumpFor(r io.Reader) *inputPump {
	shared := reflect.TypeOf(r) != nil && reflect.TypeOf(r).Comparable()
	pumpsMu.Lock()
	defer pumpsMu.Unlock()
	if shared {
		if p, ok := pumps[r]; ok {
			return p
		}
	}
	p := &inputPump{bytesCh: make(chan byte, 4096), errCh: make(chan struct{}), gate: newReadGate(r)}
	p.arrived = sync.NewCond(&p.mu)
	if shared {
		pumps[r] = p
	}
	go func() {
		buf := make([]byte, 1024)
		if p.gate != nil {
			buf = buf[:1]
		}
		for {
			p.awaitReader()
			if p.gate != nil && (!p.gate.wait() || !p.wanted()) {
				continue
			}
			n, err := r.Read(buf)
			for _, b := range buf[:n] {
				p.bytesCh <- b
			}
			if err != nil {
				p.errMu.Lock()
				p.readErr = err
				p.errMu.Unlock()
				close(p.errCh)
				return
			}
		}
	}()
	return p
}

// Attaches the console to the shared reader of its input stream
func (c *Console) startPump() {
	c.pumpOnce.Do(func() { c.pump = pumpFor(c.In) })
}

// errTimeout is a byte read that saw nothing within its window
var errTimeout = errors.New("timeout")

// Reads one input byte waiting at most timeout, forever when timeout is negative
func (c *Console) readByte(timeout time.Duration, interrupt <-chan os.Signal) (byte, error) {
	c.startPump()
	select {
	case b := <-c.pump.bytesCh:
		return b, nil
	default:
	}
	c.pump.enter()
	defer c.pump.leave()
	var timer <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case b := <-c.pump.bytesCh:
		return b, nil
	case <-c.pump.errCh:
		select {
		case b := <-c.pump.bytesCh:
			return b, nil
		default:
		}
		c.pump.errMu.Lock()
		err := c.pump.readErr
		c.pump.errMu.Unlock()
		if errors.Is(err, io.EOF) {
			return 0, ErrEOF
		}
		return 0, err
	case <-timer:
		return 0, errTimeout
	case <-interrupt:
		return 0, ErrInterrupt
	}
}

// Python input, writing the prompt and reading one line, Ctrl-C and end of input as errors
func (c *Console) input(prompt string) (string, error) {
	io.WriteString(c.Out, prompt)
	var sigCh chan os.Signal
	if c.stdinTTY() {
		sigCh = make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)
	}
	var line strings.Builder
	for {
		b, err := c.readByte(-1, sigCh)
		if err != nil {
			if errors.Is(err, ErrEOF) && line.Len() > 0 {
				return line.String(), nil
			}
			return "", err
		}
		if b == '\n' {
			return strings.TrimSuffix(line.String(), "\r"), nil
		}
		line.WriteByte(b)
	}
}

// Puts the input terminal in raw mode, returning the restore function
func (c *Console) makeRaw() (func(), error) {
	f, ok := c.In.(*os.File)
	if !ok || !xterm.IsTerminal(f.Fd()) {
		if c.stdinTTY() {
			return func() {}, nil
		}
		return nil, errors.New("input is not a terminal")
	}
	state, err := xterm.MakeRaw(f.Fd())
	if err != nil {
		return nil, err
	}
	return func() { xterm.Restore(f.Fd(), state) }, nil
}

// FlushStdin drops unread terminal input left behind by a full screen menu, a no op off a terminal
func (c *Console) FlushStdin() {
	if !c.stdinTTY() {
		return
	}
	if f, ok := c.In.(*os.File); ok {
		tcflushInput(int(f.Fd()))
	}
	c.startPump()
	for {
		select {
		case <-c.pump.bytesCh:
		default:
			return
		}
	}
}
