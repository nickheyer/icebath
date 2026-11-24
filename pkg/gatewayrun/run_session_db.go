package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python RuntimeError carrying only its message
type pyRuntimeError struct {
	msg   string
	cause error
}

func (e *pyRuntimeError) Error() string { return e.msg }

func (e *pyRuntimeError) Unwrap() error { return e.cause }

// Exception class the error reports, RuntimeError
func (e *pyRuntimeError) PyTypeName() string { return "RuntimeError" }

// Python AttributeError carrying only its message
type pyAttributeError struct{ msg string }

func (e *pyAttributeError) Error() string { return e.msg }

// Exception class the error reports, AttributeError
func (e *pyAttributeError) PyTypeName() string { return "AttributeError" }

// Async database of the active profile scope, one per path, borrowing the session store handle when present
func (r *GatewayRunner) openSessionDBForActiveScope(ctx context.Context, raiseOnError bool) (*state.AsyncSessionDB, error) {
	path := state.ResolveDefaultDBPath(ctx)
	cache := r.sessionDBHandleCache
	if cache == nil {
		cache = gateway.NewRecoverableHandleCacheWith(gateway.RecoverableHandleCacheOptions{
			Handles: r.sessionDBHandles, Lock: r.sessionDBHandlesLock})
		r.sessionDBHandles, r.sessionDBHandlesLock = cache.Handles, cache.Lock
		r.sessionDBHandleCache = cache
	}
	open := func() (any, error) {
		if store := r.sessionStore; store != nil {
			borrowed, err := store.DB(ctx)
			if err != nil {
				return nil, err
			}
			if borrowed == nil {
				return nil, storeOpenCause(store, path)
			}
			db, ok := borrowed.(*state.SessionDB)
			if !ok || db == nil {
				return nil, &pyRuntimeError{msg: fmt.Sprintf("SessionStore SQLite handle %T is not a SessionDB", borrowed)}
			}
			return state.NewBorrowedAsyncSessionDB(db), nil
		}
		db, err := state.Acquire(ctx, "")
		if err != nil {
			return nil, err
		}
		return state.NewAsyncSessionDB(db), nil
	}
	recovered := func() {
		pkgLog().Info("SQLite session store recovered")
	}
	handle, err := cache.GetWith(ctx, path, open, gateway.HandleCacheGetOptions{RaiseOnError: raiseOnError, OnRecovered: recovered})
	if err != nil || handle == nil {
		return nil, err
	}
	return handle.(*state.AsyncSessionDB), nil
}

// Pinned database when one was set, else the async database of the active profile scope or nil
func (r *GatewayRunner) sessionDB(ctx context.Context) *state.AsyncSessionDB {
	if r.sessionDBPinned.set {
		return r.sessionDBPinned.db
	}
	db, _ := r.openSessionDBForActiveScope(ctx, false)
	return db
}

// Store wrapped by the session database of the active scope, nil when it is unavailable
func (r *GatewayRunner) SessionDB(ctx context.Context) *state.SessionDB {
	db := r.sessionDB(ctx)
	if db == nil {
		return nil
	}
	return db.DB()
}

// Releases every per profile handle this runner opened, skipping the ones borrowed from the session store
func (r *GatewayRunner) closeAllSessionDBHandles() {
	closeHandle := func(handle any) {
		db, _ := handle.(*state.AsyncSessionDB)
		if db == nil || db.Borrowed() {
			return
		}
		inner := db.DB()
		if inner == nil {
			return
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					pkgLog().Debug(fmt.Sprintf("SessionDB close error during handle sweep: %v", rec))
				}
			}()
			state.ReleaseOrClose(inner)
		}()
	}
	if r.sessionDBHandleCache == nil {
		panic(&pyAttributeError{msg: "the gateway runner has no session database handle cache"})
	}
	r.sessionDBHandleCache.CloseAll(closeHandle)
}

// Open failure the session store recorded for path, the open another caller has in flight when none is recorded
func storeOpenCause(store *gateway.SessionStore, path string) error {
	if cause := store.DBHandleCache().LastFailure(path); cause != nil {
		return cause
	}
	return &pyRuntimeError{msg: fmt.Sprintf("another caller is still opening %s", path)}
}

// Session database handles and the startup state db maintenance, a launch store that will not open stopping the start
func (r *GatewayRunner) initSessionDB(ctx context.Context) error {
	r.sessionDBPinned = sessionDBPin{}
	r.sessionDBHandles = map[string]any{}
	r.sessionDBHandlesLock = new(sync.Mutex)
	r.sessionDBHandleCache = gateway.NewRecoverableHandleCacheWith(gateway.RecoverableHandleCacheOptions{
		Handles: r.sessionDBHandles, Lock: r.sessionDBHandlesLock})
	if _, err := r.openSessionDBForActiveScope(ctx, true); err != nil {
		var described *state.StoreUnavailableError
		if errors.As(err, &described) {
			return err
		}
		return state.NewStoreUnavailableError(ctx, state.ResolveDefaultDBPath(ctx), err)
	}
	launch := launchSessionsDir(ctx, r.config)
	housekeepingChore("state.db startup maintenance", func() error {
		return forEachServedProfile(ctx, r, func(ctx context.Context, _ string) error {
			return housekeepingStateDBMaintenance(ctx, launch)
		})
	})
	return nil
}

// Launch home with its configured transcript dir, nil when the config carries none
type launchSessions struct {
	home        string
	sessionsDir string
}

// Launch home and configured transcript dir, resolved outside any profile scope, nil without a sessions dir
func launchSessionsDir(ctx context.Context, cfg *gateway.GatewayConfig) *launchSessions {
	if cfg == nil || cfg.SessionsDir == "" {
		return nil
	}
	return &launchSessions{home: home.Dir(ctx), sessionsDir: cfg.SessionsDir}
}

// Transcript dir of the profile in scope, the configured one for the launch home and home sessions otherwise
func profileSessionsDir(ctx context.Context, launch *launchSessions) string {
	h := home.Dir(ctx)
	if launch != nil && filepath.Clean(launch.home) == filepath.Clean(h) {
		return launch.sessionsDir
	}
	return filepath.Join(h, "sessions")
}

// Runs one housekeeping chore, logging any failure at debug so the loop never stops
func housekeepingChore(label string, fn func() error) {
	defer func() {
		if rec := recover(); rec != nil {
			pkgLog().Debug(fmt.Sprintf("%s error: %v", label, rec))
		}
	}()
	if err := fn(); err != nil {
		pkgLog().Debug(fmt.Sprintf("%s error: %s", label, err.Error()))
	}
}

// Auto archive and auto prune with vacuum of the state db of the profile in scope, by its sessions settings
func housekeepingStateDBMaintenance(ctx context.Context, launch *launchSessions) error {
	autoArchive, autoPrune := settings.SessionsAutoArchive.Get(ctx), settings.AutoPrune.Get(ctx)
	if !autoArchive && !autoPrune {
		return nil
	}
	adb, err := state.Acquire(ctx, "")
	if err != nil {
		return err
	}
	defer state.ReleaseOrClose(adb)
	minIntervalHours := settings.SessionsMinIntervalHours.Get(ctx).Hours()
	if autoArchive {
		adb.MaybeAutoArchive(ctx, settings.SessionsAutoArchiveDays.Get(ctx).Hours()/24, minIntervalHours, true)
	}
	if autoPrune {
		opts := state.NewAutoPruneOptions()
		days := settings.SessionsRetentionDays.Get(ctx).Hours() / 24
		opts.RetentionDays = &days
		opts.MinIntervalHours = minIntervalHours
		opts.MinVacuumIntervalDays = settings.SessionsMinVacuumIntervalDays.Get(ctx).Hours() / 24
		opts.Vacuum = settings.SessionsVacuumAfterPrune.Get(ctx)
		opts.SessionsDir = profileSessionsDir(ctx, launch)
		adb.MaybeAutoPruneAndVacuum(ctx, opts)
	}
	return nil
}
