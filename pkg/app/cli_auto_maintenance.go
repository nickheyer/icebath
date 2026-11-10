package app

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// stateMaintenanceDB is the session store surface the startup maintenance drives
type stateMaintenanceDB interface {
	GetMeta(ctx context.Context, key string) (string, bool, error)
	SetMeta(ctx context.Context, key, value string) error
	PruneEmptyGhostSessions(ctx context.Context, sessionsDir string) (int, error)
	FinalizeOrphanedCompressionSessions(ctx context.Context) (int64, error)
	MaybeAutoArchive(ctx context.Context, idleDays, minIntervalHours float64, excludePinned bool) state.AutoArchiveResult
	MaybeAutoPruneAndVacuum(ctx context.Context, o state.AutoPruneOptions) state.AutoPruneResult
}

var (
	loadMaintenanceConfig = func(ctx context.Context) (map[string]any, error) {
		cfg, err := config.Load(home.Dir(ctx), nil)
		return cfg, err
	}
	autoPruneCheckpointsFromConfig = tools.AutoPruneCheckpointsFromConfig
)

// Python int of a sessions config value, the default when the key is absent
func maintenanceInt(cfg map[string]any, key string, def int64) (float64, error) {
	v, ok := cfg[key]
	if !ok {
		return float64(def), nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	f, _ := n.Float64()
	return f, nil
}

// Runs the one time state.db repairs, then auto archive, prune and vacuum per the sessions config, never failing
func runStateDBAutoMaintenance(ctx context.Context, db stateMaintenanceDB) {
	if db == nil || (reflect.ValueOf(db).Kind() == reflect.Pointer && reflect.ValueOf(db).IsNil()) {
		return
	}
	skipped := func(err any) { cliLog.Debug(fmt.Sprintf("state.db auto-maintenance skipped: %v", err)) }
	defer func() {
		if r := recover(); r != nil {
			skipped(r)
		}
	}()
	sessionsDir := filepath.Join(home.Dir(ctx), "sessions")
	repairs := []struct {
		metaKey, doneMsg, skipMsg string
		repair                    func() (int64, error)
	}{
		{"ghost_session_prune_v1", "Pruned %d empty TUI ghost sessions", "Ghost session prune skipped: %s", func() (int64, error) {
			n, err := db.PruneEmptyGhostSessions(ctx, sessionsDir)
			return int64(n), err
		}},
		{"orphaned_compression_finalize_v1", "Finalized %d orphaned compression sessions", "Orphan compression finalize skipped: %s", func() (int64, error) {
			return db.FinalizeOrphanedCompressionSessions(ctx)
		}},
	}
	for _, r := range repairs {
		done, _, err := db.GetMeta(ctx, r.metaKey)
		if err != nil {
			skipped(err)
			return
		}
		if done != "" {
			continue
		}
		count, err := r.repair()
		if err != nil {
			cliLog.Warning(fmt.Sprintf(r.skipMsg, err))
			count = 0
		}
		if err := db.SetMeta(ctx, r.metaKey, "1"); err != nil {
			skipped(err)
			return
		}
		if count != 0 {
			cliLog.Info(fmt.Sprintf(r.doneMsg, count))
		}
	}
	full, err := loadMaintenanceConfig(ctx)
	if err != nil {
		skipped(err)
		return
	}
	cfg, _ := full["sessions"].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	minIntervalHours, intervalErr := maintenanceInt(cfg, "min_interval_hours", 24)
	if cliTruthy(cfg["auto_archive"]) {
		idleDays := 3.0
		if v, ok := cfg["auto_archive_days"]; ok {
			if idleDays, err = pyint.Float(v); err != nil {
				skipped(err)
				return
			}
		}
		if intervalErr != nil {
			skipped(intervalErr)
			return
		}
		db.MaybeAutoArchive(ctx, idleDays, minIntervalHours, true)
	}
	if !cliTruthy(cfg["auto_prune"]) {
		return
	}
	retentionDays, err := maintenanceInt(cfg, "retention_days", 90)
	if err != nil {
		skipped(err)
		return
	}
	if intervalErr != nil {
		skipped(intervalErr)
		return
	}
	minVacuumIntervalDays, err := maintenanceInt(cfg, "min_vacuum_interval_days", 30)
	if err != nil {
		skipped(err)
		return
	}
	opts := state.NewAutoPruneOptions()
	opts.RetentionDays = &retentionDays
	opts.MinIntervalHours = minIntervalHours
	opts.MinVacuumIntervalDays = minVacuumIntervalDays
	opts.Vacuum = cliTruthy(mapValueOr(cfg, "vacuum_after_prune", true))
	opts.SessionsDir = sessionsDir
	db.MaybeAutoPruneAndVacuum(ctx, opts)
}

// Starts the checkpoint store retention pass on its own goroutine so its git gc never stalls the prompt
func runCheckpointAutoMaintenance(env *tools.Env) {
	go autoPruneCheckpointsFromConfig(env)
}
