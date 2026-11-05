//go:build !windows

package mcp

import (
	"os/exec"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Starts cat reading a pipe, through spawn when given, and returns it with its pipe closer
func startStdinReader(t *testing.T, spawn func(*exec.Cmd) error) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if spawn == nil {
		spawn = (*exec.Cmd).Start
	}
	if err := spawn(cmd); err != nil {
		t.Fatal(err)
	}
	return cmd, func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}
}

func TestChildFilterOwnedSpawnRacingDiscoveryDoesNotHandMCPTheSupervisor(t *testing.T) {
	isolateHome(t)
	restore := tools.IsolateOwnerDeathSupervisor()
	defer restore()
	before := snapshotChildPIDs()
	worker, stopWorker := startStdinReader(t, func(c *exec.Cmd) error { return tools.SpawnOwned(c, "test") })
	server, stopServer := startStdinReader(t, nil)
	func() {
		defer func() {
			stopWorker()
			stopServer()
			tools.ReleaseProcessGroupOnceEmpty(worker.Process.Pid, nil, 5*time.Second)
			if tools.OwnerDeathSupervisorPID() != 0 {
				t.Error("releasing the last group stops the supervisor")
			}
		}()
		supervisor := tools.OwnerDeathSupervisorPID()
		if supervisor == 0 {
			t.Fatal("the owned spawn never started the supervisor")
		}
		delta := map[int]bool{}
		for pid := range snapshotChildPIDs() {
			if !before[pid] {
				delta[pid] = true
			}
		}
		if !delta[supervisor] {
			t.Fatalf("precondition: the supervisor is in MCP's spawn delta %v", delta)
		}
		kept := filterMCPChildren(delta)
		if kept[supervisor] {
			t.Fatal("the supervisor was adopted as an MCP child")
		}
		if !kept[server.Process.Pid] {
			t.Fatalf("the MCP server %d was dropped from %v", server.Process.Pid, kept)
		}
	}()
}

func TestChildFilterUnpublishedSupervisorIsStillNotAnMCPChild(t *testing.T) {
	restore := tools.IsolateOwnerDeathSupervisor()
	defer restore()
	supervisor, err := tools.SpawnOwnerDeathSupervisor()
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.CloseAndWait()
	if kept := filterMCPChildren(map[int]bool{supervisor.PID(): true}); len(kept) != 0 {
		t.Fatalf("unpublished supervisor kept %v", kept)
	}
}
