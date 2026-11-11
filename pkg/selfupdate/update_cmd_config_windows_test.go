package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestFleetConfigMigrationLiveWindows(t *testing.T) {
	active := filepath.Join(t.TempDir(), "icebath-home")
	profiles := filepath.Join(active, "profiles")
	ucWriteProfile(t, profiles, "research", 12)
	ucWriteProfile(t, profiles, "work", 25)
	if err := os.WriteFile(filepath.Join(active, "config.yaml"), []byte("_config_version: 12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	latest := config.LatestConfigVersion()
	migrated := ucSiblingSweep(t, active)
	byName := map[string]siblingMigration{}
	for _, m := range migrated {
		byName[m.Name] = m
	}
	if len(byName) != 2 || byName["research"].Name == "" || byName["work"].Name == "" {
		t.Fatalf("migrated = %+v", migrated)
	}
	if byName["research"].From != 12 || byName["research"].To != latest {
		t.Fatalf("research = %+v", byName["research"])
	}
	for _, name := range []string{"research", "work"} {
		h := filepath.Join(profiles, name)
		if got := ucDiskVersion(t, h); got != latest {
			t.Fatalf("%s version = %d", name, got)
		}
		if got := ucDiskProvider(t, h); got != "nous" {
			t.Fatalf("%s provider = %v", name, got)
		}
	}
	if got := ucDiskVersion(t, active); got != 12 {
		t.Fatalf("active version = %d", got)
	}
	again, err := migrateSiblingProfileConfigs(context.Background(), nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %+v, %v", again, err)
	}
}
