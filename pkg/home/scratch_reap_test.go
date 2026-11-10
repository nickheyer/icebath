package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

func TestPruneReapsProcessLivingInIdleEntryAndSparesLiveTree(t *testing.T) {
	scratch := testScratch(t, t.TempDir())
	idle, live := filepath.Join(scratch, "idle-lane", "wt"), filepath.Join(scratch, "live-lane", "wt")
	for _, d := range []string{idle, live} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	age(t, 30, filepath.Dir(idle), idle, filepath.Dir(live), live)
	writeText(t, filepath.Join(live, "log"), "still writing")
	doomed, spared := startSleeper(t, idle), startSleeper(t, live)
	mustPrune(t, scratch, 1)
	if !exitedWithin(doomed, 10*time.Second) {
		t.Fatal("process living in the idle entry was not reaped")
	}
	if exitedWithin(spared, 0) {
		t.Fatal("process in the live tree was reaped")
	}
	if _, err := os.Stat(filepath.Dir(idle)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("idle entry survived")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live tree removed")
	}
}

// Writes entries as the JSON array a ledger or checkpoint file holds
func writeLedger(t *testing.T, path string, entries ...string) {
	t.Helper()
	writeText(t, path, "["+strings.Join(entries, ",")+"]")
}

// Checkpoint entry the process registry writes for a host pid
func checkpointEntry(t *testing.T, pid int) string {
	start, ok := processStartTime(pid)
	if !ok {
		t.Fatalf("no start time for %d", pid)
	}
	return fmt.Sprintf(`{"pid": %d, "pid_scope": "host", "host_start_time": %d}`, pid, start)
}

// Whether pid is gone or a zombie awaiting its parent
func goneOrZombie(pid int) bool { return !proc.Exists(pid) || proc.Zombie(pid) }

func TestPruneKeepsEntryARegisteredHermesProcessWorksInButReapsOrphans(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("orphan reparenting is a POSIX process model")
	}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	scratch := testScratch(t, h)
	ownedDir, orphanDir := filepath.Join(scratch, "live-worker"), filepath.Join(scratch, "dead-lane")
	for _, d := range []string{ownedDir, orphanDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	worker := startSleeper(t, ownedDir)
	pidfile := filepath.Join(h, "orphan.pid")
	lane := testChild("lane", pidfile)
	lane.Dir = orphanDir
	if err := lane.Start(); err != nil {
		t.Fatal(err)
	}
	lanePID := lane.Process.Pid
	writeLedger(t, filepath.Join(h, "processes.json"), checkpointEntry(t, worker.Process.Pid), checkpointEntry(t, lanePID))
	if err := lane.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidfile)
	orphan, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || orphan <= 0 {
		t.Fatalf("orphan pid unreadable: %v", err)
	}
	defer func() { _ = proc.KillPID(orphan) }()
	age(t, 30, ownedDir, orphanDir)
	mustPrune(t, scratch, 1)
	if _, err := os.Stat(ownedDir); err != nil {
		t.Fatal("entry of a registered worker removed")
	}
	if _, err := os.Stat(orphanDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("orphan entry survived")
	}
	if exitedWithin(worker, 0) {
		t.Fatal("registered worker was reaped")
	}
	waitUntil(t, 10*time.Second, "orphan reaped", func() bool { return goneOrZombie(orphan) })
	if log := pruneLog(t, h); !strings.Contains(log, "kept "+pathRepr(ownedDir)+" — live Icebath process pid=") {
		t.Fatal(log)
	}
}

func TestPruneKeepsEntryThePruningSessionsOwnLineageWorksIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows refuses to delete a directory that is a process cwd")
	}
	scratch := testScratch(t, t.TempDir())
	session := filepath.Join(scratch, "session-cwd")
	if err := os.Mkdir(session, 0o755); err != nil {
		t.Fatal(err)
	}
	age(t, 30, session)
	t.Chdir(session)
	mustPrune(t, scratch, 0)
	if _, err := os.Stat(session); err != nil {
		t.Fatal("entry the pruning session works in was removed")
	}
}

func TestPruneOwnerLookupSurvivesSymlinkedScratchAndDropsOrphanedMCPHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlinked scratch")
	}
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	elsewhere := filepath.Join(tmp, "other-disk", "scratch")
	for _, d := range []string{filepath.Join(h, "cache"), elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	scratch := filepath.Join(h, "cache", "scratch")
	if err := os.Symlink(elsewhere, scratch); err != nil {
		t.Fatal(err)
	}
	ownedDir, helperDir := filepath.Join(scratch, "live-worker"), filepath.Join(scratch, "orphan-helper")
	for _, d := range []string{ownedDir, helperDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	worker := startSleeper(t, ownedDir)
	helper := startSleeper(t, helperDir)
	spawner := testChild("sleep")
	if err := spawner.Start(); err != nil {
		t.Fatal(err)
	}
	spawnerBirth, _ := proc.Birth(spawner.Process.Pid)
	helperBirth, _ := proc.Birth(helper.Process.Pid)
	_ = spawner.Process.Kill()
	_ = spawner.Wait()
	writeLedger(t, filepath.Join(RootOf(h), "spawn-ledger.json"), fmt.Sprintf(
		`{"pid": %d, "create_time": %s, "purpose": "mcp-helper", "spawner_pid": %d, "spawner_create": %s}`,
		helper.Process.Pid, strconv.FormatFloat(helperBirth, 'g', -1, 64), spawner.Process.Pid, strconv.FormatFloat(spawnerBirth, 'g', -1, 64)))
	writeLedger(t, filepath.Join(h, "processes.json"), checkpointEntry(t, worker.Process.Pid))
	ledger, _ := readJSONList(filepath.Join(RootOf(h), "spawn-ledger.json"))
	if len(ledger) != 1 {
		t.Fatal("ledger unreadable")
	}
	age(t, 30, ownedDir, helperDir)
	mustPrune(t, scratch, 1)
	if _, err := os.Stat(ownedDir); err != nil {
		t.Fatal("registered worker entry under a symlinked scratch removed")
	}
	if _, err := os.Stat(helperDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("orphaned mcp-helper entry kept")
	}
	if exitedWithin(worker, 0) {
		t.Fatal("registered worker under a symlinked scratch was reaped")
	}
	if !exitedWithin(helper, 10*time.Second) {
		t.Fatal("orphaned mcp-helper was kept")
	}
}

func TestRegisteredLivePIDsReadsLedgerAndCheckpoints(t *testing.T) {
	h := t.TempDir()
	self := os.Getpid()
	birth, err := proc.Birth(self)
	if err != nil {
		t.Fatal(err)
	}
	writeLedger(t, filepath.Join(RootOf(h), "spawn-ledger.json"),
		fmt.Sprintf(`{"pid": %d, "create_time": %s}`, self, strconv.FormatFloat(birth, 'g', -1, 64)),
		fmt.Sprintf(`{"pid": %d, "create_time": %s}`, os.Getppid(), strconv.FormatFloat(birth-1e6, 'g', -1, 64)),
		`{"pid": 0}`, `"junk"`)
	writeLedger(t, filepath.Join(h, "processes.json"), fmt.Sprintf(`{"pid": %d, "pid_scope": "sandbox"}`, os.Getppid()))
	got := registeredLivePIDs(h)
	if !got[self] || got[os.Getppid()] || len(got) != 1 {
		t.Fatalf("registered %v", got)
	}
}
