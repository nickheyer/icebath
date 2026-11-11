package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner that holds the session database of the profile it launched with
type sessionDBRunner interface {
	SessionDB(ctx context.Context) *state.SessionDB
}

// Runner that takes the authority a launch reaches as its own
type authorityRegistrar interface {
	SetSessionAuthority(a *SessionAuthority)
}

// Stamps this store's own profile onto session rows that predate ownership, returning how many it stamped
func backfillLegacySessionOwners(ctx context.Context, db *state.SessionDB) int {
	stamp := db.OwnProfileName()
	if stamp == "" {
		return 0
	}
	stamped, err := db.BackfillNullSessionProfiles(ctx, stamp)
	if err != nil {
		slog.Warn("Legacy session owner backfill failed for "+db.Path(), "error", err)
		return 0
	}
	if stamped != 0 {
		slog.Info(fmt.Sprintf("Stamped %d legacy session row(s) with owner profile %s", stamped, pyrepr.Repr(stamp)))
	}
	return stamped
}

// Stamps a created source onto compression chain roots that predate write time stamping
func backfillCompressionCreatedSources(ctx context.Context, db *state.SessionDB) int64 {
	stamped, err := db.StampLegacyCompressionChains(ctx)
	if err != nil {
		slog.Warn("Compression created_source repair failed for "+db.Path(), "error", err)
		return 0
	}
	if stamped != 0 {
		slog.Info(fmt.Sprintf("Stamped created_source on %d compressed session row(s)", stamped))
	}
	return stamped
}

// Builds the authority of a profile store after exclusive ownership, before any adapter or API connects
func InitializeSessionAuthority(ctx context.Context, runner AuthorityRunner, profileID, instanceID string, db *state.SessionDB, register bool) (*SessionAuthority, error) {
	if db == nil {
		holder, ok := runner.(sessionDBRunner)
		if !ok {
			return nil, fmt.Errorf("'%T' object has no attribute '_session_db'", runner)
		}
		db = holder.SessionDB(ctx)
	}
	dbKey, err := pypath.Resolve(db.Path())
	if err != nil {
		return nil, err
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
		return nil, err
	}
	if _, err := settleRecoveredUnattended(ctx, db, epoch); err != nil {
		return nil, err
	}
	backfillLegacySessionOwners(ctx, db)
	backfillCompressionCreatedSources(ctx, db)
	if err := releaseHandedOffReceipts(ctx, db, epoch); err != nil {
		return nil, err
	}
	store := runner.SessionStore()
	if err := releaseSupersededBotChatAtBoot(ctx, db, store); err != nil {
		return nil, err
	}
	a := NewSessionAuthority(runner, profileID, instanceID, db, epoch)
	if err := adoptLocalCompressions(ctx, a); err != nil {
		return nil, err
	}
	if register {
		registrar, ok := runner.(authorityRegistrar)
		if !ok {
			return nil, fmt.Errorf("'%T' object has no attribute 'session_authority'", runner)
		}
		registrar.SetSessionAuthority(a)
	}
	bindCronOwner(context.WithoutCancel(ctx), a)
	store.bindLocalAuthorityEpoch(dbKey, epoch, register)
	watchRoutes(ctx, runner, store)
	if err := recoverLocalSessions(ctx, a, false); err != nil {
		return nil, err
	}
	return a, nil
}
