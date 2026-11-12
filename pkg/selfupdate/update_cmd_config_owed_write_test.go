package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestActiveConfigWriteFailureStillMigratesSiblingsAndRestoresSnapshots(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	sibling := filepath.Join(root, "profiles", "sibling")
	jobsText := `{"jobs": [{"id": "nightly", "name": "nightly", "schedule": "0 3 * * *", "prompt": "report"}]}`
	var jobs any
	if err := json.Unmarshal([]byte(jobsText), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{root, sibling} {
		if err := os.MkdirAll(filepath.Join(h, "cron"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("_config_version: 48\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), []byte(jobsText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	label := "pre-update"
	activeSnapshot, err := backup.CreateQuickSnapshot(&label, root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	siblingSnapshots, err := backup.CreatePreUpdateSnapshotsAllProfiles(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	savedSnapshots, savedMigrate := lastSiblingSnapshots, configMigrate
	t.Cleanup(func() { lastSiblingSnapshots, configMigrate = savedSnapshots, savedMigrate })
	lastSiblingSnapshots = siblingSnapshots
	for _, h := range []string{root, sibling} {
		if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), []byte(`{"jobs": []}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configMigrate = func(ctx context.Context, h string, interactive, quiet bool) (*config.MigrationResults, error) {
		if h == root {
			return nil, errors.New("disk full")
		}
		return config.MigrateConfig(ctx, h, interactive, quiet)
	}
	err = checkAndApplyConfigMigration(context.Background(), io.Discard, false, false, &activeSnapshot)
	if err == nil || !strings.Contains(err.Error(), "was not written: disk full") {
		t.Fatalf("err = %v", err)
	}
	for _, h := range []string{root, sibling} {
		raw, err := os.ReadFile(filepath.Join(h, "cron", "jobs.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, jobs) {
			t.Fatalf("%s jobs.json = %s", h, raw)
		}
	}
	if got := ucDiskVersion(t, root); got != 48 {
		t.Fatalf("active version = %d", got)
	}
	t.Setenv("ICEBATH_HOME", sibling)
	siblingVersion, latest, err := config.CheckConfigVersion(home.WithOverride(context.Background(), sibling), sibling, false)
	if err != nil {
		t.Fatal(err)
	}
	if siblingVersion != latest || latest <= 48 {
		t.Fatalf("sibling version %d latest %d", siblingVersion, latest)
	}
}
