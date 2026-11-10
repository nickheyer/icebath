//go:build unix

package tools

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Polls cond until it holds, failing the test at the deadline
func scratchWaitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Sets the access and modification times of each path thirty hours back
func scratchAge(t *testing.T, paths ...string) {
	t.Helper()
	ancient := time.Now().Add(-30 * time.Hour)
	for _, p := range paths {
		if err := os.Chtimes(p, ancient, ancient); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneKeepsEntryARegisteredHermesProcessWorksInButReapsOrphans(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	installTestEnvHooks(t)
	ctx := t.Context()
	scratch := home.ScratchDirWithoutPrune(ctx, h)
	ownedDir, orphanDir := filepath.Join(scratch, "live-worker"), filepath.Join(scratch, "dead-lane")
	for _, d := range []string{ownedDir, orphanDir} {
		if err := os.Mkdir(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	sleeper := "sleep 60"
	pidfile := filepath.Join(h, "orphan.pid")
	registry := NewProcessRegistry()
	worker, err := registry.SpawnLocal(ctx, nil, sleeper, SpawnLocalOptions{Cwd: ownedDir})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.KillProcess(ctx, nil, worker.ID, "", true)
	lane, err := registry.SpawnLocal(ctx, nil, sleeper+" >/dev/null 2>&1 </dev/null & echo $! > "+shQuote(pidfile), SpawnLocalOptions{Cwd: orphanDir})
	if err != nil {
		t.Fatal(err)
	}
	orphan := 0
	scratchWaitUntil(t, 15*time.Second, "lane shell exits after forking its orphan", func() bool {
		registry.Poll(ctx, nil, lane.ID)
		lane.mu.Lock()
		exited := lane.Exited
		lane.mu.Unlock()
		if !exited {
			return false
		}
		data, err := os.ReadFile(pidfile)
		if err != nil {
			return false
		}
		orphan, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(data), "\uFEFF")))
		return orphan > 0
	})
	defer func() { _ = proc.KillPID(orphan) }()
	scratchAge(t, ownedDir, orphanDir)
	if got, err := home.PruneScratchDir(ctx, scratch, home.ScratchMaxIdleHours); err != nil || got != 1 {
		t.Fatalf("PruneScratchDir = %d, %v, want 1", got, err)
	}
	if _, err := os.Lstat(ownedDir); err != nil {
		t.Fatalf("entry of the registered worker removed: %v", err)
	}
	if _, err := os.Lstat(orphanDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphan entry survived: %v", err)
	}
	if _, exited := worker.Process.Poll(); exited {
		t.Fatal("registered Hermes worker was reaped")
	}
	scratchWaitUntil(t, 10*time.Second, "orphan reaped", func() bool { return !proc.Exists(orphan) || proc.Zombie(orphan) })
	data, err := os.ReadFile(filepath.Join(h, "logs", "scratch-prune.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := strings.TrimPrefix(string(data), "\uFEFF")
	if !strings.Contains(log, "kept "+pyrepr.StrRepr(ownedDir)+" \u2014 live Icebath process pid=") {
		t.Fatal(log)
	}
}

func TestPruneOwnerLookupSurvivesSymlinkedScratchAndDropsOrphanedMcpHelpers(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	if err := os.Mkdir(h, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	installTestEnvHooks(t)
	ctx := t.Context()
	elsewhere := filepath.Join(tmp, "other-disk", "scratch")
	if err := os.MkdirAll(elsewhere, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h, "cache"), 0o777); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(h, "cache", "scratch")
	if err := os.Symlink(elsewhere, scratch); err != nil {
		t.Fatal(err)
	}
	ownedDir, helperDir := filepath.Join(scratch, "live-worker"), filepath.Join(scratch, "orphan-helper")
	for _, d := range []string{ownedDir, helperDir} {
		if err := os.Mkdir(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	registry := NewProcessRegistry()
	worker, err := registry.SpawnLocal(ctx, nil, "sleep 60", SpawnLocalOptions{Cwd: ownedDir})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.KillProcess(ctx, nil, worker.ID, "", true)
	helper := exec.Command("sleep", "60")
	helper.Dir = helperDir
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	helperDone := make(chan struct{})
	go func() {
		_ = helper.Wait()
		close(helperDone)
	}()
	defer func() {
		_ = helper.Process.Kill()
		<-helperDone
	}()
	spawner := exec.Command(os.Args[0])
	spawner.Env = append(os.Environ(), "ICEBATH_HOME="+h, scratchSpawnerEnv+"="+strconv.Itoa(helper.Process.Pid))
	if err := spawner.Start(); err != nil {
		t.Fatal(err)
	}
	helperRow := func() map[string]any {
		for _, e := range LedgerEntries(LedgerOptions{AllInstalls: true}) {
			if pid, ok := entryInt(e, "pid"); ok && pid == helper.Process.Pid {
				return e
			}
		}
		return nil
	}
	scratchWaitUntil(t, 20*time.Second, "spawner registered the mcp-helper", func() bool { return helperRow() != nil })
	_ = spawner.Process.Kill()
	_ = spawner.Wait()
	if dead, known := SpawnerIsDead(helperRow()); !dead || !known {
		t.Fatalf("spawner dead=%v known=%v, want a provably dead spawner", dead, known)
	}
	scratchAge(t, ownedDir, helperDir)
	if got, err := home.PruneScratchDir(ctx, scratch, home.ScratchMaxIdleHours); err != nil || got != 1 {
		t.Fatalf("PruneScratchDir = %d, %v, want 1", got, err)
	}
	if _, err := os.Lstat(ownedDir); err != nil {
		t.Fatalf("registered worker entry under a symlinked scratch removed: %v", err)
	}
	if _, err := os.Lstat(helperDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("orphaned mcp-helper entry kept: %v", err)
	}
	if _, exited := worker.Process.Poll(); exited {
		t.Fatal("registered worker under a symlinked scratch was reaped")
	}
	select {
	case <-helperDone:
	case <-time.After(10 * time.Second):
		t.Fatal("orphaned mcp-helper was kept")
	}
}
