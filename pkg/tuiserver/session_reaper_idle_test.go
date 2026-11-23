package tuiserver

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
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

// Sets the incremental flush interval for the case and puts the old one back after it
func swapFlushInterval(t *testing.T, seconds float64) {
	t.Helper()
	saved := incrementalFlushIntervalS
	incrementalFlushIntervalS = seconds
	t.Cleanup(func() { incrementalFlushIntervalS = saved })
}

// Installs the three shutdown steps a case replaces and restores the real ones after it
func swapShutdownSteps(t *testing.T, flush func(*float64) int, release func() bool,
	closeSession func(context.Context, string, string, func(*tuihost.Session) bool) bool) {
	t.Helper()
	savedFlush, savedRelease, savedClose := shutdownFlushSessions, shutdownReleaseWakeOwner, shutdownCloseSession
	shutdownFlushSessions, shutdownReleaseWakeOwner, shutdownCloseSession = flush, release, closeSession
	t.Cleanup(func() {
		shutdownFlushSessions, shutdownReleaseWakeOwner, shutdownCloseSession = savedFlush, savedRelease, savedClose
	})
}
