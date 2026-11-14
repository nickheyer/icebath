//go:build linux

package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Gone, zombie or dead per the proc stat state field
func processExited(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	text := string(raw)
	i := strings.LastIndexByte(text, ')')
	if i < 0 || i+2 >= len(text) {
		return true
	}
	state := text[i+2]
	return state == 'Z' || state == 'X'
}

func scriptProcessLive(pid int) bool { return !processExited(pid) }

func TestTimeoutLeavesNoDetachedDescendant(t *testing.T) {
	s := scriptStore(t)
	t.Setenv("ICEBATH_CRON_SCRIPT_TIMEOUT", "2")
	root := filepath.Dir(s.Home())
	marker, release := filepath.Join(root, "pids.json"), filepath.Join(root, "release")
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := writeScript(t, s, "spawner.py", "import subprocess,sys,time,os,json\nfrom pathlib import Path\n"+
		"def spawn():\n"+
		" return subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)'],"+
		"start_new_session=True,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)\n"+
		"first=spawn()\n"+
		fmt.Sprintf("Path(%q).write_text(json.dumps([os.getpid(),first.pid]))\n", marker)+
		fmt.Sprintf("while not Path(%q).exists():time.sleep(0.001)\n", release)+
		"late=spawn()\n"+
		fmt.Sprintf("Path(%q).write_text(json.dumps([os.getpid(),first.pid,late.pid]))\n", marker)+
		"time.sleep(60)\n")
	ok, out, err := s.runJobScript(context.Background(), script, root, nil, nil)
	if err != nil || ok || !strings.HasPrefix(out, "Script timed out after 2s:") {
		t.Fatalf("expected the timeout path, got %v %q %v", ok, out, err)
	}
	var ids []int
	raw, _ := os.ReadFile(marker)
	json.Unmarshal(raw, &ids)
	t.Cleanup(func() {
		for _, pid := range ids {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if len(ids) < 2 {
		t.Fatalf("detached children did not start %v", ids)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive := 0
		for _, pid := range ids {
			if !processExited(pid) {
				alive++
			}
		}
		if alive == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendants survived the timeout %v", ids)
}
