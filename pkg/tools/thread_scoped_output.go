package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/mattn/go-isatty"
)

// Shared silencing registry of one stream, kept across proxy generations so redirects never leak a sink
type routingState struct {
	sink io.Writer
}

// Stdout or stderr stand in routing each write by the calling context to the sink or the passthrough
type threadRoutingStream struct {
	passthrough io.Writer
	state       *routingState
}

type silenceKey struct{}

var errNoFileno = errors.New("fileno")

var (
	streamInstallLock sync.Mutex
	installedStreams  = map[string]*threadRoutingStream{}
	streamSinks       = map[string]io.Writer{}
	routingStates     = map[string]*routingState{}
	processStreams    = map[string]io.Writer{"stdout": os.Stdout, "stderr": os.Stderr}
	openStreamSink    = func() (io.Writer, error) { return os.OpenFile(os.DevNull, os.O_WRONLY, 0) }
)

func contextSilenced(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(silenceKey{}).(bool)
	return v
}

func (s *threadRoutingStream) target(ctx context.Context) io.Writer {
	if contextSilenced(ctx) {
		return s.state.sink
	}
	return s.passthrough
}

// Writes to the context target, a failing target reports the whole buffer written instead of an error
func (s *threadRoutingStream) write(ctx context.Context, p []byte) (int, error) {
	n, err := s.target(ctx).Write(p)
	if err != nil {
		return len(p), nil
	}
	return n, nil
}

// Flushes the context target when it buffers, swallowing every failure
func (s *threadRoutingStream) flush(ctx context.Context) {
	switch t := s.target(ctx).(type) {
	case interface{ Flush() error }:
		_ = t.Flush()
	case interface{ Sync() error }:
		_ = t.Sync()
	}
}

// Writes each line in order to the context target, a failing target swallows the rest
func (s *threadRoutingStream) writelines(ctx context.Context, lines []string) {
	t := s.target(ctx)
	for _, line := range lines {
		if _, err := io.WriteString(t, line); err != nil {
			return
		}
	}
}

// Reports whether the context target is a terminal, false for any target that cannot tell
func (s *threadRoutingStream) isatty(ctx context.Context) bool {
	f, ok := s.target(ctx).(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// Reports whether a sink was closed, the file case through its descriptor and others through a Closed method
func sinkClosed(w io.Writer) bool {
	switch t := w.(type) {
	case *os.File:
		_, err := t.Stat()
		return errors.Is(err, os.ErrClosed)
	case interface{ Closed() bool }:
		return t.Closed()
	}
	return false
}

// Descriptor of the context target, an error when the target has none
func (s *threadRoutingStream) fileno(ctx context.Context) (uintptr, error) {
	f, ok := s.target(ctx).(interface{ Fd() uintptr })
	if !ok {
		return 0, errNoFileno
	}
	return f.Fd(), nil
}

// Writer bound to one context so fmt and io helpers route through the proxy
type routedWriter struct {
	stream *threadRoutingStream
	ctx    context.Context
}

func (w routedWriter) Write(p []byte) (int, error) { return w.stream.write(w.ctx, p) }

func (w routedWriter) Flush() error { w.stream.flush(w.ctx); return nil }

func (w routedWriter) IsTerminal() bool { return w.stream.isatty(w.ctx) }

func (w routedWriter) Fd() (uintptr, error) { return w.stream.fileno(w.ctx) }

func (w routedWriter) WriteLines(lines []string) { w.stream.writelines(w.ctx, lines) }

// Installs the routing proxy as the process stream once, adopting a proxy a redirect restored
func ensureStreamInstalled(attr string) *threadRoutingStream {
	streamInstallLock.Lock()
	defer streamInstallLock.Unlock()
	proxy := installedStreams[attr]
	current := processStreams[attr]
	if cur, ok := current.(*threadRoutingStream); ok {
		installedStreams[attr] = cur
		routingStates[attr] = cur.state
		return cur
	}
	if proxy != nil && current == io.Writer(proxy) {
		return proxy
	}
	passthrough := current
	if passthrough == nil {
		if attr == "stderr" {
			passthrough = os.Stderr
		} else {
			passthrough = os.Stdout
		}
	}
	sink := streamSinks[attr]
	if sink == nil || sinkClosed(sink) {
		opened, err := openStreamSink()
		if err != nil {
			opened = io.Discard
		}
		sink = opened
		streamSinks[attr] = sink
	}
	state := routingStates[attr]
	if state == nil || state.sink != sink {
		state = &routingState{sink: sink}
		routingStates[attr] = state
	}
	proxy = &threadRoutingStream{passthrough: passthrough, state: state}
	installedStreams[attr] = proxy
	processStreams[attr] = proxy
	return proxy
}

func (s *threadRoutingStream) Write(p []byte) (int, error) { return s.write(context.Background(), p) }

// Context under which stdout and stderr written through ScopedStdout and ScopedStderr go to the null device
func ThreadScopedSilence(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ensureStreamInstalled("stdout")
	ensureStreamInstalled("stderr")
	return context.WithValue(ctx, silenceKey{}, true)
}

// Process stdout as seen by work under ctx, the null device inside a silenced context
func ScopedStdout(ctx context.Context) io.Writer {
	return routedWriter{stream: ensureStreamInstalled("stdout"), ctx: ctx}
}

// Process stderr as seen by work under ctx, the null device inside a silenced context
func ScopedStderr(ctx context.Context) io.Writer {
	return routedWriter{stream: ensureStreamInstalled("stderr"), ctx: ctx}
}
