package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// test_subagent_steer.py TestSubagentSteerRPC test_run_single_child_binds_exact_runtime_owner_artifacts
func TestSubagentSteerRPCRunSingleChildBindsExactRuntimeOwnerArtifacts(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	ownerTransport := &steerOwnerTransport{name: "owner"}
	ownerRecord := map[string]any{"session_key": "durable-parent", "history": []any{}, "transport": ownerTransport}
	child := livenessChild(env, "sid-context-owner", 0, 0)
	child.subagentID, child.pendingSteerLock = "sid-context-owner", &sync.Mutex{}
	owner := "ui-owner"
	observed := map[string]bool{}
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		observed["owner"] = SteerSubagent(child.subagentID, "owned steer", &owner, ownerTransport, ownerRecord)
		observed["foreign"] = SteerSubagent(child.subagentID, "foreign steer", &owner, &steerOwnerTransport{name: "foreign"}, ownerRecord)
		return map[string]any{"final_response": "done", "completed": true, "interrupted": false, "api_calls": 1, "messages": []any{}}, nil
	})
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: "durable-parent", SessionID: "durable-parent", UISessionID: "ui-owner"})
	t.Cleanup(func() { tools.ClearSessionVars(env) })
	entry := runSingleChild(tools.WithToolEnv(context.Background(), env), 0, "owner binding", child, &AIAgent{env: env}, "", ownerTransport, ownerRecord)
	if !observed["owner"] || observed["foreign"] {
		t.Fatalf("observed %v", observed)
	}
	if missed := entryText(entry, "missed_steer"); missed != "owned steer" || strings.Contains(missed, "foreign") {
		t.Fatalf("steered text %q entry %v", missed, entry)
	}
}
