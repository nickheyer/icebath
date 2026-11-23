package tuiserver

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Configured live session cap, zero or less meaning no cap
func maxLiveSessions(ctx context.Context) int {
	return settings.MaxLiveSessions.Get(ctx)
}

// Evicts the least recently touched reapable sessions until the live count fits the cap
func enforceSessionCap(ctx context.Context) {
	limit := maxLiveSessions(ctx)
	if limit <= 0 {
		return
	}
	sessionsLock.Lock()
	live := tuihost.LiveSessions()
	if len(live) <= limit {
		sessionsLock.Unlock()
		return
	}
	var evictable []tuihost.LiveSession
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
		full := len(tuihost.LiveSessions()) <= limit
		sessionsLock.Unlock()
		if full {
			break
		}
		sid := entry.SID
		closeSessionByID(ctx, sid, "lru_evict", func(session *tuihost.Session) bool { return sessionIsReapable(ctx, sid, session) })
	}
}

// Float of a session's last activity stamp, zero when unset
func lastActiveOf(session *tuihost.Session) float64 {
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
