package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// test_connector_local_batches.py test_single_local_unwrap_keeps_session_db_todo_store_and_setup_callback
func TestSingleLocalUnwrapKeepsSessionDbTodoStoreAndSetupCallback(t *testing.T) {
	for _, flattenProbe := range []bool{false, true} {
		t.Run(fmt.Sprint(flattenProbe), func(t *testing.T) { singleLocalUnwrapKeepsLiveState(t, flattenProbe) })
	}
}

func singleLocalUnwrapKeepsLiveState(t *testing.T, flattenProbe bool) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "recall.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "past-session", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "past-session", map[string]any{"role": "user", "content": "live-db-proof"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var callbacks []map[string]any
	connection := func(payload map[string]any) {
		mu.Lock()
		callbacks = append(callbacks, payload)
		mu.Unlock()
		time.AfterFunc(20*time.Millisecond, func() {
			opID, _ := payload["op_id"].(string)
			operation := tools.LiveGet("current-session", opID, "")
			if operation == nil {
				return
			}
			var targets []map[string]any
			for _, target := range payload["targets"].([]any) {
				targets = append(targets, map[string]any{"name": target.(map[string]any)["name"], "status": "skipped"})
			}
			answer, _ := json.Marshal(map[string]any{"targets": targets})
			if err := mcp.ApplyAnswer(ctx, operation, string(answer)); err != nil {
				t.Error(err)
			}
			operation.Settle(tools.SettleAllResolved)
		})
	}
	a := newFixtureAgent(t, AIAgentOptions{Model: "test-model", BaseURL: "http://127.0.0.1:1/v1", Provider: "custom",
		EnabledToolsets: []string{"todo", "session_search", "connections"}, DisabledToolsets: []string{},
		SessionID: "current-session", SessionDB: db, ConnectionCallback: connection})
	tools.SetSessionVars(a.Env(), tools.SessionVars{Source: "desktop", SessionKey: "current-session", SessionID: "current-session"})
	defer tools.ResetSessionVars(a.Env())
	calls := []map[string]any{
		{"name": "session_search", "arguments": map[string]any{"session_id": "past-session"}},
		{"name": "todo_list", "arguments": map[string]any{"todos": []any{map[string]any{"id": "a", "content": "live-store-proof", "status": "pending"}}}},
		{"name": "manage_connections", "arguments": map[string]any{"action": "install", "connectors": []any{map[string]any{"name": "linear", "mcp": true}}}},
	}
	ex := a.toolExecutor()
	var results []map[string]any
	for _, entry := range calls {
		name, args := entry["name"].(string), entry["arguments"].(map[string]any)
		if name != "manage_connections" {
			wrapped, _ := json.Marshal(map[string]any{"calls": []any{entry}})
			parsed := ex.ParseToolCall(ctx, state.ToolCall{ID: "call", Type: "function",
				Function: state.ToolCallFunction{Name: "tool_call", Arguments: string(wrapped)}}, flattenProbe)
			if parsed.ParseError != "" || parsed.ScopeBlock != "" {
				t.Fatalf("unwrap of %s: parse error %q scope block %q", name, parsed.ParseError, parsed.ScopeBlock)
			}
			name, args = parsed.Name, parsed.Args
		}
		if name != entry["name"] {
			t.Fatalf("unwrapped name %q for %v", name, entry["name"])
		}
		raw, err := ex.InvokeTool(ctx, name, args, "task", "call", tools.InvokeToolOptions{PreToolBlockChecked: true})
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatalf("result of %s %q", name, raw)
		}
		results = append(results, result)
	}
	dumped, _ := json.Marshal(results[0])
	if !strings.Contains(string(dumped), "live-db-proof") {
		t.Fatalf("session_search result %s", dumped)
	}
	if items := a.todoStore.Read(); len(items) == 0 || items[0].Content != "live-store-proof" {
		t.Fatalf("todo store %+v", items)
	}
	targets, _ := results[2]["targets"].([]any)
	want := map[string]any{"name": "linear", "kind": "mcp", "action": "install", "state": "skipped"}
	if len(targets) == 0 || !reflect.DeepEqual(targets[0], want) {
		t.Fatalf("manage_connections result %v", results[2])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(callbacks) != 1 || callbacks[0]["tool_call_id"] != "call" {
		t.Fatalf("callbacks %v", callbacks)
	}
	var names []any
	for _, target := range callbacks[0]["targets"].([]any) {
		names = append(names, target.(map[string]any)["name"])
	}
	if !reflect.DeepEqual(names, []any{"linear"}) {
		t.Fatalf("callback targets %v", names)
	}
}
