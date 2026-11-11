package gatewayrun

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python RuntimeError carrying only its message
type pyRuntimeError struct{ msg string }

func (e *pyRuntimeError) Error() string { return e.msg }

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
			if borrowed != nil {
				db, ok := borrowed.(*state.SessionDB)
				if !ok {
					return nil, &pyRuntimeError{msg: fmt.Sprintf("SessionStore SQLite handle %T is not a SessionDB", borrowed)}
				}
				if db != nil {
					return state.NewBorrowedAsyncSessionDB(db), nil
				}
			}
			return nil, &pyRuntimeError{msg: "SessionStore SQLite handle unavailable"}
		}
		db, err := state.Acquire(ctx, "")
		if err != nil {
			runLog().Warn(fmt.Sprintf("SQLite session store not available: %s", pyrepr.ExceptionMessage(err)))
			return nil, err
		}
		return state.NewAsyncSessionDB(db), nil
	}
	recovered := func() {
		r.sessionDBInitError = nil
		runLog().Info("SQLite session store recovered")
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

// Pins db for every later read of the session database, nil included
func (r *GatewayRunner) pinSessionDB(db *state.AsyncSessionDB) {
	r.sessionDBPinned = sessionDBPin{set: true, db: db}
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
					runLog().Debug(fmt.Sprintf("SessionDB close error during handle sweep: %v", rec))
				}
			}()
			state.ReleaseOrClose(inner)
		}()
	}
	if r.sessionDBHandleCache == nil {
		panic(&pyAttributeError{msg: "'GatewayRunner' object has no attribute '_session_db_handle_cache'"})
	}
	r.sessionDBHandleCache.CloseAll(closeHandle)
}

// Session database handles and the startup state db maintenance of every served profile
func (r *GatewayRunner) initSessionDB(ctx context.Context) {
	r.sessionDBPinned = sessionDBPin{}
	r.sessionDBHandles = map[string]any{}
	r.sessionDBHandlesLock = new(sync.Mutex)
	r.sessionDBHandleCache = gateway.NewRecoverableHandleCacheWith(gateway.RecoverableHandleCacheOptions{
		Handles: r.sessionDBHandles, Lock: r.sessionDBHandlesLock})
	if _, err := r.openSessionDBForActiveScope(ctx, true); err != nil {
		runLog().Warn(fmt.Sprintf("SQLite session store not available: %s", pyrepr.ExceptionMessage(err)), "error", err)
		msg := pyrepr.ExceptionMessage(err)
		r.sessionDBInitError = &msg
	}
	launch := launchSessionsDir(ctx, r.config)
	housekeepingChore("state.db startup maintenance", func() error {
		return forEachServedProfile(ctx, r, func(ctx context.Context, _ string) error {
			return housekeepingStateDBMaintenance(ctx, launch)
		})
	})
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
			runLog().Debug(fmt.Sprintf("%s error: %v", label, rec))
		}
	}()
	if err := fn(); err != nil {
		runLog().Debug(fmt.Sprintf("%s error: %s", label, pyrepr.ExceptionMessage(err)))
	}
}

// Auto archive and auto prune with vacuum of the state db of the profile in scope, by its sessions config
func housekeepingStateDBMaintenance(ctx context.Context, launch *launchSessions) error {
	full, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return err
	}
	sessCfg := map[string]any{}
	if raw := full["sessions"]; pyrepr.Truthy(raw) {
		m, ok := raw.(map[string]any)
		if !ok {
			return &pyAttributeError{msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
		}
		sessCfg = m
	}
	get := func(key string, def any) any {
		if v, ok := sessCfg[key]; ok {
			return v
		}
		return def
	}
	autoArchive := pyrepr.Truthy(get("auto_archive", false))
	autoPrune := pyrepr.Truthy(get("auto_prune", false))
	if !autoArchive && !autoPrune {
		return nil
	}
	adb, err := state.Acquire(ctx, "")
	if err != nil {
		return err
	}
	defer state.ReleaseOrClose(adb)
	if autoArchive {
		idleDays, err := pyint.Float(get("auto_archive_days", 3))
		if err != nil {
			return err
		}
		minInterval, err := pyint.Int64(get("min_interval_hours", 24))
		if err != nil {
			return err
		}
		adb.MaybeAutoArchive(ctx, idleDays, float64(minInterval), true)
	}
	if autoPrune {
		opts := state.NewAutoPruneOptions()
		retention, err := pyint.Int64(get("retention_days", 90))
		if err != nil {
			return err
		}
		minInterval, err := pyint.Int64(get("min_interval_hours", 24))
		if err != nil {
			return err
		}
		minVacuum, err := pyint.Int64(get("min_vacuum_interval_days", 30))
		if err != nil {
			return err
		}
		days := float64(retention)
		opts.RetentionDays = &days
		opts.MinIntervalHours = float64(minInterval)
		opts.MinVacuumIntervalDays = float64(minVacuum)
		opts.Vacuum = pyrepr.Truthy(get("vacuum_after_prune", true))
		opts.SessionsDir = profileSessionsDir(ctx, launch)
		adb.MaybeAutoPruneAndVacuum(ctx, opts)
	}
	return nil
}
