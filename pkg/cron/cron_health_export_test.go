package cron

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/monitoring"
)

func cronHealthMetric(name string) any {
	for _, m := range monitoring.BuildCronHealthSnapshot(context.Background()).Metrics {
		if m.Name == name {
			return m.Value
		}
	}
	return nil
}

func TestCronStoreWritabilityIsExported(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	storeHealth.Lock()
	prevDegraded, prevRecovered, prevListener := storeHealth.degraded, storeHealth.recovered, storeHealth.listener
	storeHealth.degraded, storeHealth.recovered, storeHealth.listener = map[string]*StoreDegraded{}, map[string]*StoreDegraded{}, nil
	storeHealth.Unlock()
	t.Cleanup(func() {
		registerTickedHomes(nil)
		storeHealth.Lock()
		storeHealth.degraded, storeHealth.recovered, storeHealth.listener = prevDegraded, prevRecovered, prevListener
		storeHealth.Unlock()
	})
	registerTickedHomes(nil)
	quiet := slog.New(slog.DiscardHandler)
	full := &os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}
	next := "2026-06-22T12:00:00+00:00"
	noteUnwritableAt(quiet, filepath.Join(h, "cron"), full, "x", "advance", &Job{ID: "job", NextRunAt: &next})
	if w, s := cronHealthMetric("hermes.cron.store.writable"), cronHealthMetric("hermes.cron.store.skipped_runs"); w != 0 || s != 1 {
		t.Fatalf("degraded writable %v skipped %v", w, s)
	}
	storeHealth.Lock()
	clear(storeHealth.degraded)
	storeHealth.Unlock()
	if w := cronHealthMetric("hermes.cron.store.writable"); w != 1 {
		t.Fatalf("cleared writable %v", w)
	}
	other := filepath.Join(t.TempDir(), "other")
	registerTickedHomes([]string{other})
	noteUnwritableAt(quiet, filepath.Join(other, "cron"), full, "x", "scan")
	if w := cronHealthMetric("hermes.cron.store.writable"); w != 0 {
		t.Fatalf("secondary store writable %v", w)
	}
	registerTickedHomes(nil)
	if w := cronHealthMetric("hermes.cron.store.writable"); w != 1 {
		t.Fatalf("departed profile writable %v", w)
	}
}
