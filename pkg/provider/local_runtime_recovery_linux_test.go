//go:build linux

package provider

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Shell that backgrounds a cat reading fd 3, prints its pid, and exits when stdin closes
func lr4ShellOwner(read *os.File) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", "cat <&3 >/dev/null & echo $!; read line")
	cmd.ExtraFiles = []*os.File{read}
	return cmd
}

// Blocks until the forked child has exec'd away from the owner shell image so its recorded executable is final
func lr4WaitExec(t *testing.T, ownerPid, childPid int) {
	t.Helper()
	shell, err := proc.Exe(ownerPid)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if exe, err := proc.Exe(childPid); err == nil && exe != shell {
			return
		}
	}
	t.Fatalf("child %d never exec'd away from %s", childPid, shell)
}

func TestLocalRuntimeReparentedRouterKeepsItsEndpoint(t *testing.T) {
	path := lr4StateHome(t)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	owner := lr4ShellOwner(read)
	stdout, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := owner.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	_ = read.Close()
	var childPid int
	if err := json.NewDecoder(stdout).Decode(&childPid); err != nil {
		t.Fatal(err)
	}
	lr4WaitExec(t, owner.Process.Pid, childPid)
	state := lr4ModernRecord(t, childPid)
	ownerCreated, err := tools.ProcessBirth(owner.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	state["owner_pid"], state["owner_create_time"] = owner.Process.Pid, ownerCreated
	delete(state, "start_time")
	delete(state, "owner_start_time")
	child, err := openPSProcess(childPid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = write.Close()
		_ = child.Wait(10 * time.Second)
	}()
	_ = stdin.Close()
	_ = owner.Wait()
	route := map[string]any{"base_url": "http://127.0.0.1:59999/v1", "api_key": "test-only"}
	state["base_url"], state["api_key"] = route["base_url"], route["api_key"]
	lr4WriteState(t, path, state)
	if ppid, err := child.PPID(); err != nil || ppid == owner.Process.Pid {
		t.Fatalf("child ppid = %d, %v", ppid, err)
	}
	if recordedProcess(readLocalRuntimeState()) == nil {
		t.Fatal("the reparented router lost its identity")
	}
	if got := lr4StateEndpoint(); got["base_url"] != route["base_url"] || got["api_key"] != route["api_key"] {
		t.Fatalf("state endpoint = %v", got)
	}
}

func TestLocalRuntimeClockStepKeepsTheRecordedRouter(t *testing.T) {
	path := lr4StateHome(t)
	_, pid := lr4Sleeper(t)
	state := lr4ModernRecord(t, pid)
	route := map[string]any{"base_url": "http://127.0.0.1:59998/v1", "api_key": "sk-local"}
	state["base_url"], state["api_key"] = route["base_url"], route["api_key"]
	lr4WriteState(t, path, state)
	prev := lrBirth
	lrBirth = func(pid int) (float64, error) {
		created, err := prev(pid)
		return created + 22, err
	}
	t.Cleanup(func() { lrBirth = prev })
	if recordedProcess(readLocalRuntimeState()) == nil {
		t.Fatal("a stepped clock lost the live router")
	}
	if got := lr4StateEndpoint(); got["base_url"] != route["base_url"] || got["api_key"] != route["api_key"] {
		t.Fatalf("state endpoint = %v", got)
	}
	state["start_time"] = state["start_time"].(int64) - 1000
	lr4WriteState(t, path, state)
	if recordedProcess(readLocalRuntimeState()) != nil {
		t.Fatal("a reused pid kept the endpoint")
	}
	if got := lr4StateEndpoint(); got != nil {
		t.Fatalf("state endpoint = %v, want none", got)
	}
}
