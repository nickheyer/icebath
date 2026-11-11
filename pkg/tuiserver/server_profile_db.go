package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Read only browse opener of a foreign profile session store, the dashboard opener assigned at startup
var OpenSessionDBAtPath func(ctx context.Context, dbPath string, readOnly bool) (*state.DB, error)

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
		profile = pystr.Strip(strOr(params["profile"]))
	}
	home, err := profileHome(profile)
	if err != nil {
		return nil, func() {}, err
	}
	if home == "" {
		return getDB(ctx), func() {}, nil
	}
	dbPath := filepath.Join(home, "state.db")
	db, err := func() (*state.DB, error) {
		if writer {
			return state.Acquire(ctx, dbPath)
		}
		if err := bootstrapServedProfileStore(ctx, dbPath); err != nil {
			return nil, err
		}
		return OpenSessionDBAtPath(ctx, dbPath, true)
	}()
	if err != nil || db == nil {
		logger().Warn(fmt.Sprintf("TUI profile session store unavailable for %s: %v", profile, err))
		return nil, func() {}, nil
	}
	return db, func() { suppress(func() { _ = db.Close() }) }, nil
}
