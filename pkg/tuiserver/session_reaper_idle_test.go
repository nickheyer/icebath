package tuiserver

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Agent exposing the persist contract of the flush, recording each flushed transcript
type flushAgent struct {
	mu         sync.Mutex
	messages   []*state.Message
	flushCalls [][]*state.Message
	persist    func(messages []*state.Message)
}

func newFlushAgent(content string) *flushAgent {
	return &flushAgent{messages: []*state.Message{{Role: "user", Content: content}}}
}

func (a *flushAgent) SessionMessages() []*state.Message { return a.messages }

func (a *flushAgent) PersistSession(messages *[]*state.Message, _ []*state.Message) error {
	a.mu.Lock()
	a.flushCalls = append(a.flushCalls, append([]*state.Message(nil), *messages...))
	a.mu.Unlock()
	if a.persist != nil {
		a.persist(*messages)
	}
	return nil
}

func (a *flushAgent) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.flushCalls)
}

// Registers one in memory session holding the agent for the case
func registerFlushSession(t *testing.T, sid string, a flushableAgent, extra map[string]any) *tuigateway.Session {
	t.Helper()
	fields := map[string]any{"agent": a, "session_key": sid, "running": false}
	for k, v := range extra {
		fields[k] = v
	}
	session := tuigateway.NewSession(fields)
	swapSessions(t, map[string]*tuigateway.Session{sid: session})
	return session
}

// Sets the incremental flush interval for the case and puts the old one back after it
func swapFlushInterval(t *testing.T, seconds float64) {
	t.Helper()
	saved := incrementalFlushIntervalS
	incrementalFlushIntervalS = seconds
	t.Cleanup(func() { incrementalFlushIntervalS = saved })
}

// Installs the three shutdown steps a case replaces and restores the real ones after it
func swapShutdownSteps(t *testing.T, flush func(*float64) int, release func() bool,
	closeSession func(context.Context, string, string, func(*tuigateway.Session) bool) bool) {
	t.Helper()
	savedFlush, savedRelease, savedClose := shutdownFlushSessions, shutdownReleaseWakeOwner, shutdownCloseSession
	shutdownFlushSessions, shutdownReleaseWakeOwner, shutdownCloseSession = flush, release, closeSession
	t.Cleanup(func() {
		shutdownFlushSessions, shutdownReleaseWakeOwner, shutdownCloseSession = savedFlush, savedRelease, savedClose
	})
}

func TestShutdownSessionsFlushesBeforeTeardown(t *testing.T) {
	t.Cleanup(tools.ResetForegroundExitFenceForTests)
	var mu sync.Mutex
	var order []string
	swapShutdownSteps(t, func(*float64) int {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "flush")
		return 0
	}, func() bool { return false }, func(_ context.Context, sid, _ string, _ func(*tuigateway.Session) bool) bool {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "close:"+sid)
		return true
	})
	swapSessions(t, map[string]*tuigateway.Session{"sess-order": tuigateway.NewSession(map[string]any{"agent": nil, "session_key": "sess-order"})})
	shutdownSessions()
	mu.Lock()
	defer mu.Unlock()
	if len(order) == 0 || order[0] != "flush" {
		t.Fatalf("order = %v", order)
	}
	if !slices.Contains(order, "close:sess-order") {
		t.Fatalf("order = %v", order)
	}
}

func TestPeriodicFlushRespectsIntervalWithFakeClock(t *testing.T) {
	swapFlushInterval(t, 300.0)
	a := newFlushAgent("unflushed turn")
	registerFlushSession(t, "sess-interval", a, nil)
	at := func(now float64) *float64 { return &now }
	if got := flushDirtySessions(at(1_000.0)); got != 1 {
		t.Fatalf("first flush = %d", got)
	}
	if a.calls() != 1 {
		t.Fatalf("flush calls = %d", a.calls())
	}
	if got := flushDirtySessions(at(1_000.0 + 299.0)); got != 0 {
		t.Fatalf("flush within the interval = %d", got)
	}
	if a.calls() != 1 {
		t.Fatalf("flush calls = %d", a.calls())
	}
	if got := flushDirtySessions(at(1_000.0 + 301.0)); got != 1 {
		t.Fatalf("flush past the interval = %d", got)
	}
	if a.calls() != 2 {
		t.Fatalf("flush calls = %d", a.calls())
	}
}

func TestPeriodicFlushSkipsRunningSessions(t *testing.T) {
	swapFlushInterval(t, 300.0)
	a := newFlushAgent("unflushed turn")
	registerFlushSession(t, "sess-running", a, map[string]any{"running": true})
	now := 1_000.0
	if got := flushDirtySessions(&now); got != 0 {
		t.Fatalf("flush of a running session = %d", got)
	}
	if a.calls() != 0 {
		t.Fatalf("flush calls = %d", a.calls())
	}
}
