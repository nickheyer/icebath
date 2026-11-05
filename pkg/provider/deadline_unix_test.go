//go:build !windows

package provider

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

const deadlineHelperEnv = "ICEBATH_DEADLINE_HELPER"

// Runs the helper modes the kill tree tests spawn, idle when the helper variable is unset
func TestDeadlineHelperProcess(t *testing.T) {
	mode := os.Getenv(deadlineHelperEnv)
	if mode == "" {
		return
	}
	args := strings.Split(mode, "|")
	switch args[0] {
	case "sleep":
		secs, _ := strconv.Atoi(args[1])
		time.Sleep(time.Duration(secs) * time.Second)
	case "record":
		_ = os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(10 * time.Second)
	case "spawn":
		child := deadlineHelperCmd("record|"+args[1], args[2] == "1")
		_ = child.Start()
		time.Sleep(10 * time.Second)
	case "forker":
		marker, release := args[1], args[2]
		spawn := func() int {
			c := deadlineHelperCmd("sleep|60", true)
			_ = c.Start()
			return c.Process.Pid
		}
		first := spawn()
		writeJSONFile(marker, []int{os.Getpid(), first})
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		late := spawn()
		writeJSONFile(marker, []int{os.Getpid(), first, late})
		time.Sleep(60 * time.Second)
	}
	os.Exit(0)
}

func writeJSONFile(path string, v any) {
	raw, _ := json.Marshal(v)
	_ = os.WriteFile(path, raw, 0o644)
}

// Builds a command re-running this test binary in one helper mode, optionally in a new session
func deadlineHelperCmd(mode string, newSession bool) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeadlineHelperProcess$")
	cmd.Env = append(os.Environ(), deadlineHelperEnv+"="+mode)
	if newSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	return cmd
}

// Reports whether pid is gone or a zombie within the timeout
func waitPidDead(pid int, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		state, err := proc.State(pid)
		if err != nil || state == 'Z' || state == 'X' {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitForFileText(t *testing.T, path string) string {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return string(raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("grandchild never spawned, test harness broken")
	return ""
}

func killTreeOfSpawner(t *testing.T, grandchildNewSession bool) {
	started := filepath.Join(t.TempDir(), "grandchild_started")
	flag := "0"
	if grandchildNewSession {
		flag = "1"
	}
	child := deadlineHelperCmd("spawn|"+started+"|"+flag, true)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	grandchild, _ := strconv.Atoi(waitForFileText(t, started))
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	if !KillProcessTree(child.Process.Pid, 0) {
		t.Fatal("kill_process_tree reported nothing signalled")
	}
	_ = child.Wait()
	if !waitPidDead(grandchild, 5*time.Second) {
		t.Fatal("grandchild survived")
	}
}

func TestKillProcessTreeKillsDescendantsOfSessionLeader(t *testing.T) {
	killTreeOfSpawner(t, false)
}

func TestKillProcessTreeKillsDescendantInItsOwnSession(t *testing.T) {
	killTreeOfSpawner(t, true)
}

func TestKillProcessTreeAlreadyDeadPidReturnsFalse(t *testing.T) {
	child := deadlineHelperCmd("exit", true)
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	if KillProcessTree(child.Process.Pid, 0) {
		t.Fatal("reaped pid reported signalled")
	}
}

func TestKillProcessTreeNonGroupLeaderFallsBackToSingleKill(t *testing.T) {
	child := deadlineHelperCmd("sleep|30", false)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if pgid, _ := syscall.Getpgid(child.Process.Pid); pgid == child.Process.Pid {
		t.Fatal("helper unexpectedly leads its group")
	}
	if !KillProcessTree(child.Process.Pid, syscall.SIGTERM) {
		t.Fatal("single kill not signalled")
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process survived SIGTERM")
	}
}

func TestKillProcessTreeClosesDescendantSnapshotForkWindow(t *testing.T) {
	dir := t.TempDir()
	marker, release := filepath.Join(dir, "pids.json"), filepath.Join(dir, "release")
	readIDs := func() []int {
		var ids []int
		if raw, err := os.ReadFile(marker); err == nil {
			_ = json.Unmarshal(raw, &ids)
		}
		return ids
	}
	child := deadlineHelperCmd("forker|"+marker+"|"+release, true)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, pid := range readIDs() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = child.Wait()
	})
	for end := time.Now().Add(10 * time.Second); len(readIDs()) < 2 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if len(readIDs()) < 2 {
		t.Fatal("pre-existing detached child did not start")
	}
	var schedules []int
	prev := procPPIDMap
	procPPIDMap = func() []procParent {
		mapping := readPPIDMap()
		_ = os.WriteFile(release, nil, 0o644)
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); {
			ids := readIDs()
			if len(ids) == 3 {
				break
			}
			if state, err := proc.State(ids[0]); err == nil && state == 'T' {
				break
			}
			time.Sleep(time.Millisecond)
		}
		schedules = append(schedules, len(mapping))
		return mapping
	}
	signalled := KillProcessTree(child.Process.Pid, 0)
	procPPIDMap = prev
	ids := readIDs()
	exited := func() bool {
		for _, pid := range ids {
			if state, err := proc.State(pid); err == nil && state != 'Z' && state != 'X' {
				return false
			}
		}
		return true
	}
	for end := time.Now().Add(10 * time.Second); !exited() && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if !signalled || len(schedules) == 0 || !exited() {
		states := map[int]string{}
		for _, pid := range ids {
			if state, err := proc.State(pid); err == nil {
				states[pid] = string(state)
			} else {
				states[pid] = "gone"
			}
		}
		t.Fatalf("signalled %v real snapshot sizes %v states %v", signalled, schedules, states)
	}
}

func TestRefusedHardKillPreservesTheTargetsOriginalRunState(t *testing.T) {
	for _, initiallyStopped := range []bool{false, true} {
		child := deadlineHelperCmd("sleep|30", true)
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		pid := child.Process.Pid
		waitForState := func(stopped bool) {
			end := time.Now().Add(5 * time.Second)
			for {
				state, _ := proc.State(pid)
				if (state == 'T') == stopped {
					return
				}
				if time.Now().After(end) {
					t.Fatalf("initially stopped %v state %q", initiallyStopped, state)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		func() {
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			if initiallyStopped {
				_ = syscall.Kill(pid, syscall.SIGSTOP)
				waitForState(true)
			}
			prev := deadlineKill
			deadlineKill = func(target int, sig syscall.Signal) error {
				if target != pid && target != -pid {
					t.Errorf("signal aimed at %d, owned probe is %d", target, pid)
				}
				if sig == syscall.SIGKILL {
					return syscall.EPERM
				}
				return syscall.Kill(target, sig)
			}
			signalled := KillProcessTree(pid, 0)
			deadlineKill = prev
			if signalled {
				t.Fatalf("initially stopped %v refused hard kill reported signalled", initiallyStopped)
			}
			waitForState(initiallyStopped)
		}()
	}
}
