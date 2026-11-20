package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Action and goroutine of one socket or client release step
type auxFdEvent struct {
	action string
	gid    uint64
}

// Goroutine stamped event log shared by the fakes of one test
type auxFdEvents struct {
	mu     sync.Mutex
	events []auxFdEvent
}

func (e *auxFdEvents) add(action string) {
	e.mu.Lock()
	e.events = append(e.events, auxFdEvent{action, pythread.GoroutineID()})
	e.mu.Unlock()
}

func (e *auxFdEvents) snapshot() []auxFdEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]auxFdEvent(nil), e.events...)
}

func (e *auxFdEvents) has(action string, gid uint64) bool {
	for _, ev := range e.snapshot() {
		if ev.action == action && ev.gid == gid {
			return true
		}
	}
	return false
}

// Raw socket double recording shutdown and close with the goroutine that performed them
type auxFdSock struct {
	events   *auxFdEvents
	prefix   string
	shutdown func()
}

func (s *auxFdSock) Read([]byte) (int, error)         { return 0, io.EOF }
func (s *auxFdSock) Write(p []byte) (int, error)      { return len(p), nil }
func (s *auxFdSock) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (s *auxFdSock) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (s *auxFdSock) SetDeadline(time.Time) error      { return nil }
func (s *auxFdSock) SetReadDeadline(time.Time) error  { return nil }
func (s *auxFdSock) SetWriteDeadline(time.Time) error { return nil }
func (s *auxFdSock) CloseRead() error {
	s.events.add(s.prefix + "shutdown")
	if s.shutdown != nil {
		s.shutdown()
	}
	return nil
}
func (s *auxFdSock) CloseWrite() error { return nil }
func (s *auxFdSock) Close() error {
	s.events.add(s.prefix + "sock.close")
	return nil
}

// Responses client whose pool holds one recording socket and whose close is recorded
type auxFdLeafClient struct {
	events    *auxFdEvents
	transport *http.Transport
	stream    AuxChatStream
}

func (c *auxFdLeafClient) BaseURL() string { return "https://chatgpt.com/backend-api/codex" }
func (c *auxFdLeafClient) APIKey() string  { return "codex-key" }
func (c *auxFdLeafClient) Close() error {
	c.events.add("client.close")
	return nil
}
func (c *auxFdLeafClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}
func (c *auxFdLeafClient) CreateResponse(context.Context, map[string]any) (any, error) {
	return c.stream, nil
}
func (c *auxFdLeafClient) poolTransports() []http.RoundTripper {
	return []http.RoundTripper{c.transport}
}

// Adapter over a leaf client whose single pooled socket records every release step
func auxFdAdapter(stream AuxChatStream) (*auxCodexCompletionsAdapter, *auxFdEvents, *auxFdSock) {
	events := &auxFdEvents{}
	sock := &auxFdSock{events: events}
	transport := &http.Transport{}
	set := &auxConnSet{}
	set.add(sock)
	auxPoolConnSets.Store(transport, set)
	client := &auxFdLeafClient{events: events, transport: transport, stream: stream}
	return &auxCodexCompletionsAdapter{client: client, model: "gpt-5.5"}, events, sock
}

// Event stream whose steps run in order, each returning its event after its callback
type auxFdStream struct {
	steps  []func() map[string]any
	events *auxFdEvents
	sock   *auxFdSock
	onEOF  func()
}

func (s *auxFdStream) Next(context.Context) (map[string]any, error) {
	if len(s.steps) == 0 {
		return nil, io.EOF
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step(), nil
}

func (s *auxFdStream) Close() error {
	if s.events != nil {
		s.events.add("stream.close")
	}
	if s.sock != nil {
		_ = s.sock.Close()
	}
	return nil
}

func (s *auxFdStream) auxAttemptConn() net.Conn {
	if s.sock == nil {
		return nil
	}
	return s.sock
}

func auxFdStrangers(events []auxFdEvent, owner uint64, actions ...string) []auxFdEvent {
	var out []auxFdEvent
	for _, ev := range events {
		if ev.gid == owner {
			continue
		}
		if len(actions) == 0 {
			out = append(out, ev)
			continue
		}
		for _, a := range actions {
			if ev.action == a {
				out = append(out, ev)
			}
		}
	}
	return out
}

func auxFdCreate(t *testing.T, adapter *auxCodexCompletionsAdapter) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	_, err := adapter.create(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "summarize"}}, "timeout": 300.0})
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
}

func TestCodexAuxiliaryTimeoutFdOwnershipStalledStreamTimeoutShutsDownFromTimerAndClosesFromOwner(t *testing.T) {
	auxNoProgressWindow(t, 0.3)
	interrupted := make(chan struct{})
	stream := &auxFdStream{steps: []func() map[string]any{func() map[string]any {
		select {
		case <-interrupted:
		case <-time.After(10 * time.Second):
			t.Error("watchdog did not interrupt the stalled transport")
		}
		return auxKeepaliveEvent()
	}}}
	adapter, events, sock := auxFdAdapter(stream)
	var once sync.Once
	sock.shutdown = func() { once.Do(func() { close(interrupted) }) }
	owner := pythread.GoroutineID()
	auxFdCreate(t, adapter)
	time.Sleep(200 * time.Millisecond)
	all := events.snapshot()
	shutdowns := auxFdStrangers(all, owner, "shutdown")
	if len(shutdowns) == 0 || events.has("shutdown", owner) {
		t.Fatalf("shutdown must run on the timer goroutine only: %v", all)
	}
	if strangers := auxFdStrangers(all, owner, "client.close", "sock.close"); len(strangers) != 0 {
		t.Fatalf("stranger-thread FD release: %v", strangers)
	}
	if !events.has("client.close", owner) {
		t.Fatalf("owner never released the client: %v", all)
	}
}

func TestCodexAuxiliaryTimeoutFdOwnershipTimerThreadNeverClosesARealSdkStream(t *testing.T) {
	auxNoProgressWindow(t, 0.3)
	streamEvents := &auxFdEvents{}
	woken := make(chan struct{})
	var once sync.Once
	streamSock := &auxFdSock{events: streamEvents, prefix: "stream.", shutdown: func() { once.Do(func() { close(woken) }) }}
	stream := &auxFdStream{events: streamEvents, sock: streamSock, steps: []func() map[string]any{func() map[string]any {
		select {
		case <-woken:
		case <-time.After(10 * time.Second):
			t.Error("watchdog did not wake the stalled stream")
		}
		return auxKeepaliveEvent()
	}}}
	adapter, clientEvents, _ := auxFdAdapter(stream)
	owner := pythread.GoroutineID()
	auxFdCreate(t, adapter)
	time.Sleep(200 * time.Millisecond)
	strangers := auxFdStrangers(streamEvents.snapshot(), owner)
	if len(strangers) == 0 {
		t.Fatalf("watchdog never touched the stream socket: %v", streamEvents.snapshot())
	}
	for _, ev := range strangers {
		if ev.action != "stream.shutdown" {
			t.Fatalf("stranger-thread FD release: %v", strangers)
		}
	}
	if !streamEvents.has("stream.close", owner) || !clientEvents.has("client.close", owner) {
		t.Fatalf("owner releases missing: stream %v client %v", streamEvents.snapshot(), clientEvents.snapshot())
	}
}

func TestCodexAuxiliaryTimeoutFdOwnershipOwnerThreadDeadlineHitClosesDirectly(t *testing.T) {
	auxNoProgressWindow(t, 0.3)
	prevArm := auxArmTimerFn
	t.Cleanup(func() { auxArmTimerFn = prevArm })
	var never []*time.Timer
	auxArmTimerFn = func(time.Duration, func()) *time.Timer {
		timer := time.AfterFunc(time.Hour, func() {})
		never = append(never, timer)
		return timer
	}
	t.Cleanup(func() {
		for _, timer := range never {
			timer.Stop()
		}
	})
	stream := &auxFdStream{steps: []func() map[string]any{auxKeepaliveEvent, func() map[string]any {
		time.Sleep(time.Second)
		return auxKeepaliveEvent()
	}}}
	adapter, events, _ := auxFdAdapter(stream)
	owner := pythread.GoroutineID()
	auxFdCreate(t, adapter)
	if strangers := auxFdStrangers(events.snapshot(), owner); len(strangers) != 0 {
		t.Fatalf("non-owner activity: %v", strangers)
	}
	if !events.has("client.close", owner) {
		t.Fatalf("owner never released the client: %v", events.snapshot())
	}
}
