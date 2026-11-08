package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Records the runtime epoch a local authority fences its store's resets with, the default too when registered
func (s *SessionStore) bindLocalAuthorityEpoch(dbKey string, epoch int64, register bool) {
	if register {
		s.localAuthorityEpoch = &epoch
	}
	if s.localAuthorityEpochs == nil {
		s.localAuthorityEpochs = map[string]int64{}
	}
	s.localAuthorityEpochs[dbKey] = epoch
}

// Epoch fencing a local reset on db, the owning store's own epoch before the registered default
func (s *SessionStore) localResetEpoch(db SessionStoreDB) (int64, error) {
	if s.localAuthorityEpoch == nil {
		return 0, fmt.Errorf("AttributeError: 'SessionStore' object has no attribute '_local_authority_epoch'")
	}
	if db == nil {
		return *s.localAuthorityEpoch, nil
	}
	pathed, ok := db.(interface{ Path() string })
	if !ok {
		return 0, fmt.Errorf("AttributeError: '%T' object has no attribute 'db_path'", db)
	}
	key, err := pypath.Resolve(pathed.Path())
	if err != nil {
		return 0, err
	}
	if epoch, held := s.localAuthorityEpochs[key]; held {
		return epoch, nil
	}
	return *s.localAuthorityEpoch, nil
}

// Resets a local route onto a fresh child through its owner's epoch fenced reset, called with the store lock held
func resetLocalSession(ctx context.Context, store *SessionStore, old *SessionEntry, sessionID string, now pyDatetime, displayName *string) (*SessionEntry, error) {
	entry := NewSessionEntry(old.SessionKey, sessionID, now, now)
	entry.Origin, entry.Platform, entry.ChatType = old.Origin, old.Platform, old.ChatType
	entry.DisplayName = old.DisplayName
	if displayName != nil {
		entry.DisplayName = displayName
	}
	entry.IsFreshReset = true
	db := store.dbForKey(ctx, old.SessionKey)
	epoch, err := store.localResetEpoch(db)
	if err != nil {
		return nil, err
	}
	resetter, ok := db.(interface {
		ResetLocalTarget(context.Context, int64, string, *pyjson.Object) error
	})
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'execute_write'", db)
	}
	if err := resetter.ResetLocalTarget(ctx, epoch, old.SessionID, entry.ToDict()); err != nil {
		return nil, err
	}
	store.entries.set(old.SessionKey, entry)
	return entry, nil
}
