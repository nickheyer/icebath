//go:build !windows

package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Pid record naming the holder as a gateway launched from ownerHome, its lock left a bare flock
func lookalikeRecord(t *testing.T, holder *lockHolder, binDir, ownerHome, pidFile string) {
	t.Helper()
	if err := os.Truncate(statusGatewayLockPath(""), 0); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"pid":         holder.pid,
		"kind":        gatewayKind,
		"argv":        []string{filepath.Join(binDir, "icebath"), "gateway", "run"},
		"start_time":  GetProcessStartTime(holder.pid),
		"hermes_home": ownerHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, pidFile, string(raw))
}

// Whether the holder is still running, polling its exit without blocking like Popen.poll
func holderAlive(h *lockHolder) bool {
	var ws syscall.WaitStatus
	got, err := syscall.Wait4(h.pid, &ws, syscall.WNOHANG, nil)
	return err == nil && got == 0
}

func TestCrossProfileStopRefusalStopProfileGatewayRefusesOtherProfilesPid(t *testing.T) {
	tmp := t.TempDir()
	rootHome := filepath.Join(tmp, "root-home")
	timHome := filepath.Join(rootHome, "profiles", "tim")
	if err := os.MkdirAll(timHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", timHome)
	binDir := filepath.Join(tmp, "bin")
	holder := spawnLockHolder(t, binDir, timHome, true)
	defer holder.stop()
	pidFile := filepath.Join(timHome, "gateway.pid")
	lookalikeRecord(t, holder, binDir, rootHome, pidFile)
	if ok, err := StopProfileGateway(context.Background()); err != nil || ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("poisoned cross-profile pid file should have been unlinked")
	}
	time.Sleep(500 * time.Millisecond)
	if !holderAlive(holder) {
		t.Fatal("cross-profile SIGTERM fired: profile A's gateway was killed")
	}
}

func TestCrossProfileStopRefusalStopProfileGatewayStillStopsOwnGateway(t *testing.T) {
	tmp := t.TempDir()
	timHome := filepath.Join(tmp, "profiles", "tim")
	if err := os.MkdirAll(timHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", timHome)
	binDir := filepath.Join(tmp, "bin")
	holder := spawnLockHolder(t, binDir, timHome, true)
	defer holder.stop()
	lookalikeRecord(t, holder, binDir, timHome, filepath.Join(timHome, "gateway.pid"))
	if ok, err := StopProfileGateway(context.Background()); err != nil || !ok {
		t.Fatalf("stop %v %v", ok, err)
	}
	exited := make(chan struct{})
	go func() { holder.cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("own gateway was not stopped")
	}
}
