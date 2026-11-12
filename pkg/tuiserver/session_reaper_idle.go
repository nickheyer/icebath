package tuiserver

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Seconds between two scans of the idle reaper
const reaperScanS = 300.0

// Seconds between incremental flushes of one idle session, zero turning the periodic flush off
var incrementalFlushIntervalS = math.Max(0, envFloat("ICEBATH_TUI_SESSION_FLUSH_INTERVAL_S", reaperScanS))

// Float of a session slot, zero when unset, a value float cannot take raising
func sessionFloat(session *tuigateway.Session, key string) float64 {
	value := session.Get(key)
	if !truthy(value) {
		return 0
	}
	stamp, err := pyint.Float(value)
	panicIf(err)
	return stamp
}

// Flushes each idle session at most once per interval and returns how many wrote, running turns persisting their own
func flushDirtySessions(now *float64) int {
	if incrementalFlushIntervalS <= 0 {
		return 0
	}
	at := pythread.Monotonic()
	if now != nil {
		at = *now
	}
	flushed := 0
	for _, session := range reaperSessionSnapshot() {
		if session == nil || truthy(session.Get("running")) {
			continue
		}
		last := sessionFloat(session, "_last_incremental_flush")
		if last != 0 && at-last < incrementalFlushIntervalS {
			continue
		}
		if flushSessionMessages(session) {
			flushed++
		}
		session.Set("_last_incremental_flush", at)
	}
	return flushed
}

// Whether a session is idle and older past the TTL and passes the reap exemptions, the clock checked first
func sessionIsEvictable(ctx context.Context, sid string, session *tuigateway.Session, now float64) bool {
	lastActive := sessionFloat(session, "last_active")
	createdAt := sessionFloat(session, "created_at")
	if now-lastActive <= sessionTTLS || now-createdAt <= sessionTTLS {
		return false
	}
	return sessionIsReapable(ctx, sid, session)
}

// Flushes idle transcripts, closes sessions idle past the TTL, rearms lost reaps, caps the count and trims when quiet
func reapIdleSessions(ctx context.Context) {
	now := nowSeconds()
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				logger().Debug("periodic incremental session flush failed", "exc_info", failureText(failure))
			}
		}()
		flushDirtySessions(nil)
	}()
	var victims []string
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		for _, live := range tuigateway.LiveSessions() {
			if sessionIsEvictable(ctx, live.SID, live.Session, now) {
				victims = append(victims, live.SID)
			}
		}
	}()
	for _, sid := range victims {
		closeSessionByID(ctx, sid, "idle_timeout", func(session *tuigateway.Session) bool {
			return sessionIsEvictable(ctx, sid, session, nowSeconds())
		})
	}
	repairMissingWSOrphanReaps(ctx)
	enforceSessionCap(ctx)
	reclaimOrphanedLeases()
	if !sessionsQuiescent("") {
		logger().Debug("idle reaper periodic trim deferred: a session is busy or attached")
		return
	}
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug(fmt.Sprintf("idle reaper memory trim failed: %s: %s", pyrepr.TypeName(failure), failureText(failure)))
		}
	}()
	memory.TrimMemoryWhenIdle(ctx, configEnv(ctx), "idle reaper periodic trim", agent.ReviewQueueQuietClock{})
}

// Rearms detached sessions whose disconnect path lost its teardown timer through the normal grace path
func repairMissingWSOrphanReaps(ctx context.Context) {
	if wsOrphanReapGraceS() <= 0 {
		return
	}
	var closed []tuigateway.Transport
	func() {
		sessionsLock.Lock()
		defer sessionsLock.Unlock()
		for _, live := range tuigateway.LiveSessions() {
			transport, _ := live.Session.Get("transport").(tuigateway.Transport)
			if transport != tuigateway.DetachedWSTransport() && tuigateway.TransportIsDead(transport) {
				closed = append(closed, transport)
			}
		}
	}()
	for _, transport := range closed {
		closeSessionsForTransport(ctx, transport, "ws_disconnect")
	}
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	var missing []string
	for _, live := range tuigateway.LiveSessions() {
		pendingWSReaps.Lock()
		_, pending := pendingWSReaps.timers[live.SID]
		pendingWSReaps.Unlock()
		if wsSessionIsDetached(live.Session) && !pending {
			missing = append(missing, live.SID)
		}
	}
	for _, sid := range missing {
		scheduleWSOrphanReap(ctx, sid, nil, nil)
	}
}

// Hands the lease registry the ids this process still owns so it drops the rest
func reclaimOrphanedLeases() {
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				logger().Debug("stale deferred lease sweep failed", "exc_info", failureText(failure))
			}
		}()
		reapStaleDeferredLeases(nowSeconds())
	}()
	defer func() {
		if failure := recover(); failure != nil {
			logger().Debug("orphaned lease reclaim failed", "exc_info", failureText(failure))
		}
	}()
	dropped, err := activesessions.ReleaseOrphanedLeases(ownLiveLeaseIDs(nil))
	if err != nil {
		logger().Debug("orphaned lease reclaim failed", "exc_info", err.Error())
		return
	}
	if dropped > 0 {
		logger().Info(fmt.Sprintf("Reclaimed %d orphaned active-session lease(s)", dropped))
	}
}

// Starts the daemon loop that runs one reap after every scan interval, a failed scan never ending it
func startIdleReaper() {
	go func() {
		defer threadExceptHook("tui-idle-reaper")
		ctx := context.Background()
		for {
			time.Sleep(time.Duration(reaperScanS * float64(time.Second)))
			suppress(func() { reapIdleSessions(ctx) })
		}
	}()
}
