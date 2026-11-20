package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Synthetic Bot Desktop launcher in its own session with the launcher.pid identity and env the runtime status reads
func liveBotDesktopLauncher(t *testing.T, profileDir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	born, err := proc.Birth(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	sd := filepath.Join(profileDir, "bot-desktop")
	writeFile(t, filepath.Join(sd, "launcher.pid"), fmt.Sprintf("%d %s", cmd.Process.Pid, strconv.FormatFloat(born, 'f', -1, 64)))
	writeFile(t, filepath.Join(sd, "env"), "DISPLAY=:42\n")
	return cmd
}

func TestProfileDeleteAndRenameStopTheProfilesBotDesktop(t *testing.T) {
	for _, op := range []string{"delete", "rename"} {
		t.Run(op, func(t *testing.T) {
			manageEnv(t)
			captureOut(t)
			profileDir, err := CreateProfile(context.Background(), "coder", CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			launcher := liveBotDesktopLauncher(t, profileDir)
			lease, _ := json.Marshal(map[string]any{"holder": "human", "viewer_id": "gone", "since": 1.0, "epoch": 3, "reason": ""})
			writeFile(t, filepath.Join(profileDir, "bot-desktop", "lease.json"), string(lease))
			oldClean := cleanupGatewayService
			t.Cleanup(func() { cleanupGatewayService = oldClean })
			cleanupGatewayService = func(context.Context, string, string) bool { return false }
			skipAliasCollision(t)
			if op == "delete" {
				_, err = DeleteProfile(context.Background(), "coder", true)
			} else {
				_, err = RenameProfile(context.Background(), "coder", "hacker")
			}
			if err != nil {
				t.Fatal(err)
			}
			pid := launcher.Process.Pid
			alive := func() bool { return proc.Exists(pid) && !proc.Zombie(pid) }
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && alive(); {
				time.Sleep(50 * time.Millisecond)
			}
			if alive() {
				t.Fatal("the launcher was not stopped by the profile op")
			}
			if op == "rename" {
				raw, err := os.ReadFile(filepath.Join(filepath.Dir(profileDir), "hacker", "bot-desktop", "lease.json"))
				if err != nil {
					t.Fatal(err)
				}
				var moved map[string]any
				if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &moved); err != nil || moved["holder"] != "agent" {
					t.Fatalf("stale human lease survived the teardown %s %v", raw, err)
				}
			}
		})
	}
}
