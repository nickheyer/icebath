package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
)

// Bare live client, the plain object the tests attach as a reconnecting peer
type orphanRacePeer struct{ id int }

func (*orphanRacePeer) Write(*pyjson.Object) bool { return true }
func (*orphanRacePeer) Close()                    {}

// Agent double whose hard interrupt runs the case body, the interrupt I/O outside the resume lock
type orphanInterruptAgent struct{ interrupt func() }

func (a *orphanInterruptAgent) HardInterrupt(string) { a.interrupt() }

// Sets the turn activity stale window the reaper reads
func orphanRaceStale(t *testing.T, stale float64) {
	t.Helper()
	wsOrphanActivityStaleS = func() float64 { return stale }
}

// Pending reap timer of a session
func pendingReap(sid string) *wsReapTimer {
	pendingWSReaps.Lock()
	defer pendingWSReaps.Unlock()
	return pendingWSReaps.timers[sid]
}

// Fires a timer as its expiry would after the given awake and wall seconds
func fireReap(timer *wsReapTimer, awake, wall float64) {
	timer.callback(timer.armedMonotonic+awake, timer.armedWall+wall)
}

// Turn thread that stays alive until the case ends, so an interrupt leaves the turn running
func liveTurnThread(t *testing.T) *memory.ContextThread {
	release := make(chan struct{})
	thread := memory.SpawnContextThread(context.Background(), "turn", func(context.Context) { <-release })
	thread.Start()
	t.Cleanup(func() { close(release) })
	return thread
}
