package tuigateway

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Streamed text event of a session
func deltaEvent(text, sessionID string) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params",
		pyjson.ObjectOf("type", "message.delta", "session_id", sessionID, "payload", pyjson.ObjectOf("text", text)))
}

// Label a frame line carries as params.payload.text
func frameLabel(line string) string {
	frame, err := pyjson.LoadsDict(strings.TrimSpace(line))
	if err != nil {
		return ""
	}
	params, _ := frame.GetOr("params", nil).(*pyjson.Object)
	if params == nil {
		return ""
	}
	payload, _ := params.GetOr("payload", nil).(*pyjson.Object)
	if payload == nil {
		return ""
	}
	label, _ := payload.GetOr("text", "").(string)
	return label
}

// One held label, entered closing when its send arrives and released letting it finish
type sendGate struct {
	entered, released      chan struct{}
	enterOnce, releaseOnce sync.Once
}

func newSendGates(held ...string) map[string]*sendGate {
	gates := map[string]*sendGate{}
	for _, label := range held {
		gates[label] = &sendGate{entered: make(chan struct{}), released: make(chan struct{})}
	}
	return gates
}

func (g *sendGate) pass() {
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.released
}

func (g *sendGate) release() { g.releaseOnce.Do(func() { close(g.released) }) }

func awaitGate(t *testing.T, gate *sendGate, what string) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Socket recording frames, the send of each held label parked until the test releases it
type gatedSocket struct {
	mu     sync.Mutex
	frames []string
	gates  map[string]*sendGate
}

func (s *gatedSocket) SendText(ctx context.Context, text string) error {
	if gate, ok := s.gates[frameLabel(text)]; ok {
		gate.pass()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, text)
	return nil
}

func (s *gatedSocket) Close(code int) error { return nil }

func (s *gatedSocket) sorted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.frames)
	slices.Sort(out)
	return out
}

func sortedFrameTexts(t *testing.T, frames ...*pyjson.Object) []string {
	t.Helper()
	out := []string{}
	for _, frame := range frames {
		out = append(out, frameText(t, frame))
	}
	slices.Sort(out)
	return out
}

func TestDetachDiscardsBatchWaitingForSendLock(t *testing.T) {
	for _, action := range []string{"detach", "reattach", "close", "overflow", "overflow-reattach", "overflow-detach"} {
		t.Run(action, func(t *testing.T) {
			setDuration(t, &wsTokenCoalesce, time.Hour)
			socket := &gatedSocket{}
			transport := NewWSTransport(socket, "", nil)
			fanout := NewFanoutTransport(transport)
			otherSession := NewFanoutTransport(transport)
			buffered := deltaEvent("buffered", "other-session")
			reply := pyjson.ObjectOf("jsonrpc", "2.0", "id", "private-reply", "result", pyjson.ObjectOf("ok", true))
			notice := deltaEvent("resume-required", "session")
			transport.sendLock.lock()
			held := true
			defer func() {
				fanout.Close()
				otherSession.Close()
				if held {
					transport.sendLock.unlock()
				}
				transport.Close()
				waitUntil(t, "fanout cleanup", func() bool { return fanoutIdle(fanout) && fanoutIdle(otherSession) })
			}()
			if !transport.Write(buffered) {
				t.Fatal("buffered token refused")
			}
			if !fanout.Write(deltaEvent("stale", "session")) {
				t.Fatal("stale frame refused")
			}
			waitUntil(t, "the fanout batch waiting for the send lock", func() bool { return transport.sendLock.waiting() == 1 })
			switch {
			case strings.HasPrefix(action, "overflow"):
				savedFrames := FanoutMaxPendingFrames
				FanoutMaxPendingFrames = 2
				defer func() { FanoutMaxPendingFrames = savedFrames }()
				for index := range 3 {
					fanout.WriteOverflow(deltaEvent(pyStrInt(index), "session"), func(Transport) *pyjson.Object { return notice })
				}
				if fanout.Contains(transport) {
					t.Fatal("overflowed peer kept its subscription")
				}
				if action == "overflow-reattach" {
					if !fanout.Attach(transport) || !fanout.Write(deltaEvent("fresh", "session")) {
						t.Fatal("reattach after overflow failed")
					}
				} else if action == "overflow-detach" {
					if fanout.Detach(transport) {
						t.Fatal("detach of an overflowed peer reported an attached peer")
					}
				}
			case action == "close":
				fanout.Close()
			default:
				if !fanout.Detach(transport) {
					t.Fatal("detach failed")
				}
				if action == "reattach" {
					if !fanout.Attach(transport) || !fanout.Write(deltaEvent("fresh", "session")) {
						t.Fatal("reattach failed")
					}
				}
			}
			if !otherSession.Write(deltaEvent("other", "other-session")) {
				t.Fatal("other session write failed")
			}
			response := make(chan bool, 1)
			go func() { response <- transport.WriteAsync(reply) }()
			waitUntil(t, "the reply queued behind the send lock", func() bool { return transport.sendLock.waiting() >= 2 })
			transport.sendLock.unlock()
			held = false
			select {
			case ok := <-response:
				if !ok {
					t.Fatal("reply write failed")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reply never sent")
			}
			waitUntil(t, "both fanout writers drained", func() bool { return fanoutIdle(fanout) && fanoutIdle(otherSession) })
			expected := []*pyjson.Object{buffered, reply, deltaEvent("other", "other-session")}
			if action == "reattach" || action == "overflow-reattach" {
				expected = append(expected, deltaEvent("fresh", "session"))
			}
			if action == "overflow" {
				expected = append(expected, notice)
			}
			if got, want := socket.sorted(), sortedFrameTexts(t, expected...); !slices.Equal(got, want) {
				t.Fatalf("frames = %v\nwant %v", got, want)
			}
			if transport.Closed() {
				t.Fatal("transport closed")
			}
		})
	}
}

// Decimal text of a small index
func pyStrInt(n int) string { return string(rune('0' + n)) }

func TestDetachDiscardsUnsentTailOfCapturedBatch(t *testing.T) {
	for _, reattach := range []bool{false, true} {
		name := "detach"
		if reattach {
			name = "reattach"
		}
		t.Run(name, func(t *testing.T) {
			socket := &gatedSocket{gates: newSendGates("seed", "first")}
			transport := NewWSTransport(socket, "", nil)
			fanout := NewFanoutTransport(transport)
			reply := pyjson.ObjectOf("jsonrpc", "2.0", "id", "reply", "result", pyjson.ObjectOf("ok", true))
			defer func() {
				fanout.Close()
				for _, gate := range socket.gates {
					gate.release()
				}
				transport.Close()
				waitUntil(t, "fanout cleanup", func() bool { return fanoutIdle(fanout) })
			}()
			if !fanout.Write(deltaEvent("seed", "session")) {
				t.Fatal("seed refused")
			}
			awaitGate(t, socket.gates["seed"], "the seed send")
			if !fanout.Write(deltaEvent("first", "session")) || !fanout.Write(deltaEvent("stale-tail", "session")) {
				t.Fatal("queued frame refused")
			}
			socket.gates["seed"].release()
			awaitGate(t, socket.gates["first"], "the captured batch")
			if !fanout.Detach(transport) {
				t.Fatal("detach failed")
			}
			if reattach {
				if !fanout.Attach(transport) || !fanout.Write(deltaEvent("fresh", "session")) {
					t.Fatal("reattach failed")
				}
			}
			response := make(chan bool, 1)
			go func() { response <- transport.WriteAsync(reply) }()
			socket.gates["first"].release()
			select {
			case ok := <-response:
				if !ok {
					t.Fatal("reply write failed")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reply never sent")
			}
			waitUntil(t, "captured fanout batch drained", func() bool { return fanoutIdle(fanout) })
			expected := []*pyjson.Object{deltaEvent("seed", "session"), deltaEvent("first", "session"), reply}
			if reattach {
				expected = append(expected, deltaEvent("fresh", "session"))
			}
			if got, want := socket.sorted(), sortedFrameTexts(t, expected...); !slices.Equal(got, want) {
				t.Fatalf("frames = %v\nwant %v", got, want)
			}
			if transport.Closed() {
				t.Fatal("transport closed")
			}
		})
	}
}

// Stream recording lines, the write of each held label parked until the test releases it
type gatedStream struct {
	mu     sync.Mutex
	text   strings.Builder
	gates  map[string]*sendGate
	closed bool
}

func (s *gatedStream) Write(p []byte) (int, error) {
	if gate, ok := s.gates[frameLabel(string(p))]; ok {
		gate.pass()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.Write(p)
}

func (s *gatedStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *gatedStream) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Split(strings.TrimSuffix(s.text.String(), "\n"), "\n")
}

func TestSyncPeerDiscardsUnsentTailOfCapturedBatch(t *testing.T) {
	for _, reattach := range []bool{false, true} {
		name := "detach"
		if reattach {
			name = "reattach"
		}
		t.Run(name, func(t *testing.T) {
			stream := &gatedStream{gates: newSendGates("seed", "first")}
			transport := NewStdioTransport(func() io.Writer { return stream }, &sync.Mutex{})
			fanout := NewFanoutTransport(transport)
			defer func() {
				fanout.Close()
				for _, gate := range stream.gates {
					gate.release()
				}
				waitUntil(t, "stdio fanout cleanup", func() bool { return fanoutIdle(fanout) })
			}()
			if !fanout.Write(deltaEvent("seed", "session")) {
				t.Fatal("seed refused")
			}
			awaitGate(t, stream.gates["seed"], "first stdio send")
			if !fanout.Write(deltaEvent("first", "session")) || !fanout.Write(deltaEvent("stale-tail", "session")) {
				t.Fatal("queued frame refused")
			}
			stream.gates["seed"].release()
			awaitGate(t, stream.gates["first"], "captured stdio batch")
			if !fanout.Detach(transport) {
				t.Fatal("detach failed")
			}
			if reattach {
				if !fanout.Attach(transport) || !fanout.Write(deltaEvent("fresh", "session")) {
					t.Fatal("reattach failed")
				}
			}
			stream.gates["first"].release()
			waitUntil(t, "stdio fanout drained", func() bool { return fanoutIdle(fanout) })
			expected := []string{frameText(t, deltaEvent("seed", "session")), frameText(t, deltaEvent("first", "session"))}
			if reattach {
				expected = append(expected, frameText(t, deltaEvent("fresh", "session")))
			}
			if got := stream.lines(); !slices.Equal(got, expected) {
				t.Fatalf("lines = %v\nwant %v", got, expected)
			}
			stream.mu.Lock()
			closed := stream.closed
			stream.mu.Unlock()
			if closed {
				t.Fatal("the fanout closed a stream its connection owns")
			}
		})
	}
}
