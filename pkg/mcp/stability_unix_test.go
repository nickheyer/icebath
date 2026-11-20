//go:build !windows

package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Live process that is neither a zombie nor gone
func processRunning(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return unix.Kill(pid, 0) == nil
	}
	rest := string(raw[strings.LastIndexByte(string(raw), ')')+1:])
	state := strings.Fields(rest)[0]
	return state != "Z" && state != "X"
}

func TestStabilityGrandchildReapedViaProcessGroup(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	grand := `trap "" TERM; echo $$ > "$1.tmp"; mv "$1.tmp" "$1"; while :; do sleep 0.2; done`
	parent := exec.Command("sh", "-c", `sh -c '`+grand+`' _ "$0" & read x`, pidFile)
	setProcessGroup(parent)
	stdin, err := parent.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	pgid, _ := unix.Getpgid(parent.Process.Pid)
	start, hasStart := leaderStartTime(parent.Process.Pid)
	var gpid int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			gpid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stdin.Close()
	_ = parent.Wait()
	if gpid == 0 {
		t.Fatal("grandchild did not start")
	}
	defer unix.Kill(-pgid, unix.SIGKILL)
	if g, _ := unix.Getpgid(gpid); !processRunning(gpid) || g != pgid {
		t.Fatalf("grandchild %d not running in group %d", gpid, pgid)
	}
	mcpMu.Lock()
	orphanStdioPIDs[parent.Process.Pid] = true
	orphanStdioPIDServers[parent.Process.Pid] = "orphan"
	stdioPGIDs[parent.Process.Pid] = pgid
	if hasStart {
		stdioStartTimes[parent.Process.Pid] = start
	}
	mcpMu.Unlock()
	killOrphanedMCPChildren(false, "")
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && processRunning(gpid) {
		time.Sleep(50 * time.Millisecond)
	}
	if processRunning(gpid) {
		t.Fatal("SIGTERM-ignoring grandchild survived the reaper after its leader exited")
	}
}
