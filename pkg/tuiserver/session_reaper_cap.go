package tuiserver

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Configured live session cap, top level else under gateway, zero when unset or unusable
func maxLiveSessions(ctx context.Context) (limit int) {
	defer func() {
		if recover() != nil {
			limit = 0
		}
	}()
	cfg := loadCfg(ctx)
	raw, present := cfg["max_live_sessions"]
	if !present || raw == nil {
		if gatewayCfg, isDict := cfg["gateway"].(map[string]any); isDict {
			raw = gatewayCfg["max_live_sessions"]
		}
	}
	coerced := activesessions.CoerceMaxConcurrentSessions(raw, "max_live_sessions")
	if coerced == nil || coerced.Sign() == 0 || !coerced.IsInt64() {
		return 0
	}
	return int(coerced.Int64())
}

// Evicts the least recently touched reapable sessions until the live count fits the cap
func enforceSessionCap(ctx context.Context) {
	limit := maxLiveSessions(ctx)
	if limit <= 0 {
		return
	}
	sessionsLock.Lock()
	live := tuigateway.LiveSessions()
	if len(live) <= limit {
		sessionsLock.Unlock()
		return
	}
	var evictable []tuigateway.LiveSession
	for _, entry := range live {
		if sessionIsReapable(ctx, entry.SID, entry.Session) {
			evictable = append(evictable, entry)
		}
	}
	sessionsLock.Unlock()
	sort.SliceStable(evictable, func(i, j int) bool {
		return lastActiveOf(evictable[i].Session) < lastActiveOf(evictable[j].Session)
	})
	for _, entry := range evictable {
		sessionsLock.Lock()
		full := len(tuigateway.LiveSessions()) <= limit
		sessionsLock.Unlock()
		if full {
			break
		}
		sid := entry.SID
		closeSessionByID(ctx, sid, "lru_evict", func(session *tuigateway.Session) bool { return sessionIsReapable(ctx, sid, session) })
	}
}

// Float of a session's last activity stamp, zero when unset
func lastActiveOf(session *tuigateway.Session) float64 {
	value := session.Get("last_active")
	if !truthy(value) {
		return 0
	}
	stamp, err := pyint.Float(value)
	panicIf(err)
	return stamp
}

// Runs the LRU cap sweep off the response path, eviction may close agents
func scheduleSessionCapEnforcement(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	reaperDaemonTimer(0.1, func() { enforceSessionCap(ctx) }, "session cap enforcement failed", "debug")
}
