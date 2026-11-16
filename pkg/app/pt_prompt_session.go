package app

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
)

// One line prompt over the prompt application with a bottom toolbar, erased once a line is accepted
type ptPromptSession struct {
	in      io.Reader
	draw    io.Writer
	history *ptInMemoryHistory
	toolbar func() string

	mu        sync.Mutex
	running   *ptApplication
	cancelled bool
}

// Line buffered writer printing complete lines above a running prompt and straight to out otherwise
type ptStdoutProxy struct {
	mu     sync.Mutex
	app    func() *ptApplication
	out    io.Writer
	above  func(text string)
	buffer bytes.Buffer
}

var _ agent.StdoutProxy = (*ptStdoutProxy)(nil)

// Marks the writer as the patch_stdout proxy whose flushes break carriage return frames
func (p *ptStdoutProxy) StdoutProxy() {}

// Descriptor of the terminal behind the proxy, the StdoutProxy fileno that isatty reads
func (p *ptStdoutProxy) Fd() uintptr {
	if f, ok := p.out.(interface{ Fd() uintptr }); ok {
		return f.Fd()
	}
	return ^uintptr(0)
}

// Reports whether the writer is the function the running prompt handed the terminal to
func (p *ptStdoutProxy) holdsTerminal() bool {
	a := p.app()
	return a != nil && a.IsRunning() && a.TerminalReleased() && a.OnLoop()
}

// Buffers text and writes everything up to its last newline, a partial line too for the holder of the terminal
func (p *ptStdoutProxy) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.buffer.Write(data)
	text := p.buffer.String()
	cut := strings.LastIndex(text, "\n") + 1
	if p.holdsTerminal() {
		cut = len(text)
	}
	p.buffer.Reset()
	p.buffer.WriteString(text[cut:])
	p.mu.Unlock()
	p.emit(text[:cut])
	return len(data), nil
}

// Writes whatever is buffered, a partial line included
func (p *ptStdoutProxy) Flush() {
	p.mu.Lock()
	text := p.buffer.String()
	p.buffer.Reset()
	p.mu.Unlock()
	p.emit(text)
}

// Prints text above the running prompt, or straight to the terminal with no prompt running or for its holder
func (p *ptStdoutProxy) emit(text string) {
	a := p.app()
	switch {
	case text == "":
	case a == nil || !a.IsRunning() || p.holdsTerminal():
		_, _ = io.WriteString(p.out, text)
	case !strings.HasSuffix(text, "\n"):
		a.PrintRaw(text)
	case p.above != nil:
		p.above(strings.TrimSuffix(text, "\n"))
	default:
		a.PrintAbove(strings.TrimSuffix(text, "\n"))
	}
}
