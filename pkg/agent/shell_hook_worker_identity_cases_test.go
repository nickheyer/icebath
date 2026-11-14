package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRealToolHooksReceiveOnlyTheOwningWorkersIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix hook scripts")
	}
	dir := t.TempDir()
	h := filepath.Join(dir, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_TASK", "t_owner")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	plugins.ResetPluginManagersForTests()
	plugins.ResetShellHooksForTests()
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{InvokeLifecycleHook: LifecycleInvokeHook})
	t.Cleanup(func() {
		plugins.InstallServices(previous)
		plugins.ResetShellHooksForTests()
		plugins.ResetPluginManagersForTests()
	})
	capture := filepath.Join(dir, "captured.json")
	script := filepath.Join(dir, "gate.sh")
	body := fmt.Sprintf("#!/usr/bin/env bash\n{ cat; printf '\\n%%s' \"${ICEBATH_KANBAN_TASK-<unset>}\"; } > %q\nprintf '{\"action\":\"block\", \"message\":\"verified gate fired\"}\\n'\n", capture)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := map[string]any{"hooks": map[string]any{
		"pre_tool_call":  []any{map[string]any{"matcher": "kanban_complete", "command": script}},
		"post_tool_call": []any{map[string]any{"matcher": "kanban_complete", "command": script}},
	}}
	if registered, err := plugins.RegisterShellHooksFromConfig(ctx, cfg, true, false); err != nil || len(registered) != 2 {
		t.Fatalf("registered %v %v", registered, err)
	}
	read := func() (map[string]any, string) {
		t.Helper()
		raw, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(capture)
		text := string(raw)
		i := strings.LastIndex(text, "\n")
		var payload map[string]any
		if err := json.Unmarshal([]byte(text[:i]), &payload); err != nil {
			t.Fatal(err)
		}
		return payload, text[i+1:]
	}
	owner := &tools.Env{Home: h, Values: &sync.Map{}}
	nonOwner := &tools.Env{Home: h, Values: &sync.Map{}}
	restore := tools.EnterNonDispatcherOwnedContext(nonOwner)
	t.Cleanup(restore)
	scopes := []struct {
		ctx  context.Context
		env  *tools.Env
		want string
	}{{ctx, owner, "t_owner"}, {tools.WithDelegatedChildContext(ctx, true), owner, ""}, {tools.WithToolEnv(ctx, nonOwner), nonOwner, ""}, {ctx, owner, "t_owner"}}
	ids := tools.ToolHookIDs{TaskID: "turn-id", SessionID: "session-id", ToolCallID: "call-id"}
	for _, s := range scopes {
		directive := pluginPreToolCall(s.ctx, s.env, "kanban_complete", map[string]any{}, ids, nil)
		if directive.Action != "block" || directive.Message != "verified gate fired" {
			t.Fatalf("directive %+v", directive)
		}
		payload, workerEnv := read()
		extra, _ := payload["extra"].(map[string]any)
		if extra["kanban_task_id"] != s.want || extra["task_id"] != "turn-id" || payload["session_id"] != "session-id" || workerEnv != "<unset>" {
			t.Fatalf("pre_tool_call payload %v worker env %q, want task %q", payload, workerEnv, s.want)
		}
		pluginPostToolCall(s.ctx, s.env, tools.PostToolCallEvent{ToolName: "kanban_complete", Args: map[string]any{}, Result: "{}", IDs: ids})
		payload, _ = read()
		extra, _ = payload["extra"].(map[string]any)
		if payload["hook_event_name"] != "post_tool_call" || extra["kanban_task_id"] != s.want {
			t.Fatalf("post_tool_call payload %v", payload)
		}
	}
}
