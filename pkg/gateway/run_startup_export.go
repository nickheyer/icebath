package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Done signal of the session task the adapter runs for a key, nil when none runs
func (b *BasePlatformAdapter) SessionTaskDone(sessionKey string) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if task := b.sessionTasks[sessionKey]; task != nil {
		return task.done
	}
	return nil
}

// Loaded routing entries the keep function selects, read under the store lock after loading
func (s *SessionStore) FilterLoadedEntries(ctx context.Context, keep func(entry *SessionEntry) bool) []*SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
	s.entries.mu.RLock()
	defer s.entries.mu.RUnlock()
	var out []*SessionEntry
	for _, key := range s.entries.keys {
		if entry := s.entries.m[key]; entry != nil && keep(entry) {
			out = append(out, entry)
		}
	}
	return out
}

// Epoch seconds of the entry's last resume mark, else of its last update
func (e *SessionEntry) ResumeMarkerTimestamp() float64 {
	if e.LastResumeMarkedAt != nil {
		return e.LastResumeMarkedAt.timestamp()
	}
	return e.UpdatedAt.timestamp()
}

// Whether the authority's store holds a canonical admission for the session, true for a retiring store that cannot answer
func HoldsAdmission(ctx context.Context, a *SessionAuthority, sessionID string) (bool, error) {
	owner, err := AdmissionOwner(ctx, a, sessionID)
	var held bool
	if err == nil {
		err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			rows, qerr := conn.QueryContext(ctx, "SELECT 1 FROM session_admissions WHERE target_session_id IN (?,?) LIMIT 1", sessionID, owner)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			held = rows.Next()
			return rows.Err()
		})
	}
	if err != nil {
		if !a.Retiring {
			return false, err
		}
		slog.Default().With("logger", "gateway.run").Warn(fmt.Sprintf("Skipping auto-resume of %s: retained store %s cannot answer: %s", sessionID, a.ProfileID, pyrepr.ExceptionMessage(err)))
		return true, nil
	}
	return held, nil
}

// Removes MEDIA directives from a message body
func StripMediaDirectives(ctx context.Context, text string) (string, error) {
	return stripMediaDirectives(ctx, text)
}

// Epoch seconds the entry's active turn marker was stamped at, false without one
func (e *SessionEntry) ActiveTurnStartedTimestamp() (float64, bool) {
	if e.ActiveTurnStartedAt == nil {
		return 0, false
	}
	return e.ActiveTurnStartedAt.timestamp(), true
}

// Authority, local owner and row id a repoint of a session onto a messaging lane carries
func ConversationHead(ctx context.Context, runner any, sessionID string) (*SessionAuthority, *string, string, error) {
	return conversationHead(ctx, runner, sessionID)
}

// Moves the local mount a viewer still holds onto the lane a handoff repointed
func FollowHandOff(ctx context.Context, a *SessionAuthority, source *SessionSource, route, owner string, localOwner *string) error {
	return followHandOff(ctx, a, source, route, owner, localOwner)
}
