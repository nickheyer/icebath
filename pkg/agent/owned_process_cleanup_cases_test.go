package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent over the shared home env the way every hermes agent of one process shares its home
func ownedProcessAgent(t *testing.T, env *tools.Env) *AIAgent {
	t.Helper()
	a, err := NewAIAgent(tools.WithToolEnv(context.Background(), env), AIAgentOptions{APIKey: "test", BaseURL: "http://127.0.0.1:9/v1", Provider: "openai-compat",
		Model: "test", EnabledToolsets: []string{}, QuietMode: true, SkipContextFiles: true, SkipMemory: true, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// Binds the turn identity to taskID and starts a sixty second background process through the terminal tool
func ownedProcessSpawn(t *testing.T, a *AIAgent, taskID, workdir string) string {
	t.Helper()
	bindTurnIdentity(a, taskID, nil, nil, nil, nil)
	raw := tools.TerminalTool(tools.WithToolEnv(context.Background(), a.env), a.env, tools.TerminalRequest{
		Command: "sleep 60", Background: true, TaskID: taskID, Workdir: workdir, NotifyOnComplete: true})
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("terminal %q: %v", raw, err)
	}
	id, _ := result["session_id"].(string)
	if id == "" {
		t.Fatalf("terminal %q", raw)
	}
	return id
}

func ownedProcessStatus(a *AIAgent, id string) any {
	return tools.DefaultProcessRegistry().Poll(context.Background(), a.env, id)["status"]
}

func TestOwnedProcessCleanup(t *testing.T) {
	caseHome(t)
	registry := tools.DefaultProcessRegistry()
	t.Run("test_child_close_kills_only_its_processes", func(t *testing.T) {
		env := promptTestEnv(t)
		parent, child, sibling, unstarted := ownedProcessAgent(t, env), ownedProcessAgent(t, env), ownedProcessAgent(t, env), ownedProcessAgent(t, env)
		tmp := t.TempDir()
		t.Cleanup(func() { registry.KillAll(context.Background(), parent.env, "", nil, "", false) })
		parentID := ownedProcessSpawn(t, parent, "parent-owner", tmp)
		childID := ownedProcessSpawn(t, child, "sa-child-owner", tmp)
		siblingID := ownedProcessSpawn(t, sibling, "sa-sibling-owner", tmp)
		taskIDs := map[string]bool{}
		for _, id := range []string{parentID, childID, siblingID} {
			taskIDs[registry.Get(parent.env, id).TaskID] = true
		}
		if len(taskIDs) != 1 {
			t.Fatalf("task ids %v", taskIDs)
		}
		unstarted.Close()
		for _, id := range []string{parentID, childID, siblingID} {
			if ownedProcessStatus(parent, id) != "running" {
				t.Fatalf("%s stopped by an unstarted agent close", id)
			}
		}
		child.Close()
		if ownedProcessStatus(child, childID) == "running" || ownedProcessStatus(parent, parentID) != "running" || ownedProcessStatus(sibling, siblingID) != "running" {
			t.Fatalf("after child close child %v parent %v sibling %v", ownedProcessStatus(child, childID), ownedProcessStatus(parent, parentID), ownedProcessStatus(sibling, siblingID))
		}
		if !registry.IsCompletionConsumed(childID) {
			t.Fatal("child completion not consumed")
		}
		child.Close()
		if ownedProcessStatus(parent, parentID) != "running" {
			t.Fatal("second child close stopped the parent")
		}
	})
	t.Run("test_close_reclaims_processes_from_previous_turns", func(t *testing.T) {
		a := ownedProcessAgent(t, promptTestEnv(t))
		tmp := t.TempDir()
		t.Cleanup(func() { registry.KillAll(context.Background(), a.env, "", nil, "", false) })
		first := ownedProcessSpawn(t, a, "turn-one-owner", tmp)
		second := ownedProcessSpawn(t, a, "turn-two-owner", tmp)
		a.Close()
		for _, id := range []string{first, second} {
			if ownedProcessStatus(a, id) == "running" {
				t.Fatalf("%s still running after close", id)
			}
		}
	})
}
