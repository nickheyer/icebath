package tuiserver

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Close grace a session turn gets to settle before teardown, and the ws orphan reap clocks
var (
	turnSettleBeforeCloseSeconds  = 5.0
	wsOrphanReapSleepSlackS       = 0.5
	wsOrphanInterruptReapPollS    = 1.0
	wsOrphanInterruptReapMaxPolls = 60
	wsOrphanActivityStaleS        = sync.OnceValue(func() float64 {
		return wsOrphanSetting(context.Background(), "ICEBATH_TUI_WS_ORPHAN_ACTIVITY_STALE_S", "ws_orphan_activity_stale_s", 600.0)
	})
)

// Applies the session.interrupt contract to one claimed session, true when the compute host control channel was used
func interruptSessionTurn(ctx context.Context, sid string, session *tuigateway.Session, requestID string, orphan bool) bool {
	useComputeHost := sessionUsesComputeHost(ctx, session, nil)
	compressing := truthy(session.Get("_manual_compress_active"))
	shouldInterrupt := truthy(session.Get("running")) && !compressing
	runThreadAlive := false
	if useComputeHost {
		if shouldInterrupt || truthy(session.Get("_compute_host_active")) {
			supervisor, err := getComputeHostSupervisor(ctx, nil)
			if err != nil {
				panic(err)
			}
			if err := supervisor.Interrupt(sid, requestID); err != nil {
				panic(err)
			}
		}
	} else if rt, ok := session.Get("_run_thread").(*memory.ContextThread); ok && rt != nil {
		runThreadAlive = rt.Alive()
	}
	unlock := historyLockOf(session)
	session.Set("_turn_cancel_requested", true)
	session.Set("queued_prompt", nil)
	session.Pop("queued_prompts")
	generation, _ := pyIntOf(session.Get("_queued_prompt_generation"))
	session.Set("_queued_prompt_generation", generation+1)
	unlock()
	if shouldInterrupt {
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					logger().Debug("agent_loop_stopped hook dispatch failed", "exc_info", failureText(failure))
				}
			}()
			scoped := sessionProfileRuntimeScope(ctx, session, false)
			plugins.InvokeHook(scoped, "agent_loop_stopped", map[string]any{"session_key": strOr(session.Get("session_key")),
				"platform": "tui", "reason": "user_stop", "invalidation_reason": "session_interrupt"})
		}()
	}
	if !useComputeHost {
		if shouldInterrupt {
			agent.RequestHardInterruptOf(session.Get("agent"), nil, nil)
		}
		if !runThreadAlive && !compressing {
			unlock := historyLockOf(session)
			if truthy(session.Get("running")) {
				session.Set("running", false)
				clearInflightTurn(session)
			}
			unlock()
		}
	}
	if a := sessionAgent(session); a != nil {
		suppress(func() { a.CancelBackgroundReview("session interrupted", "session interrupted") })
	}
	clearPending(&sid)
	suppress(func() {
		reason := "interrupt"
		if orphan {
			reason = "ws_orphan_reap"
		}
		announceCancelledGatewayApprovals(session, reason, sid)
		tools.ResolveGatewayApproval(strOr(session.Get("session_key")), "deny", true, "", "")
	})
	return useComputeHost
}

// Interrupts an isolated child turn and gives it the close grace, keeping its real lease when it never settled
func settleIsolatedTurnBeforeClose(ctx context.Context, session *tuigateway.Session) {
	if !truthy(session.Get("_compute_host_turn_id")) || !sessionUsesComputeHost(ctx, session, nil) {
		return
	}
	suppress(func() { interruptSessionTurn(ctx, lifecycleOwnSID(session, ""), session, "", false) })
	deadline := pythread.Monotonic() + turnSettleBeforeCloseSeconds
	for truthy(session.Get("_compute_host_turn_id")) && pythread.Monotonic() < deadline {
		time.Sleep(50 * time.Millisecond)
	}
	unlock := historyLockOf(session)
	if !truthy(session.Get("_compute_host_turn_id")) {
		unlock()
		return
	}
	popped, _ := session.Pop("active_session_lease")
	lease, _ := popped.(*activesessions.Lease)
	if lease == nil {
		unlock()
		return
	}
	session.Set("_deferred_active_session_lease", lease)
	deferredActiveSessionLeases.Lock()
	deferredActiveSessionLeases.leases[lease.LeaseID] = lease
	deferredActiveSessionLeases.ages[lease.LeaseID] = nowSeconds()
	deferredActiveSessionLeases.Unlock()
	unlock()
	logger().Warn(fmt.Sprintf("isolated turn still live after %.1fs close grace; holding lease for %s until the child settles",
		turnSettleBeforeCloseSeconds, strOr(session.Get("session_key"))))
}

// Finishes a close the caller claimed by popping the session, waiting out its turn first
func teardownPoppedSession(ctx context.Context, session *tuigateway.Session, endReason string) bool {
	if session == nil {
		return false
	}
	runThread, _ := session.Get("_run_thread").(*memory.ContextThread)
	if endReason != "tui_shutdown" && runThread != nil && runThread != currentRunThread(ctx) {
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					logger().Debug("failed waiting for session turn thread", "exc_info", failureText(failure))
				}
			}()
			if runThread.Alive() {
				runThread.Join(time.Duration(turnSettleBeforeCloseSeconds * float64(time.Second)))
			}
			if runThread.Alive() {
				logger().Warn(fmt.Sprintf("session turn thread still alive after %.1fs teardown grace", turnSettleBeforeCloseSeconds))
			}
		}()
	}
	if endReason != "tui_shutdown" {
		settleIsolatedTurnBeforeClose(ctx, session)
	}
	defer tuigateway.ForgetSession(strOr(session.Get("_sid")))
	teardownSessionFn(ctx, session, endReason)
	return true
}

// Finalizer every popped session goes through, the real one swapped only where a hermes case replaces the finalize
var teardownSessionFn func(ctx context.Context, session *tuigateway.Session, endReason string)

func init() { teardownSessionFn = teardownSession }

// Idempotent teardown funnel, the predicate revalidating the live record right before the claim
func closeSessionByID(ctx context.Context, sid, endReason string, predicate func(*tuigateway.Session) bool) bool {
	sessionsLock.Lock()
	current := tuigateway.SessionRecord(sid)
	if predicate != nil && (current == nil || !predicate(current)) {
		sessionsLock.Unlock()
		return false
	}
	session := popSessionByID(ctx, sid)
	sessionsLock.Unlock()
	return teardownPoppedSession(ctx, session, endReason)
}

// Activity clock surface the orphan reaper reads on whatever agent the session holds
type activitySummarizer interface{ GetActivitySummary() map[string]any }

var _ activitySummarizer = (*agent.AIAgent)(nil)

// Whether a detached running turn's activity clock is still fresh, so the reaper keeps healthy work alive
func wsOrphanTurnActivityIsFresh(session *tuigateway.Session) (fresh bool) {
	stale := wsOrphanActivityStaleS()
	if stale <= 0 {
		return false
	}
	if truthy(session.Get("_compute_host_turn_id")) {
		unlock := historyLockOf(session)
		defer unlock()
		stamp, isInt := int64Of(session.Get("_compute_host_activity_ns"))
		if !truthy(session.Get("running")) || !isInt {
			return false
		}
		age := float64(monotonicNS()-stamp) / 1e9
		return 0 <= age && age < stale
	}
	a, _ := session.Get("agent").(activitySummarizer)
	if a == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			fresh = false
		}
	}()
	elapsed, present := a.GetActivitySummary()["seconds_since_activity"]
	if !present || elapsed == nil {
		return false
	}
	seconds, err := pyFloat(elapsed)
	return err == nil && seconds < stale
}

// Reaps a session after the grace window when it is still orphaned, re-arming across host sleep
func scheduleWSOrphanReap(ctx context.Context, sid string, delayS *float64, expected *wsReapTimer) {
	grace := wsOrphanReapGraceS()
	if grace <= 0 {
		return
	}
	graceS := grace
	if delayS != nil {
		graceS = max(0, *delayS)
	}
	var timer *wsReapTimer
	reap := func(monotonic, wall float64) {
		slept := (wall - timer.armedWall) - (monotonic - timer.armedMonotonic)
		if slept > wsOrphanReapSleepSlackS {
			if rearm := max(0, graceS-(monotonic-timer.armedMonotonic)); rearm > 0 {
				scheduleWSOrphanReap(ctx, sid, &rearm, timer)
				return
			}
		}
		var rescheduleDelay *float64
		var interruptSession, session *tuigateway.Session
		claim := func() bool {
			sessionResumeLock.Lock()
			defer sessionResumeLock.Unlock()
			sessionsLock.Lock()
			defer sessionsLock.Unlock()
			pendingWSReaps.Lock()
			current := pendingWSReaps.timers[sid]
			pendingWSReaps.Unlock()
			if current != timer {
				return false
			}
			record := tuigateway.SessionRecord(sid)
			dropPending := func() {
				pendingWSReaps.Lock()
				if pendingWSReaps.timers[sid] == timer {
					delete(pendingWSReaps.timers, sid)
				}
				pendingWSReaps.Unlock()
			}
			if record == nil {
				dropPending()
				return false
			}
			if !wsSessionIsDetached(record) {
				record.Pop("_client_gone_interrupt_requested")
				record.Pop("_client_gone_interrupt_polls")
				dropPending()
				return false
			}
			interruptRequested := truthy(record.Get("_client_gone_interrupt_requested"))
			running := truthy(record.Get("running"))
			switch {
			case sessionHasActiveDelegations(ctx, sid, record) || (!running && !interruptRequested &&
				(sessionOwnsLiveWakeupSchedule(ctx, record) || sessionOwnsPendingProcessNotification(ctx, record))):
				rescheduleDelay = &grace
			case !running:
				session = popSessionByID(ctx, sid)
			case !interruptRequested && wsOrphanTurnActivityIsFresh(record):
				logger().Debug(fmt.Sprintf("client_gone sid=%s action=defer (turn activity fresh; stale threshold %.0fs)", sid, wsOrphanActivityStaleS()))
				rescheduleDelay = &grace
			default:
				prior, _ := pyIntOf(record.Get("_client_gone_interrupt_polls"))
				polls := prior + 1
				record.Set("_client_gone_interrupt_polls", polls)
				if polls > wsOrphanInterruptReapMaxPolls {
					logger().Error(fmt.Sprintf("client_gone sid=%s: turn did not settle after %d interrupt polls (%.0fs) — force-reaping detached session",
						sid, polls-1, float64(polls-1)*wsOrphanInterruptReapPollS))
					session = popSessionByID(ctx, sid)
				} else {
					if !interruptRequested {
						record.Set("_client_gone_interrupt_requested", true)
						interruptSession = record
					}
					rescheduleDelay = &wsOrphanInterruptReapPollS
				}
			}
			if rescheduleDelay == nil {
				dropPending()
			}
			return true
		}
		if !claim() {
			return
		}
		if interruptSession != nil {
			func() {
				defer func() {
					if failure := recover(); failure != nil {
						logger().Error(fmt.Sprintf("client_gone interrupt failed sid=%s", sid), "exc_info", failureText(failure))
						sessionsLock.Lock()
						pendingWSReaps.Lock()
						stillPending := pendingWSReaps.timers[sid] == timer
						pendingWSReaps.Unlock()
						if tuigateway.SessionRecord(sid) == interruptSession && stillPending {
							interruptSession.Pop("_client_gone_interrupt_requested")
						}
						sessionsLock.Unlock()
					}
				}()
				isolated := interruptSessionTurn(ctx, sid, interruptSession, "client-gone-"+sid, true)
				logger().Info(fmt.Sprintf("client_gone sid=%s action=interrupt turn_isolation=%s", sid, pyBoolText(isolated)))
			}()
		}
		if rescheduleDelay != nil {
			scheduleWSOrphanReap(ctx, sid, rescheduleDelay, timer)
			return
		}
		if session != nil && truthy(session.Get("_client_gone_interrupt_requested")) {
			logger().Info(fmt.Sprintf("client_gone sid=%s action=reap", sid))
		}
		teardownPoppedSession(ctx, session, "ws_orphan_reap")
	}
	sessionsLock.Lock()
	pendingWSReaps.Lock()
	if expected != nil && pendingWSReaps.timers[sid] != expected {
		pendingWSReaps.Unlock()
		sessionsLock.Unlock()
		return
	}
	prior := pendingWSReaps.timers[sid]
	timer = newWSReapTimer(graceS, reap)
	pendingWSReaps.timers[sid] = timer
	pendingWSReaps.Unlock()
	sessionsLock.Unlock()
	if prior != nil {
		suppress(prior.cancel)
	}
	timer.start()
}

// System wide monotonic clock in nanoseconds, comparable across local processes like perf_counter_ns
func monotonicNS() int64 { return int64(pythread.Monotonic() * 1e9) }

// Integer value of a slot, false for anything but an int
func int64Of(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	}
	return 0, false
}

// Python bool text of a flag
func pyBoolText(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

// Attaches a live peer beside the existing subscribers, re-arming the reap when that peer already closed
func rebindLiveTransport(ctx context.Context, sid string, session *tuigateway.Session, transport tuigateway.Transport) {
	detached := tuigateway.DetachedWSTransport()
	if transport != detached && tuigateway.TransportIsDead(transport) {
		sessionsLock.Lock()
		pendingWSReaps.Lock()
		_, pending := pendingWSReaps.timers[sid]
		pendingWSReaps.Unlock()
		if wsSessionIsDetached(session) && !pending {
			scheduleWSOrphanReap(ctx, sid, nil, nil)
		}
		sessionsLock.Unlock()
		return
	}
	tuigateway.AttachSessionTransport(session, transport)
	tuigateway.SetSessionViewer(session, transport, nowSeconds())
	if transport != detached {
		cancelWSOrphanReap(sid)
	}
}

// Reaps the close on disconnect sessions a departing transport leaves clientless and parks the rest for the orphan reaper
func closeSessionsForTransport(ctx context.Context, transport tuigateway.Transport, endReason string) (int, int) {
	clientless := tuigateway.DetachTransportFromSessions(transport)
	reaped, detached := 0, 0
	for _, entry := range clientless {
		sid, session := entry.SID, entry.Session
		var claimedForTeardown *tuigateway.Session
		shouldScheduleReap := false
		skip := func() bool {
			sessionResumeLock.Lock()
			defer sessionResumeLock.Unlock()
			sessionsLock.Lock()
			defer sessionsLock.Unlock()
			current := tuigateway.SessionRecord(sid)
			if current != session {
				return true
			}
			tuigateway.DropSessionViewer(current, transport)
			if tuigateway.SessionHasLiveTransportExcluding(current, transport) {
				return true
			}
			if truthy(current.Get("close_on_disconnect")) {
				claimedForTeardown = popSessionByID(ctx, sid)
				return false
			}
			tuigateway.DropSessionViewer(current, transport)
			viewers := tuigateway.SessionViewersByAge(current)
			live := slices.DeleteFunc(viewers, func(vt tuigateway.Transport) bool { return tuigateway.TransportIsDead(vt) })
			if len(live) > 0 {
				rebindLiveTransport(ctx, sid, current, live[len(live)-1])
				return false
			}
			current.Set("transport", tuigateway.DetachedWSTransport())
			current.Pop("_client_gone_interrupt_requested")
			current.Pop("_client_gone_interrupt_polls")
			shouldScheduleReap = true
			suppress(func() { scheduleWSOrphanReap(ctx, sid, nil, nil) })
			return false
		}()
		if skip {
			continue
		}
		if claimedForTeardown != nil {
			if teardownPoppedSession(ctx, claimedForTeardown, endReason) {
				reaped++
			}
		} else if shouldScheduleReap {
			detached++
		}
	}
	return reaped, detached
}
