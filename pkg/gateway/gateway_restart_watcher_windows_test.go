package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Polls cond until it holds or the timeout passes
func restartWaitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

func TestRespawnStampsBreakawayAndLeavesStdioTrace(t *testing.T) {
	dir := t.TempDir()
	h := filepath.Join(dir, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	marker := filepath.Join(dir, "respawned.marker")
	old := exec.Command("cmd.exe", "/c", "exit 0")
	if err := old.Run(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	swapSeam(t, &restartWatcherBinary, func() string { return self })
	fakeGateway := []string{"cmd.exe", "/c", "(echo %" + windowsGatewayBreakawayEnv + "%)>" + marker + " & echo fake-gateway-stderr-trace 1>&2"}
	if ok, err := SpawnGatewayRestartWatcher(context.Background(), old.Process.Pid, fakeGateway, nil, ""); err != nil || !ok {
		t.Fatalf("watcher spawn returned %v %v", ok, err)
	}
	if !restartWaitFor(func() bool { _, err := os.Stat(marker); return err == nil }, 60*time.Second) {
		t.Fatal("watcher never respawned the gateway command")
	}
	raw, _ := os.ReadFile(marker)
	if stamp := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff")); stamp != "1" && stamp != "0" {
		t.Fatalf("respawned gateway must carry the breakaway stamp, got %q", stamp)
	}
	stdioLog := filepath.Join(h, "logs", "gateway-stdio.log")
	if !restartWaitFor(func() bool {
		data, err := os.ReadFile(stdioLog)
		return err == nil && strings.Contains(string(data), "fake-gateway-stderr-trace")
	}, 30*time.Second) {
		t.Fatal("respawned gateway stderr must land in logs/gateway-stdio.log")
	}
}
