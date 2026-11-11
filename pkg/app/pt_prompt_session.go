package app

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
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
	mu      sync.Mutex
	session *ptPromptSession
	out     io.Writer
	buffer  bytes.Buffer
}

// Proxy that patches prints made while the session prompts so they land above it
func (s *ptPromptSession) patchStdout(out io.Writer) *ptStdoutProxy {
	return &ptStdoutProxy{session: s, out: out}
}

// Buffers text and writes everything up to its last newline
func (p *ptStdoutProxy) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buffer.Write(data)
	text := p.buffer.String()
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		p.buffer.Reset()
		p.buffer.WriteString(text[i+1:])
		p.emit(text[:i+1])
	}
	return len(data), nil
}

// Writes whatever is buffered, a partial line included
func (p *ptStdoutProxy) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.buffer.Len() > 0 {
		text := p.buffer.String()
		p.buffer.Reset()
		p.emit(text)
	}
}

// Prints text above the running prompt or writes it to the terminal when no prompt runs
func (p *ptStdoutProxy) emit(text string) {
	if a := p.session.app(); a != nil && a.IsRunning() {
		if strings.HasSuffix(text, "\n") {
			a.PrintAbove(strings.TrimSuffix(text, "\n"))
		} else {
			a.PrintRaw(text)
		}
		return
	}
	_, _ = io.WriteString(p.out, text)
}
