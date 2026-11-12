package tuiserver

import (
	"context"
	"math"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Bare live client, the plain object the hermes cases attach as a reconnecting peer
type orphanRacePeer struct{ id int }

func (*orphanRacePeer) Write(*pyjson.Object) bool { return true }
func (*orphanRacePeer) Close()                    {}

// Agent double whose hard interrupt runs the case body, the interrupt I/O outside the resume lock
type orphanInterruptAgent struct{ interrupt func() }

func (a *orphanInterruptAgent) HardInterrupt(string) { a.interrupt() }

// Registers one session alone with a 20 s orphan grace and the given activity stale window
func orphanRaceSession(t *testing.T, sid string, stale float64, fields map[string]any) *tuigateway.Session {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fields["transport"] = tuigateway.DetachedWSTransport()
	session := tuigateway.NewSession(fields)
	swapSessions(t, map[string]*tuigateway.Session{sid: session})
	prevGrace, prevStale := wsOrphanReapGraceS, wsOrphanActivityStaleS
	wsOrphanReapGraceS = func() float64 { return 20 }
	orphanRaceStale(t, stale)
	t.Cleanup(func() {
		cancelWSOrphanReap(sid)
		wsOrphanReapGraceS, wsOrphanActivityStaleS = prevGrace, prevStale
	})
	return session
}

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

func TestObsoleteOrphanCannotReplaceNewDetachment(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []string{"before_callback", "before_continuation", "before_initial_timer", "cold_resume_claim"} {
		t.Run(phase, func(t *testing.T) {
			sid := "generation-race"
			fake := &orphanInterruptAgent{interrupt: func() {}}
			session := orphanRaceSession(t, sid, 600, map[string]any{"running": true, "agent": fake})
			redetach := func() *wsReapTimer {
				cancelWSOrphanReap(sid)
				session.Set("transport", tuigateway.DetachedWSTransport())
				scheduleWSOrphanReap(ctx, sid, nil, nil)
				return pendingReap(sid)
			}
			if phase == "before_initial_timer" {
				transport := &orphanRacePeer{id: 1}
				session.Set("transport", transport)
				var newest *wsReapTimer
				reconnected := make(chan struct{})
				go func() {
					defer close(reconnected)
					for {
						sessionsLock.Lock()
						parked := session.Get("transport") == tuigateway.DetachedWSTransport()
						sessionsLock.Unlock()
						if parked {
							break
						}
						runtime.Gosched()
					}
					sessionResumeLock.Lock()
					defer sessionResumeLock.Unlock()
					cancelWSOrphanReap(sid)
					scheduleWSOrphanReap(ctx, sid, nil, nil)
					newest = pendingReap(sid)
				}()
				reaped, detached := closeSessionsForTransport(ctx, transport, "ws_disconnect")
				<-reconnected
				if reaped != 0 || detached != 1 {
					t.Fatalf("close returned (%d, %d)", reaped, detached)
				}
				if newest == nil || pendingReap(sid) != newest {
					t.Fatalf("pending %p, newest %p", pendingReap(sid), newest)
				}
				return
			}
			scheduleWSOrphanReap(ctx, sid, nil, nil)
			old := pendingReap(sid)
			if phase == "cold_resume_claim" {
				session.Set("session_key", sid)
				orphanRaceStale(t, 0)
				var replies []map[string]any
				fake.interrupt = func() {
					replies = append(replies, newResumeCtx(ctx, 1, map[string]any{}, sid).claim("unused", tuigateway.NewSession(map[string]any{})))
				}
				fireReap(old, 0, 0)
				if len(replies) != 1 || rpcCode(replies[0]) != 4009 {
					t.Fatalf("replies %v", replies)
				}
				if session.Get("transport") != tuigateway.DetachedWSTransport() {
					t.Fatalf("transport %v", session.Get("transport"))
				}
				if !truthy(session.Get("_client_gone_interrupt_requested")) {
					t.Fatal("interrupt claim dropped")
				}
				if next := pendingReap(sid); next == nil || next == old {
					t.Fatalf("pending %p after the poll, old %p", next, old)
				}
				return
			}
			var newest *wsReapTimer
			if phase == "before_callback" {
				newest = redetach()
			} else {
				orphanRaceStale(t, 0)
				fake.interrupt = func() {
					session.Pop("_client_gone_interrupt_requested")
					newest = redetach()
				}
			}
			fireReap(old, 0, 0)
			if newest == nil || newest == old || pendingReap(sid) != newest {
				t.Fatalf("pending %p, newest %p, old %p", pendingReap(sid), newest, old)
			}
		})
	}
}

func TestOrphanInterruptClaimClearsWhenSessionLeavesDetachedState(t *testing.T) {
	ctx := context.Background()
	for _, transition := range []string{"retire", "redetach"} {
		t.Run(transition, func(t *testing.T) {
			sid := "writer-rebound"
			session := orphanRaceSession(t, sid, 0, map[string]any{"running": true, "_run_thread": liveTurnThread(t)})
			scheduleWSOrphanReap(ctx, sid, nil, nil)
			fireReap(pendingReap(sid), 0, 0)
			if !truthy(session.Get("_client_gone_interrupt_requested")) {
				t.Fatal("first poll did not claim the interrupt")
			}
			poll := pendingReap(sid)
			peer := &orphanRacePeer{id: 2}
			session.Set("transport", peer)
			if transition == "redetach" {
				reaped, detached := closeSessionsForTransport(ctx, peer, "ws_disconnect")
				if reaped != 0 || detached != 1 {
					t.Fatalf("close returned (%d, %d)", reaped, detached)
				}
				fireReap(pendingReap(sid), 0, 0)
				if !truthy(session.Get("_client_gone_interrupt_requested")) {
					t.Fatal("redetached poll did not claim the interrupt")
				}
				if polls, _ := pyIntOf(session.Get("_client_gone_interrupt_polls")); polls != 1 {
					t.Fatalf("polls %v", session.Get("_client_gone_interrupt_polls"))
				}
				replacement := pendingReap(sid)
				fireReap(poll, 0, 0)
				if pendingReap(sid) != replacement {
					t.Fatal("obsolete poll replaced the newer timer")
				}
				if !truthy(session.Get("_client_gone_interrupt_requested")) {
					t.Fatal("obsolete poll cleared the newer interrupt claim")
				}
				return
			}
			fireReap(poll, 0, 0)
			_, requested := session.Lookup("_client_gone_interrupt_requested")
			if _, polls := session.Lookup("_client_gone_interrupt_polls"); requested || polls {
				t.Fatalf("interrupt flags survived: %v %v", session.Get("_client_gone_interrupt_requested"), session.Get("_client_gone_interrupt_polls"))
			}
			if refusal := reattachRefusal(1, sid, session); refusal != nil {
				t.Fatalf("refusal %v", refusal)
			}
			if pendingReap(sid) != nil {
				t.Fatal("reap still pending")
			}
		})
	}
}

func TestWsOrphanReapRearmsAfterSystemSleep(t *testing.T) {
	ctx := context.Background()
	sid := "slept-through-grace"
	session := orphanRaceSession(t, sid, 600, map[string]any{"running": false})
	scheduleWSOrphanReap(ctx, sid, nil, nil)
	first := pendingReap(sid)
	if first == nil || first.interval != 20 {
		t.Fatalf("first timer %+v", first)
	}
	fireReap(first, 2, 42)
	if tuigateway.SessionRecord(sid) != session {
		t.Fatal("reaped a session the host slept through")
	}
	second := pendingReap(sid)
	if second == nil || second == first || math.Abs(second.interval-18.0) >= 1e-9 {
		t.Fatalf("re-armed timer %+v", second)
	}
	fireReap(second, 18, 18)
	if tuigateway.SessionRecord(sid) != nil {
		t.Fatal("awake remainder did not reap")
	}
}

func TestWsOrphanReapRearmSparesPostWakeReconnect(t *testing.T) {
	ctx := context.Background()
	sid := "woke-and-reconnected"
	session := orphanRaceSession(t, sid, 600, map[string]any{"running": false})
	scheduleWSOrphanReap(ctx, sid, nil, nil)
	first := pendingReap(sid)
	fireReap(first, 2, 42)
	second := pendingReap(sid)
	if second == nil || second == first {
		t.Fatal("slept through the wait without re-arming")
	}
	cancelWSOrphanReap(sid)
	session.Set("transport", &orphanRacePeer{id: 3})
	fireReap(second, 18, 18)
	if tuigateway.SessionRecord(sid) != session {
		t.Fatal("reaped a session that reconnected")
	}
}
