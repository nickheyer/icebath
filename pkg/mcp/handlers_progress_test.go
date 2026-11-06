package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

type progressCall struct {
	args  []string
	extra map[string]any
}

func TestMCPProgressNotificationsReachToolProgressCallback(t *testing.T) {
	dir, env := managerHome(t)
	writeProfileConfig(t, dir, fmt.Sprintf("mcp_servers:\n  prog:\n    command: %q\n    args: []\n    env:\n      MOCK_MCP_TOOLS: slow\n", mockBinary(t)))
	ctx := context.Background()
	names, err := ConnectAll(ctx, env, nil)
	if err != nil || !strings.Contains(strings.Join(names, ","), "mcp__prog__slow") {
		t.Fatalf("discovered %v %v", names, err)
	}
	var mu sync.Mutex
	var events []progressCall
	bound := tools.BoundToolProgress(ctx, func(event, name, preview string, _ map[string]any, extra map[string]any) {
		mu.Lock()
		events = append(events, progressCall{[]string{event, name, preview}, extra})
		mu.Unlock()
	}, "mcp__prog__slow", "call-7")
	result := tools.NewDispatcher(tools.Default).RegistryDispatch(bound, &tools.Call{Name: "mcp__prog__slow", Args: map[string]any{}, Env: env})
	if !strings.Contains(result, "done") {
		t.Fatalf("result %q", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || len(events) >= 20 {
		t.Fatalf("events %d", len(events))
	}
	for _, e := range events {
		if e.args[0] != tools.ToolProgressEvent || e.args[1] != "mcp__prog__slow" || e.extra["tool_call_id"] != "call-7" {
			t.Fatalf("event %+v", e)
		}
	}
	last := events[len(events)-1]
	if last.extra["percent"] != 100 || last.args[2] != "stage 20" {
		t.Fatalf("last %+v", last)
	}
}

func TestMCPCallOutsideABoundRelaySendsNoProgressToken(t *testing.T) {
	dir, env := managerHome(t)
	record := dir + "/record"
	writeProfileConfig(t, dir, fmt.Sprintf("mcp_servers:\n  prog:\n    command: %q\n    args: []\n    env:\n      MOCK_MCP_TOOLS: slow\n      MOCK_MCP_RECORD: %q\n", mockBinary(t), record))
	ctx := context.Background()
	if _, err := ConnectAll(ctx, env, nil); err != nil {
		t.Fatal(err)
	}
	if result := tools.NewDispatcher(tools.Default).RegistryDispatch(ctx, &tools.Call{Name: "mcp__prog__slow", Args: map[string]any{}, Env: env}); !strings.Contains(result, "done") {
		t.Fatalf("result %q", result)
	}
	if got := recorded(t, record, "tools/call slow"); !strings.Contains(got, "tools/call slow progress=false") {
		t.Fatalf("recorded %q", got)
	}
}
