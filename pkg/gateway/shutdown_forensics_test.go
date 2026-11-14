package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const forensicsArgvCanary = "lin_api_CANARY_SHUTDOWN_FORENSICS_9f3a2c"

// Live child whose argv carries a token shaped value, killed when the test ends
func childWithSecretArgv(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30", "--token="+forensicsArgvCanary)
	if runtime.GOOS == "linux" {
		cmd = exec.Command("sh", "-c", "sleep 30", "sh", "--token="+forensicsArgvCanary)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestSnapshotShutdownContextDetectsTakeoverMarkerForSelf(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	marker := filepath.Join(dir, ".gateway-takeover.json")
	if err := os.WriteFile(marker, fmt.Appendf(nil, `{"target_pid": %d, "replacer_pid": 99999}`, os.Getpid()), 0o644); err != nil {
		t.Fatal(err)
	}
	sig := int(syscall.SIGTERM)
	ctx := SnapshotShutdownContext(&sig)
	if _, ok := ctx.Get("takeover_marker"); !ok {
		t.Fatal("takeover_marker missing")
	}
	if v, _ := ctx.Get("takeover_marker_for_self"); v != true {
		t.Fatalf("takeover_marker_for_self = %v", v)
	}
}

func TestFormattersContextAsJSONHandlesUnserialisableValues(t *testing.T) {
	ctx := pyjson.NewObject()
	ctx.Set("signal", "SIGTERM")
	ctx.Set("weird", struct{ x chan int }{})
	var decoded map[string]any
	if err := json.Unmarshal([]byte(ContextAsJSON(ctx)), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["signal"] != "SIGTERM" {
		t.Fatalf("signal = %v", decoded["signal"])
	}
	if _, ok := decoded["weird"]; !ok {
		t.Fatal("weird missing")
	}
}

func TestArgvFreePersistenceSnapshotAndLogLineIdentifyProcessWithoutArgv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	child := childWithSecretArgv(t)
	summary := procSummary(child.Process.Pid)
	if v, _ := summary.Get("pid"); v != int64(child.Process.Pid) {
		t.Fatalf("pid = %v", v)
	}
	if v, _ := summary.Get("name"); v == nil || v == "" {
		t.Fatal("name missing")
	}
	if _, ok := summary.Get("cmdline"); ok {
		t.Fatal("cmdline persisted")
	}
	sig := int(syscall.SIGTERM)
	ctx := SnapshotShutdownContext(&sig)
	ctx.Set("parent", summary)
	line := FormatContextForLog(ctx)
	if strings.Contains(line, forensicsArgvCanary) || strings.Contains(ContextAsJSON(ctx), forensicsArgvCanary) {
		t.Fatal("argv canary leaked")
	}
	if !strings.Contains(line, fmt.Sprintf("parent_pid=%d", child.Process.Pid)) {
		t.Fatalf("line = %s", line)
	}
}

// Asserts the detached diagnostic lands its listing with the ps header first
func assertDiagnosticWritten(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "diag.log")
	pid, ok := SpawnAsyncDiagnostic(logPath, "SIGTERM", 3.0)
	if !ok || pid <= 0 {
		t.Fatalf("pid = %d ok = %v", pid, ok)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), "=== end ===") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	if !strings.Contains(contents, "shutdown diagnostic") || !strings.Contains(contents, "SIGTERM") {
		t.Fatalf("contents = %s", contents)
	}
	lines := strings.Split(contents, "\n")
	at := -1
	for i, l := range lines {
		if l == "--- ps (top 60 by cpu, comm only) ---" {
			at = i
			break
		}
	}
	if at < 0 || at+1 >= len(lines) {
		t.Fatalf("no ps section: %s", contents)
	}
	if fields := strings.Fields(lines[at+1]); len(fields) < 2 || fields[0] != "PID" || fields[1] != "PPID" {
		t.Fatalf("ps column header must lead the listing, not sort as a 0.0-cpu row: %q", lines[at+1])
	}
}

func TestSpawnAsyncDiagnosticSpawnsSubprocessAndWritesOutput(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	assertDiagnosticWritten(t)
}

func TestSpawnAsyncDiagnosticSpawnsWithoutGnuTimeoutOnMacos(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macos only")
	}
	assertDiagnosticWritten(t)
}

func TestSpawnAsyncDiagnosticDiagnosticLogOmitsChildArgvAndIsOwnerOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	childWithSecretArgv(t)
	logPath := filepath.Join(t.TempDir(), "diag.log")
	if err := os.WriteFile(logPath, []byte("prior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(logPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := SpawnAsyncDiagnostic(logPath, "SIGTERM", 5.0); !ok {
		t.Fatal("diagnostic did not start")
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "=== end ===") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "shutdown diagnostic") || strings.Contains(string(data), forensicsArgvCanary) {
		t.Fatalf("contents = %s", data)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestParseSystemdDurationSeconds(t *testing.T) {
	if got := ParseSystemdDurationToUS("90s"); got == nil || got.Int64() != 90*1_000_000 {
		t.Fatalf("got %v", got)
	}
}

func TestParseSystemdDurationMinutes(t *testing.T) {
	if got := ParseSystemdDurationToUS("3min"); got == nil || got.Int64() != 180*1_000_000 {
		t.Fatalf("got %v", got)
	}
}
