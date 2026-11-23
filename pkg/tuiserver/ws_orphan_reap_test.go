package tuiserver

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Agent double whose plain interrupt runs a case body
type interruptOnlyAgent struct{ fn func() }

func (a interruptOnlyAgent) Interrupt(string) { a.fn() }

// Live async delegation of a UI session, removed when the case ends
func liveDelegation(t *testing.T, delegationID, sid string) func() {
	t.Helper()
	remove := tools.SetAsyncDelegationRecordForTests(delegationID, map[string]any{"delegation_id": delegationID, "status": "running",
		"origin_ui_session_id": sid})
	t.Cleanup(remove)
	return remove
}

// Live window whose socket can close under the session
type closablePeer struct {
	recordingTransport
	closed atomic.Bool
}

func (p *closablePeer) Closed() bool { return p.closed.Load() }

// Waits for the frames a window received
func receivedFrames(peer *closablePeer, want int) []map[string]any {
	deadline := time.Now().Add(5 * time.Second)
	for {
		peer.mu.Lock()
		frames := append([]map[string]any(nil), peer.frames...)
		peer.mu.Unlock()
		if len(frames) >= want || time.Now().After(deadline) {
			return frames
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWsOrphanReapDisabledWhenGraceZero(t *testing.T) {
	prev := wsOrphanReapGraceS
	wsOrphanReapGraceS = func() float64 { return 0 }
	t.Cleanup(func() { wsOrphanReapGraceS = prev })
	scheduleWSOrphanReap(context.Background(), "any-sid", nil, nil)
	if pendingReap("any-sid") != nil {
		t.Fatal("timer armed with a zero grace")
	}
}

// Agent double with a settable activity clock and a recorded interrupt
type activityAgent struct {
	mu          sync.Mutex
	summary     map[string]any
	fail        bool
	interrupted []string
}

func (a *activityAgent) GetActivitySummary() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		panic("summary unavailable")
	}
	return maps.Clone(a.summary)
}

func (a *activityAgent) Interrupt(string) {
	a.mu.Lock()
	a.interrupted = append(a.interrupted, "interrupted")
	a.mu.Unlock()
}

func (a *activityAgent) set(key string, value any) {
	a.mu.Lock()
	a.summary[key] = value
	a.mu.Unlock()
}

func (a *activityAgent) interrupts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.interrupted)
}

func TestWsOrphanActivityGateUnreadableSummaryStaysEligible(t *testing.T) {
	orphanRaceStale(t, 300)
	t.Cleanup(func() { wsOrphanActivityStaleS = func() float64 { return 600 } })
	for name, held := range map[string]any{
		"raises":   &activityAgent{fail: true},
		"none":     nil,
		"no_clock": &struct{}{},
		"no_value": &activityAgent{summary: map[string]any{"seconds_since_activity": nil}},
	} {
		if wsOrphanTurnActivityIsFresh(tuihost.NewSession(map[string]any{"agent": held})) {
			t.Fatalf("%s summary reads as fresh", name)
		}
	}
}

// Agent double counting its closes
type closingAgent struct{ closes int }

func (a *closingAgent) CloseContext(context.Context) error {
	a.closes++
	return nil
}
