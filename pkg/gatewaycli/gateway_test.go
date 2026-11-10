package gatewaycli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

const lockHolderEnv = "ICEBATH_GATEWAYCLI_TEST_LOCK_HOLDER"

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	if os.Getenv(lockHolderEnv) == "1" {
		if !gateway.AcquireGatewayRuntimeLock(context.Background()) {
			fmt.Fprintln(os.Stderr, "could not acquire gateway.lock")
			os.Exit(1)
		}
		if err := gateway.WritePidFile(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(os.Getpid())
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Live child launched as icebath gateway run holding the home gateway lock, stopped at cleanup
func spawnGatewayHolder(t *testing.T, root, home string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "bin", "icebath")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, script); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, "gateway", "run")
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+home, lockHolderEnv+"=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		got <- strings.TrimSpace(line)
	}()
	var line string
	select {
	case line = <-got:
	case <-time.After(20 * time.Second):
	}
	pid, err := strconv.Atoi(line)
	if err != nil {
		t.Fatalf("lock holder failed to start: %q %s", line, stderr.String())
	}
	return pid
}

// Pid of a child that already exited and left the process table
func exitedPid(t *testing.T) int {
	t.Helper()
	exe, _ := os.Executable()
	child := exec.Command(exe, "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	pid := child.ProcessState.Pid()
	for deadline := time.Now().Add(10 * time.Second); processListed(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("exited child %d never left the process table", pid)
		}
	}
	return pid
}

// Rewrites the pid file as a replaced gateway left it, its own pid and an earlier start
func writePreviousGatewayPidFile(t *testing.T, home string, pid int) {
	t.Helper()
	path := filepath.Join(home, "gateway.pid")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(string(data))
	if err != nil {
		t.Fatal(err)
	}
	record := v.(*pyjson.Object)
	raw, _ := record.Get("start_time")
	start, isInt := raw.(int64)
	if !isInt {
		t.Fatalf("start_time %T", raw)
	}
	record.Set("pid", int64(pid))
	record.Set("start_time", max(int64(1), start-500))
	text, _ := pyjson.Dumps(record)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Home with a live gateway holder and no named profiles beside it
func discoveryHome(t *testing.T) (string, int) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid := spawnGatewayHolder(t, root, home)
	t.Setenv("ICEBATH_HOME", home)
	return home, pid
}

func assertDiscovered(t *testing.T, home string, pid int, strict bool) {
	t.Helper()
	procs, err := FindProfileGatewayProcesses(context.Background(), nil, strict)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(home)
	if len(procs) != 1 || procs[0].PID != pid || procs[0].Profile != "default" {
		t.Fatalf("discovered %+v, want default %d", procs, pid)
	}
	if got, _ := filepath.EvalSymlinks(procs[0].Path); got != want {
		t.Fatalf("path %q, want %q", got, want)
	}
	if strict != (procs[0].CreateTime > 0) {
		t.Fatalf("create time %v under strict %v", procs[0].CreateTime, strict)
	}
}

func TestUpdaterDiscoveryMapsTheProfileWithoutAPidFile(t *testing.T) {
	home, pid := discoveryHome(t)
	if err := os.Remove(filepath.Join(home, "gateway.pid")); err != nil {
		t.Fatal(err)
	}
	assertDiscovered(t, home, pid, true)
}

func TestUpdaterDiscoveryMapsTheProfileWithAStalePidFile(t *testing.T) {
	home, pid := discoveryHome(t)
	writePreviousGatewayPidFile(t, home, exitedPid(t))
	assertDiscovered(t, home, pid, true)
}

func TestDiscoveryExclusionsAndInspectionFailures(t *testing.T) {
	home, pid := discoveryHome(t)
	assertDiscovered(t, home, pid, false)
	procs, err := FindProfileGatewayProcesses(context.Background(), map[int]bool{pid: true}, true)
	if err != nil || len(procs) != 0 {
		t.Fatalf("excluded pid discovered %+v %v", procs, err)
	}
	if err := os.WriteFile(filepath.Join(home, "gateway.pid"), []byte("not a record"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = FindProfileGatewayProcesses(context.Background(), nil, true)
	var inspect *InspectError
	if !errors.As(err, &inspect) || inspect.Profile != "default" || !strings.Contains(inspect.Err.Error(), "malformed") ||
		err.Error() != "Could not inspect gateway PID for profile default" {
		t.Fatalf("strict malformed pid file %v", err)
	}
	prev := listProfiles
	t.Cleanup(func() { listProfiles = prev })
	listProfiles = func(context.Context) ([]manage.ProfileInfo, error) { return nil, errors.New("listing failed") }
	if procs, err := FindProfileGatewayProcesses(context.Background(), nil, false); err != nil || len(procs) != 0 {
		t.Fatalf("lenient listing failure %+v %v", procs, err)
	}
	if _, err := FindProfileGatewayProcesses(context.Background(), nil, true); err == nil || err.Error() != "listing failed" {
		t.Fatalf("strict listing failure %v", err)
	}
}
