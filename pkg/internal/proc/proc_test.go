package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestKillReachesTheWholeGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this platform")
	}
	marker := filepath.Join(t.TempDir(), "alive")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "(sleep 2; touch "+marker+") & sleep 2")
	Group(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := Kill(cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("grandchild survived")
	}
	if err := Kill(exec.Command("sh")); err != nil {
		t.Fatal("killing an unstarted command must be harmless")
	}
}

func TestShellCommandRunsALine(t *testing.T) {
	out, err := ShellCommand(context.Background(), "echo hi").Output()
	if err != nil || string(out) != "hi\n" {
		t.Fatalf("%q %v", out, err)
	}
}
