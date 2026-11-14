package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestSessionDetailStampsTheSameAnswerForCronRunsOnly(t *testing.T) {
	root := t.TempDir()
	defaultHome := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(filepath.Join(defaultHome, "cron"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultHome, "config.yaml"), []byte("model: test-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", defaultHome)
	ctx := context.Background()
	jobID := "detail"
	record, err := cron.CreateExecutionIn(ctx, defaultHome, jobID, "test")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := cron.MarkExecutionRunningIn(ctx, defaultHome, record["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if execution == nil {
		execution = record
	}
	claimed, ok := cronISOToEpoch(execution["claimed_at"])
	if !ok {
		t.Fatalf("claimed_at %v did not parse", execution["claimed_at"])
	}
	row := func() *pyjson.Object {
		o := pyjson.NewObject()
		o.Set("id", "cron_"+jobID+"_20260929_120000")
		o.Set("source", "cron")
		o.Set("started_at", claimed+1)
		o.Set("last_active", claimed+1)
		o.Set("ended_at", nil)
		o.Set("archived", false)
		return o
	}
	owned, err := cronRunSchedulerOwned(ctx, row(), "")
	if err != nil || owned == nil || !*owned {
		t.Fatalf("open run: owned=%v err=%v, want true", owned, err)
	}
	ended := row()
	ended.Set("ended_at", claimed+5)
	if owned, err = cronRunSchedulerOwned(ctx, ended, ""); err != nil || owned == nil || *owned {
		t.Fatalf("ended run: owned=%v err=%v, want false", owned, err)
	}
	desktop := row()
	desktop.Set("source", "desktop")
	if owned, err = cronRunSchedulerOwned(ctx, desktop, ""); err != nil || owned != nil {
		t.Fatalf("desktop row: owned=%v err=%v, want nil", owned, err)
	}
}
