package tuiserver

import (
	"strings"

	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/state"
)

// Read only session store with every schema probe run, closed again when a probe fails
func openProbedSessionDB(ctx context.Context, dbPath string) (*state.DB, error) {
	db, err := state.NewSessionDB(ctx, dbPath, true)
	if err != nil {
		return nil, err
	}
	err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		statements, err := state.SchemaReadProbeStatements(ctx)
		if err != nil {
			return err
		}
		for _, statement := range statements {
			rows, err := conn.QueryContext(ctx, statement)
			if err != nil {
				return err
			}
			rows.Next()
			err = rows.Err()
			rows.Close()
			if err != nil {
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

// Read only browse of a profile session store, schema drift reconciled once and an unusable store an error
func openBrowseSessionDB(ctx context.Context, dbPath string) (*state.DB, error) {
	info, err := os.Stat(dbPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil || info.Size() <= 0 {
		return nil, errors.New("Session store is not initialized. Start this profile's gateway to initialize it.")
	}
	db, err := openProbedSessionDB(ctx, dbPath)
	if err != nil && state.IsDeclarativeDriftError(err) {
		if _, reconcileErr := state.ReconcileDriftForBrowse(ctx, dbPath); reconcileErr != nil {
			var drift *state.DeclarativeDriftError
			if errors.As(reconcileErr, &drift) {
				pkgLog().Warn(fmt.Sprintf("session store at %s is behind SCHEMA_SQL: %s", dbPath, drift.Error()))
			}
			return nil, reconcileErr
		}
		db, err = openProbedSessionDB(ctx, dbPath)
	}
	if err == nil {
		return db, nil
	}
	class, isSQLite := state.SQLiteClassOf(err)
	if !isSQLite || class == state.SQLiteInterfaceError {
		return nil, err
	}
	detail := "Session store schema is unavailable or corrupt. Start or restart this profile's " +
		"gateway to reconcile it; if it persists, run `icebath doctor` for diagnosis. " +
		"Browsing does not repair the store."
	if class == state.SQLiteOperationalError && state.IsTransientSQLiteError(err) {
		detail = "Session store is busy (disk I/O or lock). Retry; the list was not cleared."
	}
	return nil, fmt.Errorf("%s (%w)", detail, err)
}

// Seeds the empty or missing session store of a served profile this process is about to own
func bootstrapServedProfileStore(ctx context.Context, dbPath string) error {
	if info, err := os.Stat(dbPath); err == nil {
		if info.Size() > 0 {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	db, err := state.Acquire(ctx, dbPath)
	if err != nil {
		return err
	}
	state.ReleaseOrClose(db)
	return nil
}

// Session store of the profile param with its release, the shared launch handle left open and nil when unavailable
func profileDB(ctx context.Context, params map[string]any, writer bool) (*state.DB, func(), error) {
	profile := ""
	if params != nil {
		profile = strings.TrimSpace(strOr(params["profile"]))
	}
	home, err := profileHome(profile)
	if err != nil {
		return nil, func() {}, err
	}
	if home == "" {
		db, err := getDB(ctx)
		return db, func() {}, err
	}
	dbPath := filepath.Join(home, "state.db")
	db, err := func() (*state.DB, error) {
		if writer {
			return state.Acquire(ctx, dbPath)
		}
		if err := bootstrapServedProfileStore(ctx, dbPath); err != nil {
			return nil, err
		}
		return openBrowseSessionDB(ctx, dbPath)
	}()
	if err != nil {
		return nil, func() {}, storeUnavailable(ctx, dbPath, err)
	}
	return db, func() { suppress(func() { _ = db.Close() }) }, nil
}
