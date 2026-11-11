//go:build !windows

package gateway

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const ignoringChildScript = `trap "" TERM; printf ready > "$1"; exec sleep 300`

// Backend stand in with a hosted terminal child behind a real PTY, a detached bot and a wedged SIGTERM teardown
const wedgedBackendScript = `tui_pid="$1"; bot_pid="$2"; ready="$3"
TUI_PID_FILE="$tui_pid" script -qfc 'trap "" TERM HUP; echo $$ > "$TUI_PID_FILE"; exec sleep 300' /dev/null >/dev/null 2>&1 &
BOT_PID_FILE="$bot_pid" setsid sh -c 'echo $$ > "$BOT_PID_FILE"; exec sleep 300' </dev/null >/dev/null 2>&1 &
while [ ! -s "$tui_pid" ] || [ ! -s "$bot_pid" ]; do sleep 0.01; done
trap "" TERM
printf ready > "$ready"
exec sleep 300`

// Installs a holder grammar that reads the token after the first icebath word of a command line
func pinIcebathWordGrammar(t *testing.T) {
	t.Helper()
	swapSeam(t, &HolderSubcommand, func(cmdline string) (string, bool) {
		fields := strings.Fields(cmdline)
		for i, f := range fields {
			if filepath.Base(f) == "icebath" && i+1 < len(fields) {
				return fields[i+1], true
			}
		}
		return "", false
	})
}

// Starts a shell child and waits for the ready file it writes once its signal dispositions are set
func spawnReady(t *testing.T, script, readyPath string, args ...string) *exec.Cmd {
	t.Helper()
	child := exec.Command("sh", append([]string{"-c", script, "sh"}, args...)...)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("child exited before signaling ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("child never signaled ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	childExits[child] = exited
	return child
}

var childExits = map[*exec.Cmd]chan error{}

// Runs the POSIX kill sequence on the child and returns its results with the signal that ended it
func killAndReap(t *testing.T, child *exec.Cmd) ([]int, []DashboardKillFailure, syscall.Signal) {
	t.Helper()
	killed, failed := []int{}, []DashboardKillFailure{}
	killPidsPosix(context.Background(), []int{child.Process.Pid}, &killed, &failed)
	var err error
	select {
	case err = <-childExits[child]:
	case <-time.After(10 * time.Second):
		_ = child.Process.Kill()
		err = <-childExits[child]
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child ended with %v", err)
	}
	return killed, failed, exitErr.Sys().(syscall.WaitStatus).Signal()
}

// True while pid exists and is not a zombie
func pidRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}

func TestSigtermIgnoringProcessIsStillSigkilled(t *testing.T) {
	swapSeam(t, &posixTermGraceSeconds, 0.6)
	ready := filepath.Join(t.TempDir(), "ready")
	child := spawnReady(t, ignoringChildScript, ready, ready)
	killed, failed, sig := killAndReap(t, child)
	if len(failed) != 0 {
		t.Fatalf("failed = %v", failed)
	}
	if sig != syscall.SIGKILL {
		t.Fatalf("child ended by %v", sig)
	}
	if !slices.Equal(killed, []int{child.Process.Pid}) {
		t.Fatalf("killed = %v", killed)
	}
}

func TestWedgedPtyDescendantIsGoneButDetachedBotSurvives(t *testing.T) {
	for _, tool := range []string{"script", "setsid"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	swapSeam(t, &posixTermGraceSeconds, 0.6)
	dir := t.TempDir()
	tuiPidFile, botPidFile, ready := filepath.Join(dir, "tui.pid"), filepath.Join(dir, "bot.pid"), filepath.Join(dir, "ready")
	backend := spawnReady(t, wedgedBackendScript, ready, tuiPidFile, botPidFile, ready)
	readPid := func(path string) int {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}
	tuiPid, botPid := readPid(tuiPidFile), readPid(botPidFile)
	defer func() {
		for _, pid := range []int{tuiPid, botPid} {
			if pidRunning(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	killed, failed, sig := killAndReap(t, backend)
	if !slices.Equal(killed, []int{backend.Process.Pid}) || len(failed) != 0 {
		t.Fatalf("killed = %v failed = %v", killed, failed)
	}
	if sig != syscall.SIGKILL {
		t.Fatalf("backend ended by %v", sig)
	}
	if pidRunning(tuiPid) {
		t.Fatal("wedged hosted TUI outlived the dashboard stop")
	}
	if !pidRunning(botPid) {
		t.Fatal("detached gateway bot was killed with the dashboard")
	}
}

func TestScanSparesOnlyTheCallersWrapperShell(t *testing.T) {
	pinIcebathWordGrammar(t)
	swapSeam(t, &dashboardGOOS, "linux")
	swapSeam(t, &dashboardRunProbe, func(context.Context, []string, time.Duration) (*pysubprocess.Completed, error) {
		return &pysubprocess.Completed{Stdout: []byte("111 /opt/icebath/bin/python icebath dashboard --port 9119\n" +
			"222 bash -c icebath dashboard --stop\n333 bash -c icebath serve\n")}, nil
	})
	swapSeam(t, &dashboardLedgerEntries, func() []map[string]any { return nil })
	swapSeam(t, &dashboardScanGetpid, func() int { return 999 })
	swapSeam(t, &dashboardProcPPID, func(pid int) (int, bool) {
		if pid == 999 {
			return 222, true
		}
		return 0, false
	})
	swapSeam(t, &dashboardProcCmdline, func(pid int) ([]string, error) {
		if pid == 222 || pid == 333 {
			return []string{"/usr/bin/bash", "-c"}, nil
		}
		return []string{"/usr/bin/python3.12"}, nil
	})
	if got := findStaleDashboardPIDs(context.Background(), nil, ""); !slices.Equal(got, []int{111, 333}) {
		t.Fatalf("pids = %v", got)
	}
}

func TestPythonHeadedOrUnreadableAncestorStaysKilled(t *testing.T) {
	ancestors := map[int]bool{4242: true}
	swapSeam(t, &dashboardProcCmdline, func(int) ([]string, error) { return []string{"python3.12"}, nil })
	if isCallerWrapperShell(4242, ancestors) {
		t.Fatal("a python headed ancestor was spared")
	}
	swapSeam(t, &dashboardProcCmdline, func(int) ([]string, error) { return nil, os.ErrNotExist })
	if isCallerWrapperShell(4242, ancestors) {
		t.Fatal("an unreadable ancestor was spared")
	}
	swapSeam(t, &dashboardProcCmdline, func(int) ([]string, error) { return []string{"bash"}, nil })
	if !isCallerWrapperShell(4242, ancestors) {
		t.Fatal("the wrapper shell of the caller was not spared")
	}
}
