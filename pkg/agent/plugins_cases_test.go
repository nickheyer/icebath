package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func pluginsCasesWrite(t *testing.T, path string, v any) {
	t.Helper()
	body, isText := v.(string)
	if !isText {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pluginsCasesDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestForceReloadSymmetryPreToolCallTimeoutDoesNotReachToolHandler(t *testing.T) {
	c := bridgePluginContext(t)
	pluginsCasesWrite(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), "plugins:\n  hook_callback_timeout: 0.1\n")
	hold := make(chan struct{})
	defer close(hold)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		<-hold
		return nil, nil
	}, false)
	var dispatchCalls [][]any
	d, env := bridgedDispatcher(t, func(_ context.Context, call *tools.Call) string {
		dispatchCalls = append(dispatchCalls, []any{call.Name, call.Args})
		return `{"ok": true}`
	})
	result := d.Dispatch(context.Background(), &tools.Call{Name: "web_search", Args: map[string]any{"query": "test"}, TaskID: "t1", SessionID: "s1", Env: env})
	if len(dispatchCalls) != 0 {
		t.Fatalf("the tool handler ran: %v", dispatchCalls)
	}
	if !strings.Contains(result, "pre_tool_call plugin callback timed out or is still running") {
		t.Fatalf("result %q", result)
	}
}
