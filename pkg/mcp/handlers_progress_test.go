package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestProgressNotificationsReachTheToolProgressCallbackThroughTheRegistry(t *testing.T) {
	_, env := managerHome(t)
	writeServersConfig(t, fmt.Sprintf("  prog:\n    command: %q\n    env:\n      MOCK_MCP_TOOLS: progress\n", mockBinary(t)))
	ctx := context.Background()
	names, err := ConnectAll(ctx, env, nil)
	if err != nil || !strings.Contains(fmt.Sprint(names), "mcp__prog__progress") {
		t.Fatalf("discovered %v %v", names, err)
	}
	var mu sync.Mutex
	var events []tools.Event
	callEnv := &tools.Env{Home: env.Home, Notify: func(e tools.Event) { mu.Lock(); events = append(events, e); mu.Unlock() }}
	result := tools.NewDispatcher(tools.Default).RegistryDispatch(ctx, &tools.Call{Name: "mcp__prog__progress", Args: map[string]any{}, ToolCallID: "call-7", Env: callEnv})
	if !strings.Contains(result, "done") {
		t.Fatalf("result %s", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || len(events) >= 20 {
		t.Fatalf("events %d", len(events))
	}
	for _, e := range events {
		if e.Kind != "tool.progress" || e.Tool != "mcp__prog__progress" || e.Data["tool_call_id"] != "call-7" {
			t.Fatalf("event %+v", e)
		}
	}
	if last := events[len(events)-1]; last.Data["percent"] != 100 || last.Text != "stage 20" {
		t.Fatalf("last %+v", last)
	}
}
