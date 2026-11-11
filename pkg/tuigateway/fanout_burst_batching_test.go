package tuigateway

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Batch capable socket that only records frames and holds every send until released
type loopSocket struct {
	t                *testing.T
	mu               sync.Mutex
	frames           []*pyjson.Object
	sends            int
	firstSendStarted chan struct{}
	startOnce        sync.Once
	release          chan struct{}
	closed           bool
}

func newLoopSocket(t *testing.T) *loopSocket {
	return &loopSocket{t: t, firstSendStarted: make(chan struct{}), release: make(chan struct{})}
}

func (s *loopSocket) WriteManyIfCurrentAsync(frames []*pyjson.Object, isCurrent func() bool) bool {
	if !isCurrent() {
		return true
	}
	s.mu.Lock()
	s.sends++
	s.mu.Unlock()
	s.startOnce.Do(func() { close(s.firstSendStarted) })
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frames...)
	return true
}

func (s *loopSocket) Write(frame *pyjson.Object) bool {
	s.t.Error("a batch capable socket is written through its batch send, not frame by frame")
	return false
}

func (s *loopSocket) Abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *loopSocket) Close() { s.Abort() }

func (s *loopSocket) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, frame := range s.frames {
		payload := frame.GetOr("params", nil).(*pyjson.Object).GetOr("payload", nil).(*pyjson.Object)
		out = append(out, payload.GetOr("text", nil).(string))
	}
	return out
}

func burstDelta(text string) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "method", "event", "params",
		pyjson.ObjectOf("type", "message.delta", "session_id", "s", "payload", pyjson.ObjectOf("text", text)))
}

func awaitFirstSend(t *testing.T, socket *loopSocket) {
	t.Helper()
	select {
	case <-socket.firstSendStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain never reached the socket")
	}
}

func TestTheDrainSendsAQueuedBacklogAsOneBatch(t *testing.T) {
	socket := newLoopSocket(t)
	fanout := NewFanoutTransport(socket)
	var gapMu sync.Mutex
	gaps := 0
	overflow := func(Transport) *pyjson.Object {
		gapMu.Lock()
		defer gapMu.Unlock()
		gaps++
		return nil
	}
	fanout.WriteOverflow(burstDelta("0"), overflow)
	awaitFirstSend(t, socket)
	backlog := fanoutMaxPendingFrames - 1
	want := []string{"0"}
	for index := 1; index <= backlog; index++ {
		fanout.WriteOverflow(burstDelta(strconv.Itoa(index)), overflow)
		want = append(want, strconv.Itoa(index))
	}
	close(socket.release)
	deadline := time.Now().Add(30 * time.Second)
	for len(socket.texts()) != backlog+1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the backlog delivered")
		}
		time.Sleep(time.Millisecond)
	}
	socket.mu.Lock()
	closed, sends := socket.closed, socket.sends
	socket.mu.Unlock()
	if gaps != 0 || closed {
		t.Fatalf("gaps = %d, closed = %v", gaps, closed)
	}
	if got := socket.texts(); !slices.Equal(got, want) {
		t.Fatal("the backlog arrived out of order")
	}
	if sends > 2 {
		t.Fatalf("%d socket round trips for %d frames", sends, backlog+1)
	}
}

func TestAViewerATurnOfStreamedTextBehindKeepsItsSubscription(t *testing.T) {
	socket := newLoopSocket(t)
	fanout := NewFanoutTransport(socket)
	var gapMu sync.Mutex
	gaps := 0
	overflow := func(Transport) *pyjson.Object {
		gapMu.Lock()
		defer gapMu.Unlock()
		gaps++
		return nil
	}
	fanout.WriteOverflow(burstDelta("0"), overflow)
	awaitFirstSend(t, socket)
	const behind = 3000
	want := []string{}
	for index := 1; index <= behind; index++ {
		text := strings.Repeat("x", 300) + strconv.Itoa(index)
		fanout.WriteOverflow(burstDelta(text), overflow)
		want = append(want, text)
	}
	if gaps != 0 || !fanout.Contains(socket) {
		t.Fatal("a viewer one reply behind lost its subscription")
	}
	close(socket.release)
	deadline := time.Now().Add(30 * time.Second)
	for len(socket.texts()) != behind+1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the backlog delivered")
		}
		time.Sleep(time.Millisecond)
	}
	if got := socket.texts()[1:]; !slices.Equal(got, want) {
		t.Fatal("the backlog arrived out of order")
	}
}
