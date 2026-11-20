//go:build !windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Fake git that forks a long lived descendant recording its pid, then stalls
func writeForkingScript(t *testing.T, dir string) (script, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "child.pid")
	script = filepath.Join(dir, "fakegit.sh")
	body := "#!/usr/bin/env bash\nsleep 300 &\necho $! > " + marker + "\nsleep 300\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

// Alive while a null signal finds the pid, zombies included as os.kill does
func treeKillPidAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// Pid the marker file names once written, failing after timeout
func waitMarkerPid(t *testing.T, marker string, timeout time.Duration) int {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if raw, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(raw)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatal("forking script never wrote its descendant pid")
	return 0
}

func TestTimeoutKillsDescendants(t *testing.T) {
	script, marker := writeForkingScript(t, t.TempDir())
	if out := boundedGitProbe([]string{script}, time.Second, nil); out != "" {
		t.Fatalf("probe output %q, want empty", out)
	}
	childPid := waitMarkerPid(t, marker, 5*time.Second)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && treeKillPidAlive(childPid); {
		time.Sleep(50 * time.Millisecond)
	}
	if treeKillPidAlive(childPid) {
		_ = syscall.Kill(childPid, syscall.SIGKILL)
		t.Fatalf("descendant %d survived probe timeout", childPid)
	}
}

func TestPosixSpawnUsesOwnProcessGroup(t *testing.T) {
	script := filepath.Join(t.TempDir(), "pgid.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\necho \"$$ $(ps -o pgid= -p $$ | tr -d ' ')\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(boundedGitProbe([]string{script}, 5*time.Second, nil))
	if len(fields) != 2 {
		t.Fatalf("probe output fields %q", fields)
	}
	pid, pgid := fields[0], fields[1]
	if pid != pgid {
		t.Fatalf("probe child pid=%s does not lead its group pgid=%s", pid, pgid)
	}
	ours, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if pgid == strconv.Itoa(ours) {
		t.Fatal("probe child must not share our group")
	}
}

func TestGroupKillSkippedWhenChildSharesOurGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := WatchCmd(cmd)
	theirs, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	if theirs != ours {
		t.Fatalf("shared group precondition failed, child %d ours %d", theirs, ours)
	}
	KillChildTree(child, 0)
	if !child.WaitExit(5 * time.Second) {
		t.Fatal("direct child was not killed")
	}
	if cmd.ProcessState == nil {
		t.Fatal("child has no return code")
	}
}

func TestGraceKillTakesATermIgnoringMemberOnceTheLeaderExits(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "member.pid")
	script := filepath.Join(dir, "leader.sh")
	body := "#!/bin/sh\n" + fmt.Sprintf("sh -c 'trap \"\" TERM; echo $$ > %s; exec sleep 300' &\n", marker) + "sleep 300\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := WatchCmd(cmd)
	member := waitMarkerPid(t, marker, 5*time.Second)
	t.Cleanup(func() {
		if treeKillPidAlive(member) {
			_ = exec.Command("kill", "-9", strconv.Itoa(member)).Run()
		}
	})
	KillChildTree(child, 5*time.Second)
	select {
	case <-child.Done():
	default:
		t.Fatal("leader has no return code after the graced kill")
	}
	for deadline := time.Now().Add(3 * time.Second); treeKillPidAlive(member); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("SIGTERM-ignoring member %d to die with the graced kill", member)
		}
	}
}

func TestFastPathUnaffected(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if out := boundedGitProbe([]string{"git", "-C", repo, "rev-parse", "--is-inside-work-tree"}, 10*time.Second, nil); out != "true" {
		t.Fatalf("probe output %q, want true", out)
	}
}

// Handle whose kill always fails as a reaped Popen raising OSError
type deadTreeKillChild struct{}

func (deadTreeKillChild) Pid() int                    { return 1 << 22 }
func (deadTreeKillChild) Kill() error                 { return errors.New("already reaped") }
func (deadTreeKillChild) WaitExit(time.Duration) bool { return true }

func TestKillHelperSwallowAllFailures(t *testing.T) {
	KillChildTree(deadTreeKillChild{}, 0)
}
