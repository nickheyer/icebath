package tuiserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Holds process admission and the history lock until the running claim is visible, returning whether work was admitted
func sessionTurnAdmission(session *tuigateway.Session, body func(admitted bool)) {
	admitted, done := provider.Retirement.Work()
	defer done()
	unlock := lockHistory(session)
	defer unlock()
	body(admitted)
}

// Context key naming the session work thread a call runs on
type runThreadKey struct{}

// Session work thread the context runs on, nil off every session thread
func currentRunThread(ctx context.Context) *memory.ContextThread {
	thread, _ := ctx.Value(runThreadKey{}).(*memory.ContextThread)
	return thread
}

// Starts target on a context thread reserved in the retirement fence until it unwinds, nil when the fence refuses
func startSessionWork(ctx context.Context, target func(ctx context.Context), name string, session *tuigateway.Session) *memory.ContextThread {
	if !provider.Retirement.Acquire() {
		return nil
	}
	started := false
	defer func() {
		if !started {
			provider.Retirement.Release()
		}
	}()
	var thread *memory.ContextThread
	thread = memory.SpawnContextThread(ctx, name, func(ctx context.Context) {
		defer provider.Retirement.Release()
		target(context.WithValue(ctx, runThreadKey{}, thread))
	})
	if session != nil {
		session.Set("_run_thread", thread)
	}
	thread.Start()
	started = true
	return thread
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

// Adopts the parent slot as an inert token on a compute host child whose frame vouches for this id
func installBorrowedLease(sid string, session *tuigateway.Session, frame map[string]any) {
	vouch, isDict := frame["active_session_lease"].(map[string]any)
	if !isDict || session.Get("active_session_lease") != nil {
		return
	}
	key := strOr(session.Get("session_key"))
	if key == "" || strOr(vouch["session_id"]) != key {
		return
	}
	leaseID := strOr(vouch["lease_id"])
	if leaseID == "" {
		leaseID = sid
	}
	surface := strOr(frame["source"])
	if surface == "" {
		surface = "desktop"
	}
	session.Set("active_session_lease", activesessions.NewBorrowedLease(fmt.Sprintf("borrowed:%s", leaseID), key, surface))
}

// Session source of a hosted room member, whose slot is held only while a turn is in flight
const roomSessionSource = "bot_room"

// Live leases deferred past a close until the isolated turn of their session settles, with their ages
var deferredActiveSessionLeases = struct {
	sync.Mutex
	leases map[string]*activesessions.Lease
	ages   map[string]float64
}{leases: map[string]*activesessions.Lease{}, ages: map[string]float64{}}

// Stores the lease on the session, which owns its row again
func attachLease(session *tuigateway.Session, lease *activesessions.Lease) {
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
func releaseActiveSessionSlot(session *tuigateway.Session) bool {
	if session == nil {
		return true
	}
	lease, _ := session.Get("active_session_lease").(*activesessions.Lease)
	if lease == nil {
		return true
	}
	attempts := 1
	if lease.TrackLiveness {
		attempts = 3
	}
	if err := leaseRetry(attempts, lease.Release); err != nil {
		logger().Warn(fmt.Sprintf("Failed to release active session slot: %s", err))
		return false
	}
	if !(lease.Released() || !lease.Enabled) {
		return false
	}
	if current, _ := session.Get("active_session_lease").(*activesessions.Lease); current == lease {
		session.Pop("active_session_lease")
	}
	return true
}

// Releases the slot of a hosted room member session at the end of its turn
func releaseHostedRoomTurnSlot(session *tuigateway.Session) {
	if sessionSource(session) == roomSessionSource {
		releaseActiveSessionSlot(session)
	}
}

// Lease ids still backed by live records of this process plus the deferred ones, sparing exclude
func ownLiveLeaseIDs(exclude *activesessions.Lease) map[string]bool {
	ids := map[string]bool{}
	for _, live := range tuigateway.LiveSessions() {
		if lease, _ := live.Session.Get("active_session_lease").(*activesessions.Lease); lease != nil && lease != exclude {
			ids[lease.LeaseID] = true
		}
	}
	deferredActiveSessionLeases.Lock()
	defer deferredActiveSessionLeases.Unlock()
	for id := range deferredActiveSessionLeases.leases {
		ids[id] = true
	}
	return ids
}

// Ceiling past which a deferred lease whose settlement never arrived is force released
var deferredActiveSessionLeaseTTLSeconds = 1800.0

// Force releases deferred leases whose compute host settlement never arrived and returns how many
func reapStaleDeferredLeases(now float64) int {
	deferredActiveSessionLeases.Lock()
	stale := []string{}
	for leaseID, deferredAt := range deferredActiveSessionLeases.ages {
		if now-deferredAt > deferredActiveSessionLeaseTTLSeconds {
			stale = append(stale, leaseID)
		}
	}
	deferredActiveSessionLeases.Unlock()
	reaped := 0
	for _, leaseID := range stale {
		deferredActiveSessionLeases.Lock()
		delete(deferredActiveSessionLeases.ages, leaseID)
		lease := deferredActiveSessionLeases.leases[leaseID]
		delete(deferredActiveSessionLeases.leases, leaseID)
		deferredActiveSessionLeases.Unlock()
		if lease == nil {
			continue
		}
		if err := leaseRetry(3, lease.Release); err != nil {
			logger().Warn(fmt.Sprintf("Failed to force-release stale deferred active session lease %s: %s", leaseID, err))
			continue
		}
		logger().Warn(fmt.Sprintf("Force-released deferred active session lease %s held past %.0fs without a "+
			"compute-host settlement (zombie concurrency slot, #62823)", leaseID, deferredActiveSessionLeaseTTLSeconds))
		reaped++
	}
	return reaped
}

// Releases the lease a closing session deferred for its isolated turn, nothing when it deferred none
func releaseDeferredActiveSessionLease(session *tuigateway.Session) {
	value, _ := session.Pop("_deferred_active_session_lease")
	lease, _ := value.(*activesessions.Lease)
	if lease == nil {
		return
	}
	deferredActiveSessionLeases.Lock()
	delete(deferredActiveSessionLeases.leases, lease.LeaseID)
	delete(deferredActiveSessionLeases.ages, lease.LeaseID)
	deferredActiveSessionLeases.Unlock()
	if err := leaseRetry(3, lease.Release); err != nil {
		logger().Warn(fmt.Sprintf("Failed to release deferred active session slot: %s", err))
	}
}

// Live UI sid of a session, the hint, else its stamped sid, else a registry scan
func lifecycleOwnSID(session *tuigateway.Session, sidHint string) string {
	ownSID := sidHint
	if ownSID == "" {
		ownSID = strOr(session.Get("_sid"))
	}
	if ownSID == "" {
		for _, live := range tuigateway.LiveSessions() {
			if live.Session == session {
				return live.SID
			}
		}
	}
	return ownSID
}

// Latest activity of each closed session's resolved profile home, kept after close and reap
var closedSessionActivity = struct {
	sync.Mutex
	byHome map[string]float64
}{byHome: map[string]float64{}}

// Detaches one live session from the registry as the teardown claim, stamping its closing state and sid
func popSessionByID(ctx context.Context, sid string) *tuigateway.Session {
	session := tuigateway.PopSessionRecord(sid)
	if session == nil {
		return nil
	}
	owner := strOr(session.Get("profile_home"))
	if owner == "" {
		owner = home.Dir(ctx)
	}
	resolved := resolveLax(owner)
	lastActive := floatOr(session.Get("last_active"), 0)
	if truthy(session.Get("running")) {
		lastActive = nowSeconds()
	}
	closedSessionActivity.Lock()
	closedSessionActivity.byHome[resolved] = max(closedSessionActivity.byHome[resolved], lastActive)
	closedSessionActivity.Unlock()
	session.Set("_closing", true)
	session.Set("_sid", sid)
	return session
}

// Whether a live session is still bound to the parked sink a disconnected WebSocket client left
func wsSessionIsDetached(session *tuigateway.Session) bool {
	if session == nil || truthy(session.Get("_finalized")) {
		return false
	}
	transport, _ := session.Get("transport").(tuigateway.Transport)
	return transport != nil && transport == tuigateway.DetachedWSTransport()
}

// Whether a parked WebSocket session is idle
func wsSessionIsOrphaned(session *tuigateway.Session) bool {
	return wsSessionIsDetached(session) && !truthy(session.Get("running"))
}

// Pending WebSocket orphan reap timers by session id
var pendingWSReaps = struct {
	sync.Mutex
	timers map[string]*time.Timer
}{timers: map[string]*time.Timer{}}

// Cancels the pending orphan reap of a session whose client came back
func cancelWSOrphanReap(sid string) {
	pendingWSReaps.Lock()
	timer := pendingWSReaps.timers[sid]
	delete(pendingWSReaps.timers, sid)
	pendingWSReaps.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

// Error frame refusing a reattach to a stale session or one whose disconnect interrupt still settles, nil to proceed
func reattachRefusal(rid any, sid string, session *tuigateway.Session) map[string]any {
	if tuigateway.SessionRecord(sid) != session {
		return rpcErr(rid, 4007, "session no longer live; retry resume", nil)
	}
	if truthy(session.Get("_client_gone_interrupt_requested")) {
		return rpcErr(rid, 4009, "session disconnect interrupt settling", nil)
	}
	return nil
}
