package tuiserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Runs body under the session history lock so its running claim is visible before the lock drops
func underHistoryLock(session *tuihost.Session, body func()) {
	unlock := lockHistory(session)
	defer unlock()
	body()
}

// Context key naming the session work thread a call runs on
type runThreadKey struct{}

// Session work thread the context runs on, nil off every session thread
func currentRunThread(ctx context.Context) *memory.ContextThread {
	thread, _ := ctx.Value(runThreadKey{}).(*memory.ContextThread)
	return thread
}

// Starts target on a context thread, recorded as the session's run thread when a session is given
func startSessionWork(ctx context.Context, target func(ctx context.Context), name string, session *tuihost.Session) {
	var thread *memory.ContextThread
	thread = memory.SpawnContextThread(ctx, name, func(ctx context.Context) {
		defer threadExceptHook(name)
		target(context.WithValue(ctx, runThreadKey{}, thread))
	})
	if session != nil {
		session.Set("_run_thread", thread)
	}
	thread.Start()
}

// Fires a session lifecycle hook with CLI parity, returning the on_session_finalize user messages
func notifySessionBoundary(ctx context.Context, eventType string, sessionID *string, platform string) (messages []string) {
	messages = []string{}
	defer func() {
		if recover() != nil {
			messages = []string{}
		}
	}()
	var sid any
	if sessionID != nil {
		sid = *sessionID
	}
	kwargs := map[string]any{"session_id": sid, "platform": resolveAgentPlatform(platform)}
	if eventType == "on_session_finalize" {
		return agent.SessionEndMessages(agent.FinalizeSession(ctx, kwargs))
	}
	agent.LifecycleInvokeHook(ctx, eventType, kwargs)
	return []string{}
}

// Session source of a hosted room member, whose slot is held only while a turn is in flight
const roomSessionSource = "bot_room"

// Stores the lease on the session, which owns its row again
func attachLease(session *tuihost.Session, lease *activesessions.Lease) {
	session.Set("active_session_lease", lease)
	session.Pop("_lease_taken_over")
}

// Calls fn up to attempts times with a 50ms growing backoff, the last error when every try failed
func leaseRetry(attempts int, fn func() error) error {
	var last error
	for attempt := range attempts {
		err := func() (err error) {
			defer func() {
				if failure := recover(); failure != nil {
					err = fmt.Errorf("%s", failureText(failure))
				}
			}()
			return fn()
		}()
		if err == nil {
			return nil
		}
		last = err
		if attempt+1 < attempts {
			time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
		}
	}
	return last
}

// Releases the session lease, three tries for a liveness tracked one, true when it is released
func releaseActiveSessionSlot(session *tuihost.Session) bool {
	if session == nil {
		return true
	}
	lease, _ := session.Get("active_session_lease").(slotLease)
	if real, isReal := lease.(*activesessions.Lease); lease == nil || isReal && real == nil {
		return true
	}
	attempts := 1
	if lease.TracksLiveness() {
		attempts = 3
	}
	if err := leaseRetry(attempts, lease.Release); err != nil {
		pkgLog().Warn(fmt.Sprintf("Failed to release active session slot: %s", err))
		return false
	}
	if !(lease.Released() || !lease.IsEnabled()) {
		return false
	}
	if session.Get("active_session_lease") == any(lease) {
		session.Pop("active_session_lease")
	}
	return true
}

// Lease surface the slot release drives on whatever object the session holds
type slotLease interface {
	Release() error
	Released() bool
	IsEnabled() bool
	TracksLiveness() bool
}

var _ slotLease = (*activesessions.Lease)(nil)

// Releases the slot of a hosted room member session at the end of its turn
func releaseHostedRoomTurnSlot(session *tuihost.Session) {
	if sessionSource(session) == roomSessionSource {
		releaseActiveSessionSlot(session)
	}
}

// Lease ids still backed by live records of this process, sparing exclude
func ownLiveLeaseIDs(exclude *activesessions.Lease) map[string]bool {
	ids := map[string]bool{}
	for _, live := range tuihost.LiveSessions() {
		if lease, _ := live.Session.Get("active_session_lease").(*activesessions.Lease); lease != nil && lease != exclude {
			ids[lease.LeaseID] = true
		}
	}
	return ids
}

// Live UI sid of a session, the hint, else its stamped sid, else a registry scan
func lifecycleOwnSID(session *tuihost.Session, sidHint string) string {
	ownSID := sidHint
	if ownSID == "" {
		ownSID = strOr(session.Get("_sid"))
	}
	if ownSID == "" {
		for _, live := range tuihost.LiveSessions() {
			if live.Session == session {
				return live.SID
			}
		}
	}
	return ownSID
}

// Detaches one live session from the registry as the teardown claim, stamping its closing state and sid
func popSessionByID(sid string) *tuihost.Session {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	session := tuihost.PopSessionRecord(sid)
	if session == nil {
		return nil
	}
	session.Set("_closing", true)
	session.Set("_sid", sid)
	return session
}

// Whether a live session is still bound to the parked sink a disconnected WebSocket client left
func wsSessionIsDetached(session *tuihost.Session) bool {
	if session == nil || truthy(session.Get("_finalized")) {
		return false
	}
	transport, _ := session.Get("transport").(tuihost.Transport)
	return transport != nil && transport == tuihost.DetachedWSTransport()
}

// Whether a parked WebSocket session is idle
func wsSessionIsOrphaned(session *tuihost.Session) bool {
	return wsSessionIsDetached(session) && !truthy(session.Get("running"))
}

// Pending WebSocket orphan reap timers by session id
var pendingWSReaps = struct {
	sync.Mutex
	timers map[string]*wsReapTimer
}{timers: map[string]*wsReapTimer{}}

// One orphan reap countdown with the awake and wall clocks sampled when it was armed
type wsReapTimer struct {
	interval       float64
	armedMonotonic float64
	armedWall      float64
	callback       func(monotonic, wall float64)
	timer          *time.Timer
}

// Unstarted countdown of interval seconds whose expiry runs callback with the clocks read at that moment
func newWSReapTimer(interval float64, callback func(monotonic, wall float64)) *wsReapTimer {
	r := &wsReapTimer{interval: interval, armedMonotonic: pythread.Monotonic(), armedWall: nowSeconds(), callback: callback}
	r.timer = time.AfterFunc(time.Hour*24*365*100, func() { r.callback(pythread.Monotonic(), nowSeconds()) })
	r.timer.Stop()
	return r
}

// Starts the countdown of its interval
func (r *wsReapTimer) start() { r.timer.Reset(time.Duration(r.interval * float64(time.Second))) }

// Stops a countdown whose callback has not been dispatched yet
func (r *wsReapTimer) cancel() { r.timer.Stop() }

// Cancels the pending orphan reap of a session whose client came back
func cancelWSOrphanReap(sid string) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	pendingWSReaps.Lock()
	timer := pendingWSReaps.timers[sid]
	delete(pendingWSReaps.timers, sid)
	pendingWSReaps.Unlock()
	if timer != nil {
		timer.cancel()
	}
}

// Error frame refusing a reattach to a stale session or one whose disconnect interrupt still settles, nil to proceed
func reattachRefusal(rid any, sid string, session *tuihost.Session) map[string]any {
	if tuihost.SessionRecord(sid) != session {
		return rpcErr(rid, 4007, "session no longer live; retry resume", nil)
	}
	if truthy(session.Get("_client_gone_interrupt_requested")) {
		return rpcErr(rid, 4009, "session disconnect interrupt settling", nil)
	}
	return nil
}
