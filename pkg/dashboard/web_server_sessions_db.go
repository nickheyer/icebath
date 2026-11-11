package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Runs a bulk store operation holding the exact owner lock of the profile home, 409 when an owner holds it
func withSessionMaintenance[T any](ctx context.Context, profile string, operation func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	var dir string
	if profile != "" {
		_, profileHome, err := cronProfileHome(ctx, profile)
		if err != nil {
			return zero, err
		}
		dir = profileHome
	} else {
		dir = filepath.Dir(state.ResolveDefaultDBPath(ctx))
	}
	result, err := func() (T, error) {
		inner, release, err := gateway.ExclusiveMaintenance(ctx, []string{dir}, "")
		if err != nil {
			return zero, err
		}
		defer release()
		return operation(inner)
	}()
	var conflict *gateway.OwnershipConflict
	if errors.As(err, &conflict) {
		webServerLog().Warn(fmt.Sprintf("session write refused for %s: %s", dir, conflict.Error()))
		return zero, &causedHTTPException{NewHTTPException(409, conflict.Error()), err}
	}
	return result, err
}

// Statements that probe the declared schema without reconciling it
func sessionDBReadProbeStatements(ctx context.Context) ([]string, error) {
	return state.SchemaReadProbeStatements(ctx)
}

// Runs one probe statement and fetches its first row
func fetchProbeRow(ctx context.Context, conn state.SQLiteConn, statement string) error {
	rows, err := conn.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	rows.Next()
	return rows.Err()
}

// Read only store with every schema probe run, closed again when a probe fails
func openProbedSessionDB(ctx context.Context, dbPath string) (*state.DB, error) {
	db, err := state.NewSessionDB(ctx, dbPath, true)
	if err != nil {
		return nil, err
	}
	err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		statements, err := sessionDBReadProbeStatements(ctx)
		if err != nil {
			return err
		}
		for _, statement := range statements {
			if err := fetchProbeRow(ctx, conn, statement); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Whether an error stands for the Python exception class named
func isPyClass(err error, name string) bool {
	var named interface{ PyTypeName() string }
	return errors.As(err, &named) && named.PyTypeName() == name
}

// Session store at a path, writable through the shared registry or read only after its schema probes
func openSessionDBAtPath(ctx context.Context, dbPath string, readOnly bool) (*state.DB, error) {
	if !readOnly {
		return state.Acquire(ctx, dbPath)
	}
	info, err := os.Stat(dbPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil || info.Size() <= 0 {
		return nil, NewHTTPException(503,
			"Session store is not initialized. Start this profile's gateway to initialize it.")
	}
	db, err := openProbedSessionDB(ctx, dbPath)
	if err != nil {
		if state.IsDeclarativeDriftError(err) {
			if _, rerr := state.ReconcileDriftForBrowse(ctx, dbPath); rerr != nil {
				var drift *state.DeclarativeDriftError
				if errors.As(rerr, &drift) {
					webServerLog().Warn(fmt.Sprintf("session store at %s is behind SCHEMA_SQL: %s", dbPath, drift.Error()))
					return nil, NewHTTPException(503, drift.Error())
				}
				err = rerr
			} else {
				db, err = openProbedSessionDB(ctx, dbPath)
			}
		}
	}
	if err == nil {
		return db, nil
	}
	class, isSQLite := state.SQLiteClassOf(err)
	isDatabase := isSQLite && class != state.SQLiteInterfaceError
	if !isDatabase && !isPyClass(err, "UnicodeDecodeError") {
		return nil, err
	}
	if isDatabase && state.IsMalformedDBError(err) {
		return nil, corruptStoreStatus(dbPath, err)
	}
	detail := "Session store schema is unavailable or corrupt. Start or restart this profile's " +
		"gateway to reconcile it; if it persists, run `icebath doctor` for diagnosis. " +
		"Browsing does not repair the store."
	if isSQLite && class == state.SQLiteOperationalError && state.IsTransientSQLiteError(err) {
		detail = "Session store is busy (disk I/O or lock). Retry; the list was not cleared."
	}
	return nil, NewHTTPException(503, detail)
}

// state.db path of a profile, this process's own store when the profile is empty
func sessionDBPathForProfile(ctx context.Context, profile *string) (string, error) {
	if profile != nil && *profile != "" {
		_, profileHome, err := cronProfileHome(ctx, *profile)
		if err != nil {
			return "", err
		}
		return filepath.Join(profileHome, "state.db"), nil
	}
	return state.ResolveDefaultDBPath(ctx), nil
}

// Session store of a profile, this process's own when the profile is empty
func openSessionDBForProfile(ctx context.Context, profile *string, readOnly bool) (*state.DB, error) {
	dbPath, err := sessionDBPathForProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	return openSessionDBAtPath(ctx, dbPath, readOnly)
}

const autoArchiveCheckIntervalS = 300.0

// In process throttle of the auto archive config read, keyed by profile
var lastAutoArchiveCheck = struct {
	sync.Mutex
	at map[string]float64
}{at: map[string]float64{}}

// Sweeps idle sessions of a profile store when its config enables auto archive and no gateway owns it
func maybeAutoArchiveForProfile(ctx context.Context, profile *string) {
	if err := autoArchiveForProfile(ctx, profile); err != nil {
		webServerLog().Debug("opportunistic auto-archive skipped: " + err.Error())
	}
}

// Throttled config gated archive sweep of one profile store, an error for anything the sweep raised
func autoArchiveForProfile(ctx context.Context, profile *string) error {
	key := ""
	if profile != nil {
		key = *profile
	}
	now := pythread.Monotonic()
	lastAutoArchiveCheck.Lock()
	last, seen := lastAutoArchiveCheck.at[key]
	if seen && now-last < autoArchiveCheckIntervalS {
		lastAutoArchiveCheck.Unlock()
		return nil
	}
	lastAutoArchiveCheck.at[key] = now
	lastAutoArchiveCheck.Unlock()
	dbPath, err := sessionDBPathForProfile(ctx, profile)
	if err != nil {
		return err
	}
	profileHome := filepath.Dir(dbPath)
	homeCtx := home.WithOverride(ctx, profileHome)
	tree, err := config.LoadContext(homeCtx, home.Dir(homeCtx), config.EnvRefLookupContext(homeCtx))
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if sessions := tree["sessions"]; pyTruthy(sessions) {
		m, ok := sessions.(map[string]any)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(sessions))
		}
		cfg = m
	}
	if !pyTruthy(cfg["auto_archive"]) {
		return nil
	}
	if manage.CheckGatewayRunning(profileHome) {
		return nil
	}
	var idleDays any = int64(3)
	if v, ok := cfg["auto_archive_days"]; ok {
		idleDays = v
	}
	var minInterval any = int64(24)
	if v, ok := cfg["min_interval_hours"]; ok {
		minInterval = v
	}
	days, err := pyint.Float(idleDays)
	if err != nil {
		return err
	}
	hours, err := pyint.Int64(minInterval)
	if err != nil {
		return err
	}
	db, err := openSessionDBForProfile(ctx, profile, false)
	if err != nil {
		return err
	}
	defer db.Close()
	db.MaybeAutoArchive(ctx, days, float64(hours), true)
	return nil
}

// Seconds since the last chat activity of this profile, false while one of its sessions runs a turn
func skillMaintenanceIdleFor(ctx context.Context, startedAt float64) (float64, bool, error) {
	profileHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return 0, false, err
	}
	key := pypath.Normcase(profileHome)
	var mine []*tuigateway.Session
	for _, live := range tuigateway.LiveSessions() {
		owner := profileHome
		if raw := live.Session.Get("profile_home"); pyTruthy(raw) {
			text, ok := raw.(string)
			if !ok {
				return 0, false, fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyrepr.TypeName(raw))
			}
			if owner, err = pypath.Resolve(text); err != nil {
				return 0, false, err
			}
		}
		if pypath.Normcase(owner) == key {
			mine = append(mine, live.Session)
		}
	}
	lastActive := max(startedAt, tuiserver.ClosedSessionActivity(profileHome))
	for _, session := range mine {
		if pyTruthy(session.Get("running")) {
			return 0, false, nil
		}
	}
	for _, session := range mine {
		var raw any = startedAt
		if v := session.Get("last_active"); pyTruthy(v) {
			raw = v
		}
		active, err := pyint.Float(raw)
		if err != nil {
			return 0, false, err
		}
		lastActive = max(lastActive, active)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	return max(0.0, now-lastActive), true, nil
}

// Runs a curator pass for this profile when idle long enough and no live gateway owns its chores
func maybeRunSkillMaintenance(ctx context.Context, startedAt float64) {
	if manage.CheckGatewayRunning(home.Dir(ctx)) {
		return
	}
	idleFor, idle, err := skillMaintenanceIdleFor(ctx, startedAt)
	if err != nil {
		webServerLog().Debug("serve curator tick skipped: " + err.Error())
		return
	}
	if idle {
		memory.MaybeRunCurator(ctx, &idleFor, nil)
	}
}

// Polls the auto archive and curator chores of this serve profile until ctx ends
func autoArchiveTickerLoop(ctx context.Context, intervalS, initialDelayS float64) {
	startedAt := float64(time.Now().UnixNano()) / 1e9
	sweep := func() {
		maybeAutoArchiveForProfile(ctx, nil)
		maybeRunSkillMaintenance(ctx, startedAt)
	}
	wait := time.Duration(initialDelayS * float64(time.Second))
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		sweep()
		wait = time.Duration(intervalS * float64(time.Second))
	}
}
