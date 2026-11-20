package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
)

// Store the removed shared metrics counters lived in under a home
func sharedMetricsStore(h string) string { return filepath.Join(h, "telemetry", "shared_metrics") }

// Removes the shared metrics store and its telemetry parent once that is left empty
func removeSharedMetricsStore(store string) error {
	if err := os.RemoveAll(store); err != nil {
		return err
	}
	parent := filepath.Dir(store)
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) > 0 {
		return err
	}
	return os.Remove(parent)
}

// Drops the telemetry section and the counter store of the removed shared metrics feature
func migrateTo61(r *migrationRun) error {
	cfg := r.read()
	_, inConfig := cfg["telemetry"]
	store := sharedMetricsStore(r.h)
	onDisk := false
	if r.localFilesAllowed() {
		if _, err := os.Lstat(store); err == nil {
			onDisk = true
		}
	}
	if !inConfig && !onDisk {
		return nil
	}
	if onDisk {
		if err := removeSharedMetricsStore(store); err != nil {
			return err
		}
	}
	var removed []string
	if inConfig {
		removed = append(removed, "telemetry")
	}
	if onDisk {
		removed = append(removed, home.Display(store))
	}
	message := "  ✓ Removed " + strings.Join(removed, " and ") + " — shared metrics are gone, icebath counts and uploads nothing."
	if !inConfig {
		r.say(message)
		return nil
	}
	delete(cfg, "telemetry")
	return r.commit(cfg, "removed telemetry", message)
}
