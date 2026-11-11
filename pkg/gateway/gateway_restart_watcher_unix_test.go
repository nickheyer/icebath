//go:build !windows

package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Pid of a child that already exited and was reaped
func restartDeadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestRestartWatcherRelaunchesFromAnInterpreterWithoutSitePackages(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "respawned")
	h := filepath.Join(dir, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Chdir(dir)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	swapSeam(t, &restartWatcherBinary, func() string { return self })
	relaunch := []string{"/bin/sh", "-c", "printf ok > '" + marker + "'"}
	host := false
	if ok, err := SpawnGatewayRestartWatcher(context.Background(), restartDeadPid(t), relaunch, &host, ""); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	info, err := os.Stat(marker)
	if err != nil {
		stdio, _ := os.ReadFile(filepath.Join(h, "logs", "gateway-stdio.log"))
		t.Fatalf("the restart watcher never relaunched the gateway command; stdio log:\n%s", stdio)
	}
	if info.Size() != 2 {
		t.Fatalf("size %d", info.Size())
	}
}
