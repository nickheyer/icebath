package cron

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Empties the outage registry and drives its clocks and probe from the returned knobs
func freshStoreHealth(t *testing.T) (wall, mono *float64, probe *error, events *[]string) {
	t.Helper()
	savedHooks := storeHealthHooks
	storeHealth.Lock()
	storeHealth.degraded, storeHealth.recovered, storeHealth.listener = map[string]*StoreDegraded{}, map[string]*StoreDegraded{}, nil
	storeHealth.Unlock()
	t.Cleanup(func() {
		storeHealthHooks = savedHooks
		SetStoreTransitionListener(nil)
		storeHealth.Lock()
		storeHealth.degraded, storeHealth.recovered = map[string]*StoreDegraded{}, map[string]*StoreDegraded{}
		storeHealth.Unlock()
	})
	w, m := 1000.0, 50.0
	var p error
	ev := &[]string{}
	storeHealthHooks.wall = func() float64 { return w }
	storeHealthHooks.mono = func() float64 { return m }
	storeHealthHooks.probe = func(string) error { return p }
	SetStoreTransitionListener(func(event string, r *StoreDegraded) { *ev = append(*ev, event) })
	return &w, &m, &p, ev
}

var enospc = &fs.PathError{Op: "write", Path: "jobs.json", Err: syscall.ENOSPC}

func TestDescribeStoreError(t *testing.T) {
	if got := DescribeStoreError(enospc); got != "ENOSPC: No space left on device" {
		t.Fatalf("got %q", got)
	}
	if got := DescribeStoreError(errors.New("odd failure")); got != "odd failure" {
		t.Fatalf("got %q", got)
	}
}

func TestOutageEntersOnceAndRecoversThroughAnySave(t *testing.T) {
	wall, _, _, events := freshStoreHealth(t)
	s, buf := newTestStore(t)
	due := &Job{ID: "a", NextRunAt: strPtr("2026-01-01T00:00:00+00:00")}
	storeHealthNoteUnwritable(s, enospc, "due-scan repairs not persisted", "scan", due)
	storeHealthNoteUnwritable(s, enospc, "x", "scan", due)
	storeHealthNoteUnwritable(s, enospc, "fire claim not persisted", "claim", &Job{ID: "b"})
	if strings.Count(buf.String(), "is unwritable") != 1 || strings.Count(buf.String(), "still unwritable") != 1 {
		t.Fatalf("log %s", buf.String())
	}
	rec := DegradedRecord(s.cronDir)
	if rec == nil || rec.SkippedRuns() != 2 || rec.Since != 1000 || rec.Error != "ENOSPC: No space left on device" || rec.lastProbe == nil {
		t.Fatalf("record %+v", rec)
	}
	if f := rec.NoticeFields(); f["skipped"] != 2 || !strings.Contains(f["fix"].(string), s.cronDir) || f["store"] != StoreKey(s.home) {
		t.Fatalf("notice %v", f)
	}
	*wall = 1500
	if !OutageCovers(s.cronDir, 1200, 0) || OutageCovers(s.cronDir, 900, 60) {
		t.Fatal("live outage window")
	}
	if err := s.SaveJobs(context.Background(), nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if DegradedRecord(s.cronDir) != nil || len(DegradedRecords()) != 0 {
		t.Fatal("save did not end the outage")
	}
	if !OutageCovers(s.cronDir, 1200, 0) || OutageCovers(s.cronDir, 1600, 0) {
		t.Fatal("recovered window lost before a scan save")
	}
	*wall = 1700
	storeHealthNoteUnwritable(s, enospc, "x", "scan")
	if rec := DegradedRecord(s.cronDir); rec.Since != 1000 {
		t.Fatalf("re entry within the window restarted since %v", rec.Since)
	}
	s.SaveJobs(context.Background(), nil, nil, false)
	storeHealthEndRecoveryWindow(s, s.cronDir)
	if OutageCovers(s.cronDir, 1200, 0) {
		t.Fatal("window survived the scan save")
	}
	if strings.Join(*events, ",") != "unwritable,recovered,unwritable,recovered" {
		t.Fatalf("events %v", *events)
	}
}

func TestListenerPanicNeverBreaksTheTick(t *testing.T) {
	freshStoreHealth(t)
	SetStoreTransitionListener(func(string, *StoreDegraded) { panic("listener down") })
	s, buf := newTestStore(t)
	storeHealthNoteUnwritable(s, enospc, "x", "scan")
	if !strings.Contains(buf.String(), "transition listener failed") {
		t.Fatalf("log %s", buf.String())
	}
	ForgetStores([]string{StoreKey(s.home)})
	if DegradedRecord(s.cronDir) != nil {
		t.Fatal("forgotten store still degraded")
	}
}

func TestDispatchBlockedThrottlesThenReprobes(t *testing.T) {
	_, mono, probe, _ := freshStoreHealth(t)
	s, _ := newTestStore(t)
	storeHealthNoteUnwritable(s, enospc, "x", "scan")
	if s.dispatchBlocked([]*Job{{ID: "a"}}) {
		t.Fatal("dispatch held before any dispatch write was tried")
	}
	storeHealthNoteUnwritable(s, enospc, "x", "claim")
	if !s.dispatchBlocked([]*Job{{ID: "a"}}) {
		t.Fatal("throttled dispatch not held")
	}
	*mono += ProbeIntervalSeconds
	*probe = &fs.PathError{Op: "open", Path: "x", Err: syscall.EROFS}
	if !s.dispatchBlocked([]*Job{{ID: "c"}}) || DegradedRecord(s.cronDir).Error != "EROFS: Read-only file system" {
		t.Fatal("failing re probe not held")
	}
	*mono += ProbeIntervalSeconds
	*probe = nil
	if s.dispatchBlocked([]*Job{{ID: "d"}}) {
		t.Fatal("passing re probe still held")
	}
	if n := DegradedRecord(s.cronDir).SkippedRuns(); n != 2 {
		t.Fatalf("skipped %d", n)
	}
}

func TestRecheckIdleClearsARecoveredStore(t *testing.T) {
	_, mono, probe, events := freshStoreHealth(t)
	s, _ := newTestStore(t)
	storeHealthNoteUnwritable(s, enospc, "x", "claim")
	*probe = enospc
	*mono += ProbeIntervalSeconds
	if err := s.recheckIdle(context.Background()); err != nil || DegradedRecord(s.cronDir) == nil {
		t.Fatalf("failing probe cleared the store: %v", err)
	}
	if err := s.recheckIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	*probe = nil
	*mono += ProbeIntervalSeconds
	if err := s.recheckIdle(context.Background()); err != nil || DegradedRecord(s.cronDir) != nil {
		t.Fatalf("passing probe left the store degraded: %v", err)
	}
	if strings.Join(*events, ",") != "unwritable,recovered" {
		t.Fatalf("events %v", *events)
	}
}

func TestProbeStoreFindsUnwritableTargets(t *testing.T) {
	s, _ := newTestStore(t)
	if err := ProbeStore(filepath.Join(s.home, "missing")); err != nil {
		t.Fatalf("missing dir probed: %v", err)
	}
	if err := s.SaveJobs(context.Background(), []*Job{{ID: "a"}}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := ProbeStore(s.cronDir); err != nil || ProbeReport(s.cronDir) != nil {
		t.Fatalf("healthy store failed the probe: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(s.cronDir, ".probe_*")); len(left) != 0 {
		t.Fatalf("probe files left %v", left)
	}
	if os.Geteuid() == 0 {
		return
	}
	lock := filepath.Join(s.cronDir, ".tick.lock")
	os.WriteFile(lock, nil, 0o400)
	var pe *fs.PathError
	if err := ProbeStore(s.cronDir); !errors.As(err, &pe) || pe.Path != lock || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("unopenable tick lock not reported: %v", err)
	}
	os.Remove(lock)
	os.Chmod(s.cronDir, 0o500)
	defer os.Chmod(s.cronDir, 0o700)
	report := ProbeReport(s.cronDir)
	if report == nil || !strings.HasPrefix(report["error"].(string), "EACCES") || report["since"] == "unknown" {
		t.Fatalf("report %v", report)
	}
	if free, ok := FreeBytes(s.cronDir); !ok || free == 0 {
		t.Fatalf("free bytes %d %v", free, ok)
	}
}
