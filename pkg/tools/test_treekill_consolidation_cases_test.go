//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Runs the parent or the setsid grandchild of the end to end tree kill case when the test binary is re-executed
func init() {
	switch os.Getenv("ICEBATH_TREEKILL_E2E") {
	case "parent":
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "ICEBATH_TREEKILL_E2E=grandchild")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(3)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "grandchild":
		_ = os.WriteFile(os.Getenv("ICEBATH_TREEKILL_STARTED"), []byte("x"), 0o644)
		time.Sleep(10 * time.Second)
		_ = os.WriteFile(os.Getenv("ICEBATH_TREEKILL_MARKER"), []byte("x"), 0o644)
		os.Exit(0)
	}
}

func TestE2eSetsidGrandchildKilledViaCompatWrapper(t *testing.T) {
	dir := t.TempDir()
	started, marker := filepath.Join(dir, "grandchild_started"), filepath.Join(dir, "grandchild_survived")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ICEBATH_TREEKILL_E2E=parent", "ICEBATH_TREEKILL_STARTED="+started, "ICEBATH_TREEKILL_MARKER="+marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := WatchCmd(cmd)
	t.Cleanup(func() {
		if child.Kill() == nil {
			child.WaitExit(5 * time.Second)
		}
	})
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatal("grandchild never spawned, test harness broken")
	}
	table, err := proc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	descendants := table.Descendants(cmd.Process.Pid)
	if len(descendants) == 0 {
		t.Fatal("no descendants snapshotted before the tree kill")
	}
	KillChildTree(child, 0)
	if !child.WaitExit(5 * time.Second) {
		t.Fatal("parent did not exit after the tree kill")
	}
	var alive []int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		alive = alive[:0]
		for _, pid := range descendants {
			if proc.Exists(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(alive) > 0 {
		for _, pid := range alive {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Fatalf("survivors after tree kill: %v", alive)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild wrote its survival marker")
	}
}
