package app

import (
	"bytes"
	"io"
	"os"
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

// Prompt session reading keys from in and drawing to out with the toolbar text under the input line
func newPTPromptSession(in io.Reader, out io.Writer, toolbar func() string) *ptPromptSession {
	return &ptPromptSession{in: in, draw: out, history: &ptInMemoryHistory{}, toolbar: toolbar}
}

// Application of the session while a prompt runs, nil between prompts
func (s *ptPromptSession) app() *ptApplication {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Reads one line after the message, the default prefilled, errKeyboardInterrupt on Ctrl-C and io.EOF on Ctrl-D
func (s *ptPromptSession) prompt(message, def string) (string, error) {
	a := newPTApplication(s.in, s.draw)
	a.enableCPR = os.Getenv("PROMPT_TOOLKIT_NO_CPR") != "1"
	buf := newPTBuffer(s.history)
	a.setBuffer(buf)
	if def != "" {
		_ = buf.setDocument(def, len([]rune(def)), true)
	}
	a.prompt = func() []styledFragment { return []styledFragment{{"", message}} }
	a.layout = []*ptWindow{
		{name: "input", kind: ptWindowBuffer},
		{name: "bottom-toolbar", kind: ptWindowText, style: "class:bottom-toolbar", wrapLines: true,
			fragments: func() []styledFragment { return []styledFragment{{"class:bottom-toolbar.text", s.toolbar()}} },
			filter:    func() bool { return s.toolbar != nil && !a.isDone() }},
	}
	var result string
	var failure error
	finish := func(text string, err error) {
		result, failure = text, err
		a.Exit()
	}
	kb := &ptKeyBindings{}
	kb.add(func(e *ptEvent) {
		b := e.currentBuffer()
		text := b.text()
		b.appendToHistory()
		finish(text, nil)
	}, ptBind{}, "enter")
	kb.add(func(e *ptEvent) { finish("", errKeyboardInterrupt) }, ptBind{}, "c-c")
	kb.add(func(e *ptEvent) { finish("", io.EOF) }, ptBind{filter: func() bool { return a.buffer.text() == "" }}, "c-d")
	a.keyBindings = kb
	a.onStart = append(a.onStart, func() {
		s.mu.Lock()
		s.running = a
		cancelled := s.cancelled
		s.cancelled = false
		s.mu.Unlock()
		if cancelled {
			a.Exit()
		}
	})
	err := a.run()
	s.mu.Lock()
	s.running = nil
	s.cancelled = false
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if !a.isDone() {
		return "", io.EOF
	}
	return result, failure
}

// Ends the running prompt the way a cancelled prompt task does, or the one about to start
func (s *ptPromptSession) cancel() {
	s.mu.Lock()
	a := s.running
	if a == nil {
		s.cancelled = true
	}
	s.mu.Unlock()
	if a != nil {
		a.Exit()
	}
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

// Proxy that patches prints made while the session prompts so they land above it
func (s *ptPromptSession) patchStdout(out io.Writer) *ptStdoutProxy {
	return &ptStdoutProxy{app: s.app, out: out}
}

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
