package acp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

func captureRegistrations(t *testing.T, names []string, defs []map[string]any) map[string]map[string]any {
	t.Helper()
	registered := map[string]map[string]any{}
	swap(t, &registerMCPServers, func(ctx context.Context, env *tools.Env, configs map[string]map[string]any) ([]string, error) {
		for name, cfg := range configs {
			registered[name] = cfg
		}
		return names, nil
	})
	swap(t, &getToolDefinitions, func(context.Context, *tools.Env, []string, []string) []map[string]any { return defs })
	return registered
}

func TestSessionWithMcpServersRegistersTools(t *testing.T) {
	a, m := newTestAgent(t)
	ctx := context.Background()
	servers := []pyacp.McpServer{
		stdioServer("test-fs", "/usr/bin/mcp-fs", []string{"--root", "/tmp"}, pyacp.EnvVariable{Name: "DEBUG", Value: "1"}),
		&pyacp.HttpMcpServer{Name: "test-api", URL: "https://api.example.com/mcp", Headers: []pyacp.HttpHeader{{Name: "Authorization", Value: "Bearer tok123"}}},
	}
	fakeTools := []map[string]any{
		{"function": map[string]any{"name": "mcp_test_fs_read"}}, {"function": map[string]any{"name": "mcp_test_fs_write"}},
		{"function": map[string]any{"name": "mcp_test_api_search"}}, {"function": map[string]any{"name": "terminal"}},
	}
	registered := captureRegistrations(t, []string{"mcp_test_fs_read", "mcp_test_fs_write", "mcp_test_api_search"}, fakeTools)
	resp, err := a.NewSession(ctx, &pyacp.NewSessionRequest{Cwd: "/tmp", MCPServers: servers})
	if err != nil || resp == nil {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	st := m.GetSession(ctx, resp.SessionID)
	fs, ok := registered["test-fs"]
	if !ok || fs["command"] != "/usr/bin/mcp-fs" || !reflect.DeepEqual(fs["args"], []any{"--root", "/tmp"}) || !reflect.DeepEqual(fs["env"], map[string]any{"DEBUG": "1"}) {
		t.Fatalf("stdio config = %v", fs)
	}
	api, ok := registered["test-api"]
	if !ok || api["url"] != "https://api.example.com/mcp" || !reflect.DeepEqual(api["headers"], map[string]any{"Authorization": "Bearer tok123"}) {
		t.Fatalf("http config = %v", api)
	}
	mock := mockOf(t, st)
	if !reflect.DeepEqual(mock.toolDefs, fakeTools) {
		t.Fatalf("tools = %v", mock.toolDefs)
	}
	want := map[string]bool{"mcp_test_fs_read": true, "mcp_test_fs_write": true, "mcp_test_api_search": true, "terminal": true}
	if !reflect.DeepEqual(mock.validToolNames, want) {
		t.Fatalf("valid names = %v", mock.validToolNames)
	}
}

func TestPromptWithToolCallsEmitsAcpEvents(t *testing.T) {
	a, m := newTestAgent(t)
	ctx := context.Background()
	created := newSession(t, a, "/tmp")
	st := m.GetSession(ctx, created.SessionID)
	conn := &captureConn{}
	a.conn = conn
	mock := mockOf(t, st)
	mock.run = func(ctx context.Context, userMessage any, o agent.RunConversationOptions) (map[string]any, error) {
		if progress, ok := mock.toolProgressCallback.(func(event, name, preview string, args map[string]any, extra map[string]any)); ok && progress != nil {
			progress("tool.started", "terminal", "$ echo hello", map[string]any{"command": "echo hello"}, nil)
		}
		if step, ok := mock.stepCallback.(func(apiCallCount int, prevTools []map[string]any)); ok && step != nil {
			step(1, []map[string]any{{"name": "terminal", "result": `{"output": "hello\n", "exit_code": 0}`}})
		}
		return map[string]any{"final_response": "The command output 'hello'.", "messages": []map[string]any{
			{"role": "user", "content": userMessage}, {"role": "assistant", "content": "The command output 'hello'."}}}, nil
	}
	resp, err := a.Prompt(ctx, textPrompt(created.SessionID, "run echo hello"))
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	var starts []*pyacp.ToolCallStart
	var completions []*pyacp.ToolCallProgress
	for _, u := range conn.seen() {
		switch update := u.update.(type) {
		case *pyacp.ToolCallStart:
			starts = append(starts, update)
		case *pyacp.ToolCallProgress:
			completions = append(completions, update)
		}
	}
	if len(starts) < 1 || !strings.Contains(starts[0].Title, "echo hello") {
		t.Fatalf("starts = %+v", starts)
	}
	if len(completions) < 1 || completions[0].Status == nil || *completions[0].Status != "completed" {
		t.Fatalf("completions = %+v", completions)
	}
	if len(completions[0].Content) == 0 {
		t.Fatal("completion carries no content")
	}
	content, ok := completions[0].Content[0].(*pyacp.ContentToolCallContent)
	if !ok {
		t.Fatalf("content = %#v", completions[0].Content[0])
	}
	text, ok := content.Content.(*pyacp.TextContentBlock)
	if !ok || !strings.Contains(text.Text, "hello") {
		t.Fatalf("content text = %#v", content.Content)
	}
	if completions[0].RawOutput != nil {
		t.Fatalf("raw output = %v", completions[0].RawOutput)
	}
}

func TestPatchModeToolStartDefersDiffToEditApprovalPrompt(t *testing.T) {
	args := pyjson.NewObject()
	args.Set("mode", "patch")
	args.Set("patch", "*** Begin Patch\n*** Update File: src/app.py\n@@\n-old line\n+new line\n*** Add File: src/new.py\n+hello\n*** End Patch")
	update := BuildToolStart("tc-1", "patch", args, nil)
	if len(update.Content) != 1 {
		t.Fatalf("content = %v", update.Content)
	}
	if _, ok := update.Content[0].(*pyacp.ContentToolCallContent); !ok {
		t.Fatalf("content type = %T", update.Content[0])
	}
}

func lifecycleSession(t *testing.T, a *HermesACPAgent, m *SessionManager) string {
	t.Helper()
	created := newSession(t, a, "/tmp")
	mock := mockOf(t, m.GetSession(context.Background(), created.SessionID))
	mock.enabledToolsets, mock.disabledToolsets = []string{"icebath-acp"}, nil
	mock.toolDefs, mock.validToolNames = []map[string]any{}, map[string]bool{}
	return created.SessionID
}

func TestLoadSessionRegistersMcp(t *testing.T) {
	a, m := newTestAgent(t)
	sid := lifecycleSession(t, a, m)
	registered := captureRegistrations(t, []string{}, []map[string]any{})
	if _, err := a.LoadSession(context.Background(), &pyacp.LoadSessionRequest{Cwd: "/tmp", SessionID: sid,
		MCPServers: []pyacp.McpServer{stdioServer("srv", "/bin/test", nil)}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := registered["srv"]; !ok {
		t.Fatalf("registered = %v", registered)
	}
}

func TestResumeSessionRegistersMcp(t *testing.T) {
	a, m := newTestAgent(t)
	sid := lifecycleSession(t, a, m)
	registered := captureRegistrations(t, []string{}, []map[string]any{})
	if _, err := a.ResumeSession(context.Background(), &pyacp.ResumeSessionRequest{Cwd: "/tmp", SessionID: sid,
		MCPServers: []pyacp.McpServer{stdioServer("srv2", "/bin/test2", nil)}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := registered["srv2"]; !ok {
		t.Fatalf("registered = %v", registered)
	}
}

func TestForkSessionRegistersMcp(t *testing.T) {
	a, m := newTestAgent(t)
	sid := lifecycleSession(t, a, m)
	registered := captureRegistrations(t, []string{}, []map[string]any{})
	if _, err := a.ForkSession(context.Background(), &pyacp.ForkSessionRequest{Cwd: "/tmp", SessionID: sid,
		MCPServers: []pyacp.McpServer{&pyacp.HttpMcpServer{Name: "api", URL: "https://api.test/mcp", Headers: []pyacp.HttpHeader{}}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := registered["api"]; !ok {
		t.Fatalf("registered = %v", registered)
	}
}
