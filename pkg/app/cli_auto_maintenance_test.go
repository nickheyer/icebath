package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// maintenanceDouble records the maintenance calls of a store whose one time repairs already ran
type maintenanceDouble struct {
	pruneCalls []state.AutoPruneOptions
}

func (d *maintenanceDouble) GetMeta(context.Context, string) (string, bool, error) {
	return "already-done", true, nil
}
func (d *maintenanceDouble) SetMeta(context.Context, string, string) error { return nil }
func (d *maintenanceDouble) PruneEmptyGhostSessions(context.Context, string) (int, error) {
	return 0, nil
}
func (d *maintenanceDouble) FinalizeOrphanedCompressionSessions(context.Context) (int64, error) {
	return 0, nil
}
func (d *maintenanceDouble) MaybeAutoArchive(context.Context, float64, float64, bool) state.AutoArchiveResult {
	return state.AutoArchiveResult{}
}
func (d *maintenanceDouble) MaybeAutoPruneAndVacuum(_ context.Context, o state.AutoPruneOptions) state.AutoPruneResult {
	d.pruneCalls = append(d.pruneCalls, o)
	return state.AutoPruneResult{}
}

func withSessionsConfig(t *testing.T, sessions map[string]any) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	prev := loadMaintenanceConfig
	loadMaintenanceConfig = func(context.Context) (map[string]any, error) { return map[string]any{"sessions": sessions}, nil }
	t.Cleanup(func() { loadMaintenanceConfig = prev })
	return h
}

func TestExplicitAutoPruneFalseIsRespected(t *testing.T) {
	withSessionsConfig(t, map[string]any{"auto_prune": false, "retention_days": 90})
	db := &maintenanceDouble{}
	runStateDBAutoMaintenance(context.Background(), db)
	if len(db.pruneCalls) != 0 {
		t.Fatalf("an explicit opt out still pruned: %+v", db.pruneCalls)
	}
}

func TestNegativeRetentionDaysInConfigDeletesNothing(t *testing.T) {
	h := withSessionsConfig(t, map[string]any{"auto_prune": true, "retention_days": -1})
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "ended", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("ended", "done"); err != nil {
		t.Fatal(err)
	}
	runStateDBAutoMaintenance(ctx, db)
	if row, err := db.GetSession(ctx, "ended"); err != nil || row == nil {
		t.Fatalf("the ended session was deleted: %v %v", row, err)
	}
}

func TestCliAutoMaintenanceForwardsVacuumInterval(t *testing.T) {
	h := withSessionsConfig(t, map[string]any{"auto_prune": true, "retention_days": 90, "vacuum_after_prune": true,
		"min_interval_hours": 24, "min_vacuum_interval_days": 17})
	db := &maintenanceDouble{}
	runStateDBAutoMaintenance(context.Background(), db)
	if len(db.pruneCalls) != 1 {
		t.Fatalf("prune calls %+v", db.pruneCalls)
	}
	got := db.pruneCalls[0]
	if got.RetentionDays == nil || *got.RetentionDays != 90 || got.MinIntervalHours != 24 || got.MinVacuumIntervalDays != 17 || !got.Vacuum ||
		got.SessionsDir != filepath.Join(h, "sessions") {
		t.Fatalf("forwarded %+v", got)
	}
}
