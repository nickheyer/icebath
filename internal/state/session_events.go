package state

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/internal/home"
)

// Reasons a session deleted event carries
const (
	DeleteReasonDelete     = "delete"
	DeleteReasonArchive    = "archive"
	DeleteReasonEmptySweep = "empty_sweep"
	DeleteReasonGhostPrune = "ghost_prune"
)

// Committed delete or deliberate archive the on_session_delete observers receive
type SessionDeleted struct {
	// Session the caller asked for, the first removed id standing in for a bulk sweep
	SessionID string
	Reason    string
	// Profile owning the store, empty when the store sits outside the profile tree
	Profile string
	// Every removed id, set only when more than one row went
	DeletedIDs []string
	// Transcript directory the caller swept, empty when it named none
	SessionsDir string
	// Home the store lives in, the profile whose plugins the event reaches
	Home string
}

// Hands a committed delete to the observer under the store's own home, never failing the delete
func (s *Store) emitSessionDeleted(ctx context.Context, deletedIDs []string, reason, primary, sessionsDir string) {
	seen := map[string]bool{}
	var ids []string
	for _, id := range deletedIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	observe := s.tables.state.options.OnSessionDelete
	if observe == nil {
		return
	}
	storeHome := filepath.Dir(absPath(s.path))
	profile, _ := home.ProfileName(storeHome)
	ev := SessionDeleted{SessionID: ids[0], Reason: reason, Profile: profile, SessionsDir: sessionsDir, Home: storeHome}
	if containsString(ids, primary) {
		ev.SessionID = primary
	}
	if len(ids) > 1 {
		ev.DeletedIDs = ids
	}
	dispatch := ctx
	if current, err := s.eng.home.Home(ctx); profile != "" && (err != nil || resolvePath(current) != resolvePath(storeHome)) {
		dispatch = home.WithOverride(ctx, storeHome)
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("on_session_delete dispatch failed", "panic", r)
		}
	}()
	observe(dispatch, ev)
}
