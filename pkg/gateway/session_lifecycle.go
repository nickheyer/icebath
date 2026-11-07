package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python datetime value, naive ones holding local wall time and aware ones their own offset
type pyDatetime struct {
	T     time.Time
	Aware bool
}

// Current local wall time as a naive datetime
func sessionNow() pyDatetime { return pyDatetime{T: time.Now()} }

// Session id stamped from the given instant with an eight character random suffix
func newRoutedSessionID(now pyDatetime) string { return state.NewSessionIDAt(now.T, 8) }

// Python isoformat of the value in its naive or aware form
func (d pyDatetime) isoformat() string {
	if d.Aware {
		return pydatetime.ISOFormat(d.T)
	}
	return pydatetime.ISOFormatNaive(d.T)
}

// Epoch seconds, a naive value read as local time
func (d pyDatetime) timestamp() float64 { return float64(d.T.UnixNano()) / 1e9 }

// Isoformat of an optional datetime, nil when absent
func isoOf(d *pyDatetime) any {
	if d == nil {
		return nil
	}
	return d.isoformat()
}

// Strict fromisoformat that keeps a naive value as local wall time
func fromISOFormat(value string) (pyDatetime, error) {
	t, aware, err := pydatetime.ParseISOFormat(value)
	if err != nil {
		return pyDatetime{}, err
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	return pyDatetime{T: t, Aware: aware}, nil
}

// Fromisoformat returning nil for an empty, non string or malformed value
func parseISOValue(value any) *pyDatetime {
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	d, err := fromISOFormat(s)
	if err != nil {
		return nil
	}
	return &d
}

const autoContinueFreshnessSecsDefault = 60 * 60

// Resume freshness window in seconds from the environment, one hour when unset or unparsable
func AutoContinueFreshnessWindow() float64 {
	raw := os.Getenv("ICEBATH_AUTO_CONTINUE_FRESHNESS")
	if raw == "" {
		return autoContinueFreshnessSecsDefault
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return autoContinueFreshnessSecsDefault
	}
	return v
}

// True when the row is ended or gone in its owning store, false with no store or on a read error
func (s *SessionStore) isSessionEndedInDB(ctx context.Context, sessionID, sessionKey string) bool {
	s.mu.Lock()
	pending := s.rowsPending[sessionID]
	s.mu.Unlock()
	if pending {
		return false
	}
	var db SessionStoreDB
	if sessionKey != "" {
		db = s.dbForKey(ctx, sessionKey)
	} else {
		db = s.dbForSessionID(ctx, sessionID)
	}
	if db == nil || sessionID == "" {
		return false
	}
	row, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return false
	}
	if row == nil {
		return true
	}
	end, ok := row.Get("end_reason")
	return ok && end != nil
}

// Physical target the local receipt names for the entry's local route, empty otherwise
func (s *SessionStore) localReceiptTarget(ctx context.Context, entry *SessionEntry) string {
	if entry == nil || entry.Origin == nil || entry.Origin.Platform != PlatformLocal {
		return ""
	}
	db := s.dbForKey(ctx, entry.SessionKey)
	if db == nil {
		return ""
	}
	receipt, err := db.LocalReceipt(ctx, entry.Origin.ChatID)
	if err != nil || receipt == nil {
		return ""
	}
	if route, _ := receipt.Get("route"); route != entry.SessionKey {
		return ""
	}
	inner, _ := receipt.Get("entry")
	obj, ok := inner.(*pyjson.Object)
	if !ok {
		return ""
	}
	sid, ok := obj.Get("session_id")
	if !ok {
		return ""
	}
	return pyFloatStr(sid)
}

// True when the API binding receipt names this entry's route
func (s *SessionStore) apiBound(ctx context.Context, entry *SessionEntry) bool {
	if entry == nil || entry.Origin == nil || entry.Origin.Platform != PlatformAPIServer {
		return false
	}
	db := s.dbForKey(ctx, entry.SessionKey)
	if db == nil {
		return false
	}
	saved, ok, err := db.GetMeta(ctx, state.LocalAPIBindingPrefix+entry.Origin.ChatID)
	if err != nil || !ok {
		return false
	}
	parsed, err := pyjson.Loads(saved)
	if err != nil {
		return false
	}
	outer, ok := parsed.(*pyjson.Object)
	if !ok {
		return false
	}
	inner, _ := outer.Get("entry")
	obj, ok := inner.(*pyjson.Object)
	if !ok {
		return false
	}
	key, _ := obj.Get("session_key")
	return key == entry.SessionKey
}

// Reset reason for the route, only an explicit suspension replaces a conversation
func (s *SessionStore) routeResetReason(entry *SessionEntry) string {
	if entry.Suspended {
		return "suspended"
	}
	return ""
}

// Applies mutate under the lock and saves, false when missing or mutate declined
func (s *SessionStore) updateEntry(ctx context.Context, sessionKey string, mutate func(*SessionEntry) bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || !mutate(entry) {
		return false, nil
	}
	return true, s.save(ctx)
}

// Applies mutate to every entry under the lock and saves once when any changed
func (s *SessionStore) updateAllEntriesLocked(ctx context.Context, mutate func(*SessionEntry) bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
	changed := 0
	for _, entry := range s.entries.values() {
		if mutate(entry) {
			changed++
		}
	}
	if changed > 0 {
		if err := s.save(ctx); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// Marks the session suspended so its next access auto resets, true when it existed
func (s *SessionStore) SuspendSession(ctx context.Context, sessionKey string) (bool, error) {
	return s.updateEntry(ctx, sessionKey, func(e *SessionEntry) bool { e.Suspended = true; return true })
}

// Persists the active turn pair before publishing it in memory, lock held
func (s *SessionStore) setTurnMarkerLocked(ctx context.Context, sessionKey string, entry *SessionEntry, token *string, startedAt *pyDatetime) error {
	candidate := entry.ToDict()
	if token == nil {
		candidate.Set("active_turn_token", nil)
	} else {
		candidate.Set("active_turn_token", *token)
	}
	candidate.Set("active_turn_started_at", isoOf(startedAt))
	var touched *pyDatetime
	if startedAt != nil {
		now := sessionNow()
		touched = &now
		candidate.Set("updated_at", touched.isoformat())
	}
	if err := s.saveEntry(ctx, sessionKey, candidate, true); err != nil {
		return err
	}
	entry.ActiveTurnToken = token
	entry.ActiveTurnStartedAt = startedAt
	if touched != nil {
		entry.UpdatedAt = *touched
	}
	return nil
}

// Random uuid4 hex string
func uuid4HexString() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// Persists ownership of the running turn and returns its token, empty when the entry is missing
func (s *SessionStore) MarkTurnActive(ctx context.Context, sessionKey string) (string, error) {
	token := uuid4HexString()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil {
		return "", nil
	}
	started := pyDatetime{T: time.Now().UTC(), Aware: true}
	if err := s.setTurnMarkerLocked(ctx, sessionKey, entry, &token, &started); err != nil {
		return "", err
	}
	return token, nil
}

// Compare and swap clear of the active turn marker, false when a newer turn owns it
func (s *SessionStore) ClearTurnActive(ctx context.Context, sessionKey, token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || entry.ActiveTurnToken == nil || *entry.ActiveTurnToken != token {
		return false, nil
	}
	if err := s.setTurnMarkerLocked(ctx, sessionKey, entry, nil, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Promotes crash left turn markers into resume pending and returns how many were newly promoted
func (s *SessionStore) RecoverInterruptedTurns(ctx context.Context, maxAgeSeconds int) (int, error) {
	now, epochNow := sessionNow(), float64(time.Now().UnixNano())/1e9
	promoted := 0
	_, err := s.updateAllEntriesLocked(ctx, func(entry *SessionEntry) bool {
		if entry.ActiveTurnToken == nil || *entry.ActiveTurnToken == "" {
			return false
		}
		started := entry.ActiveTurnStartedAt
		stale := started == nil || (maxAgeSeconds > 0 && epochNow-started.timestamp() > float64(maxAgeSeconds))
		if !stale && !entry.Suspended {
			if entry.ResumePending {
				if entry.LastResumeMarkedAt == nil {
					marked := now
					entry.LastResumeMarkedAt = &marked
				}
			} else {
				reason, marked := "restart_interrupted", now
				entry.ResumePending, entry.ResumeReason, entry.LastResumeMarkedAt = true, &reason, &marked
				promoted++
			}
		}
		entry.ActiveTurnToken, entry.ActiveTurnStartedAt = nil, nil
		return true
	})
	return promoted, err
}

// Clears orphan turn markers after a verified clean shutdown
func (s *SessionStore) DiscardActiveTurnMarkers(ctx context.Context) (int, error) {
	return s.updateAllEntriesLocked(ctx, func(entry *SessionEntry) bool {
		if (entry.ActiveTurnToken == nil || *entry.ActiveTurnToken == "") && entry.ActiveTurnStartedAt == nil {
			return false
		}
		entry.ActiveTurnToken, entry.ActiveTurnStartedAt = nil, nil
		return true
	})
}

// Marks the session resumable after a restart interruption unless it is suspended
func (s *SessionStore) MarkResumePending(ctx context.Context, sessionKey, reason string) (bool, error) {
	return s.updateEntry(ctx, sessionKey, func(entry *SessionEntry) bool {
		if entry.Suspended {
			return false
		}
		marked := sessionNow()
		entry.ResumePending, entry.ResumeReason, entry.LastResumeMarkedAt = true, &reason, &marked
		return true
	})
}

// Clears the resume pending flag after a resumed turn, true when it was set
func (s *SessionStore) ClearResumePending(ctx context.Context, sessionKey string) (bool, error) {
	return s.updateEntry(ctx, sessionKey, func(entry *SessionEntry) bool {
		if !entry.ResumePending {
			return false
		}
		entry.ResumePending, entry.ResumeReason, entry.LastResumeMarkedAt = false, nil, nil
		return true
	})
}

// Drops routing entries idle past the age keeping suspended and busy ones, zero or less disables
func (s *SessionStore) PruneOldEntries(ctx context.Context, maxAgeDays int) (int, error) {
	if maxAgeDays <= 0 {
		return 0, nil
	}
	cutoff := sessionNow().T.AddDate(0, 0, -maxAgeDays)
	s.mu.Lock()
	s.ensureLoadedLocked(ctx)
	var removed []string
	for _, entry := range s.entries.values() {
		if !entry.Suspended && !s.hasActiveProcessesSafe(entry.SessionKey, "prune") && entry.UpdatedAt.T.Before(cutoff) {
			removed = append(removed, entry.SessionKey)
		}
	}
	for _, key := range removed {
		s.entries.delete(key)
	}
	var err error
	if len(removed) > 0 {
		err = s.save(ctx)
	}
	s.mu.Unlock()
	if len(removed) > 0 {
		slog.Info(fmt.Sprintf("SessionStore pruned %d entries older than %d days", len(removed), maxAgeDays))
	}
	return len(removed), err
}

// Python str of a decoded JSON scalar, empty for null
func pyFloatStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e16 {
			return strconv.FormatFloat(x, 'f', 1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}
