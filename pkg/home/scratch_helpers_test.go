package home

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Child modes of the test binary standing in for the shells and launchers the scratch tests spawn
const testModeEnv = "ICEBATH_HOME_TEST_MODE"

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	DispatchScratchPruneEntry()
	if mode := os.Getenv(testModeEnv); mode != "" {
		os.Exit(runTestMode(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// Runs one child role and returns its exit code
func runTestMode(mode string, args []string) int {
	switch mode {
	case "sleep":
		time.Sleep(60 * time.Second)
	case "launch":
		if st, err := os.Stat(EnsureScratchDir(args[0])); err != nil || !st.IsDir() {
			return 1
		}
	case "wrap", "shell":
		next := map[string]string{"wrap": "launch", "shell": "wrap"}[mode]
		cmd := testChild(next, args...)
		cmd.Dir = os.Getenv("ICEBATH_HOME_TEST_CWD")
		if err := cmd.Run(); err != nil {
			return 1
		}
		if mode == "shell" {
			time.Sleep(120 * time.Second)
		}
	case "rename":
		name := []byte{}
		fmt.Sscanf(args[0], "%x", &name)
		if err := os.WriteFile("/proc/self/comm", name, 0o644); err != nil {
			return 1
		}
		_ = os.WriteFile(args[1], nil, 0o644)
		time.Sleep(60 * time.Second)
	case "export", "tempdir":
		if mode == "export" {
			ExportScratchTmpEnv()
		}
		dir, err := pypath.Gettempdir()
		if err != nil {
			return 1
		}
		fmt.Println(dir)
		if mode == "export" {
			out, err := testChild("tempdir").Output()
			if err != nil {
				return 1
			}
			fmt.Print(string(out))
		}
	case "gateway":
		shell := testChild("sleep")
		shell.Dir = args[0]
		if err := shell.Start(); err != nil {
			return 1
		}
		_ = os.WriteFile(args[1], []byte(fmt.Sprint(shell.Process.Pid)), 0o644)
		time.Sleep(60 * time.Second)
	case "lane":
		orphan := testChild("sleep")
		if err := orphan.Start(); err != nil {
			return 1
		}
		_ = os.WriteFile(args[0], []byte(fmt.Sprint(orphan.Process.Pid)), 0o644)
	default:
		if run, ok := platformTestModes[mode]; ok {
			return run(args)
		}
		return 2
	}
	return 0
}

// The test binary re-run in a child mode with stdin closed
func testChild(mode string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), testModeEnv+"="+mode)
	return cmd
}

// Started sleeper child working in dir, killed and collected at cleanup
func startSleeper(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := testChild("sleep")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	sleeperDone[cmd] = done
	return cmd
}

// Child roles a platform test file registers at init
var platformTestModes = map[string]func(args []string) int{}

var sleeperDone = map[*exec.Cmd]chan struct{}{}

// Whether the sleeper exited within d
func exitedWithin(cmd *exec.Cmd, d time.Duration) bool {
	select {
	case <-sleeperDone[cmd]:
		return true
	case <-time.After(d):
		return false
	}
}

// Polls cond until it holds or fails the test after timeout
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Recorder in place of the detached prune launcher, the conftest scratch_prune_spawns fixture
func scratchPruneSpawns(t *testing.T) *[]string {
	t.Helper()
	spawned, restore := CaptureScratchPruneSpawns()
	t.Cleanup(restore)
	return spawned
}

// Scratch dir of h created without starting a prune
func testScratch(t *testing.T, h string) string {
	t.Helper()
	dir := scratchDir(h, false)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("scratch dir %s not created: %v", dir, err)
	}
	return dir
}

// Sets path mtime hoursAgo in the past without following symlinks where possible
func age(t *testing.T, hoursAgo float64, paths ...string) {
	t.Helper()
	when := time.Now().Add(-time.Duration(hoursAgo * float64(time.Hour)))
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

// Writes body to path creating parents
func writeText(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Contents of the prune log under home h
func pruneLog(t *testing.T, h string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, "logs", "scratch-prune.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(string(data), "\xef\xbb\xbf")
}

// Runs a prune of scratch and fails unless it removed want entries
func mustPrune(t *testing.T, scratch string, want int) {
	t.Helper()
	got, err := PruneScratchDir(scratch, ScratchMaxIdleHours)
	if err != nil || got != want {
		t.Fatalf("PruneScratchDir = %d, %v, want %d", got, err, want)
	}
}

// Replaces a package seam for the test duration
func swap[T any](t *testing.T, target *T, v T) {
	prev := *target
	*target = v
	t.Cleanup(func() { *target = prev })
}

// Idle entry with a nested file aged thirty hours
func idleEntry(t *testing.T, scratch, name string) string {
	t.Helper()
	entry := filepath.Join(scratch, name)
	writeText(t, filepath.Join(entry, "sub", "out.log"), "old output")
	age(t, 30, filepath.Join(entry, "sub", "out.log"), filepath.Join(entry, "sub"), entry)
	return entry
}
