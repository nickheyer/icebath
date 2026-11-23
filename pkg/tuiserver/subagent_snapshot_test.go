package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// One RPC of the owner session through the real dispatcher on the given transport
func subagentSnapshotCall(via tuihost.Transport, method string, params map[string]any) map[string]any {
	merged := map[string]any{"session_id": "ui-owner"}
	for key, value := range params {
		merged[key] = value
	}
	return pyjson.Plain(Dispatch(context.Background(), map[string]any{"id": 1, "method": method, "params": merged}, via)).(map[string]any)
}

// Result of one owner RPC, failing the case on an error reply
func subagentSnapshotResult(t *testing.T, via tuihost.Transport, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := subagentSnapshotCall(via, method, params)
	result, isDict := reply["result"].(map[string]any)
	if !isDict {
		t.Fatalf("%s answered %v", method, reply)
	}
	return result
}

// Subagent ids of the roster one transport reads for the owner session
func subagentSnapshotIDs(t *testing.T, via tuihost.Transport) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	rows, _ := subagentSnapshotResult(t, via, "subagent.list", nil)["subagents"].([]any)
	for _, row := range rows {
		id, _ := row.(map[string]any)["subagent_id"].(string)
		ids[id] = true
	}
	return ids
}

// Subagent ids of one session's roster in roster order
func subagentSnapshotOrder(t *testing.T, sessionID string, via tuihost.Transport) []string {
	t.Helper()
	ids := []string{}
	for _, row := range subagentSnapshotResult(t, via, "subagent.list", map[string]any{"session_id": sessionID})["subagents"].([]any) {
		ids = append(ids, row.(map[string]any)["subagent_id"].(string))
	}
	return ids
}
