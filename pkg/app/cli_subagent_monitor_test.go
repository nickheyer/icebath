package app

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears the live subagent registry around a case
func cleanSubagentRegistry(t *testing.T) {
	t.Helper()
	agent.ClearActiveSubagents()
	t.Cleanup(agent.ClearActiveSubagents)
}

// Agent owning children under the session id
func ownerAgent(t *testing.T, sessionID string) *GatewayAgent {
	return offlineAgent(t, &CLI{ctx: context.Background(), sessionID: sessionID})
}

// Child double whose steer text is recorded
type steerableChild struct{ delivered *[]string }

func (s steerableChild) Steer(text string) bool {
	*s.delivered = append(*s.delivered, text)
	return true
}

// CLI holding only the agent, session and pending queue the dock reads
func dockCLI(t *testing.T, a *GatewayAgent, sessionID string) *CLI {
	t.Helper()
	recordPrints(t)
	c := bareCLI()
	c.sessionID, c.pendingInput = sessionID, &inputQueue{}
	if a != nil {
		c.setAgent(a)
	}
	return c
}

// Polls until the condition holds or five seconds pass
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Starts a background shell process through the default registry
func spawnDockProcess(t *testing.T, command string) *tools.ProcessSession {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", t.TempDir())
	proc, err := tools.DefaultProcessRegistry().SpawnLocal(context.Background(), nil, command,
		tools.SpawnLocalOptions{Cwd: ".", TaskID: "t", OwnerTaskID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		registry := tools.DefaultProcessRegistry()
		registry.KillProcess(context.Background(), nil, proc.ID, "process.kill", true)
		deadline := time.Now().Add(5 * time.Second)
		for !processExited(proc.ID) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		registry.ListSessions(context.Background(), nil, "", "", false)
		registry.ForgetFinished(proc.ID)
	})
	return proc
}

// Whether the registry saw the process exit
func processExited(id string) bool {
	proc := tools.DefaultProcessRegistry().Get(nil, id)
	return proc != nil && proc.DockFields().ExitedAt != 0
}
