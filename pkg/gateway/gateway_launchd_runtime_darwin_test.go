package gateway

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const launchdJobProbeEnv = "ICEBATH_TEST_LAUNCHD_JOB_PROBE"

// Runs the launchd job probe child before the test runner when the probe env names its directory
func init() {
	if dir := os.Getenv(launchdJobProbeEnv); dir != "" {
		os.Exit(runLaunchdJobProbe(dir))
	}
}

// Writes parent pid, XPC_SERVICE_NAME and PROBE_MARKER then waits up to thirty seconds for the release file
func runLaunchdJobProbe(dir string) int {
	data, _ := json.Marshal(map[string]any{"ppid": os.Getppid(), "xpc": os.Getenv("XPC_SERVICE_NAME"), "marker": os.Getenv("PROBE_MARKER")})
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o644); err != nil {
		return 3
	}
	for range 300 {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0
}

// Output of ps for one pid and column
func psColumn(pid int, column string) string {
	out, _ := exec.Command("ps", "-o", column+"=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// Polls pred every tenth of a second until it holds or timeout passes
func waitUntilTrue(pred func() bool, timeout time.Duration) bool {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if pred() {
			return true
		}
	}
	return pred()
}

func TestGatewayJobIsALaunchdOsascriptChildThatReadsAsUnsupervised(t *testing.T) {
	tmp := t.TempDir()
	report, release := filepath.Join(tmp, "report.json"), filepath.Join(tmp, "release")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	env["PROBE_MARKER"], env[launchdJobProbeEnv] = "env-carried", tmp
	logPath := filepath.Join(tmp, "gateway-stdio.log")
	job, err := startGatewayJob(context.Background(), []string{exe, "-test.run=^$"}, env, tmp, logPath, pythread.Monotonic()+60)
	if err != nil || job == nil {
		t.Fatalf("launchd refused the job in this user's GUI domain: %v", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = os.WriteFile(release, nil, 0o644)
		}
	}()
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	waitUntilTrue(func() bool { _, done := job.Poll(); return exists(report) || done }, 30*time.Second)
	if !exists(report) {
		logText, _ := os.ReadFile(logPath)
		state, _ := exec.Command("launchctl", "print", job.(*launchdRuntimeJob).target).Output()
		t.Fatalf("tmp=%s\nlog=%q\nlaunchctl print:\n%.3000s", tmp, logText, state)
	}
	var seen struct {
		Ppid   int     `json:"ppid"`
		Xpc    *string `json:"xpc"`
		Marker *string `json:"marker"`
	}
	raw, _ := os.ReadFile(report)
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	if cmd := psColumn(seen.Ppid, "command"); !strings.HasPrefix(cmd, "/usr/bin/osascript") {
		t.Fatalf("wrapper command = %q", cmd)
	}
	if ppid := psColumn(seen.Ppid, "ppid"); ppid != "1" {
		t.Fatalf("wrapper parent = %q", ppid)
	}
	if seen.Marker == nil || *seen.Marker != "env-carried" {
		t.Fatalf("marker = %v", seen.Marker)
	}
	if seen.Xpc == nil || *seen.Xpc != "0" {
		t.Fatalf("XPC_SERVICE_NAME = %v", seen.Xpc)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(tmp, ".*.plist")); len(leftovers) != 0 {
		t.Fatalf("the plist holding the launch env must not stay on disk: %v", leftovers)
	}
	if _, done := job.Poll(); done {
		t.Fatal("job finished before release")
	}
	_ = os.WriteFile(release, nil, 0o644)
	finished = true
	if !waitUntilTrue(func() bool { _, done := job.Poll(); return done }, 30*time.Second) {
		t.Fatal("job never finished after release")
	}
	if status, _ := job.Poll(); status != 0 {
		t.Fatalf("exit status = %d", status)
	}
}
