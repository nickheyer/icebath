package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Isolated home and global registry whose MCP tools are dropped again when the test ends
func refreshEnv(t *testing.T) (context.Context, *tools.Env) {
	t.Helper()
	dir := isolateHome(t)
	ctx := context.Background()
	isolateMCPRegistry(t, ctx)
	t.Cleanup(func() { isolateMCPRegistry(t, ctx) })
	return ctx, &tools.Env{Home: dir}
}

// Session whose server lists exactly the named tools
func listingSession(t *testing.T, name string, toolNames ...string) *Session {
	t.Helper()
	listed := []any{}
	for _, n := range toolNames {
		listed = append(listed, map[string]any{"name": n, "description": ""})
	}
	tr := &handlerTransport{in: make(chan *Message, 8), respond: func(method string, params map[string]any) (any, error) {
		return map[string]any{"tools": listed}, nil
	}}
	t.Cleanup(func() { _ = tr.Close() })
	return NewSession(tr, SessionOptions{Name: name})
}

// Task whose server advertises tools only, the Go form of the Python session that lists tools alone
func toolsOnlyServer(env *tools.Env, name string) *serverTask {
	s := newServerTask(name, env)
	s.config = map[string]any{}
	s.initializeResult = &InitializeResult{Capabilities: capsOf(false, false)}
	return s
}

// Registers a tool straight into the global registry under a server's toolset
func seedRegistryTool(t *testing.T, name, toolset string) {
	t.Helper()
	tools.Default.Register(tools.Def{Name: name, Toolset: toolset, Schema: map[string]any{},
		Handler: func(context.Context, *tools.Call) string { return "" }, Check: func(*tools.Env) bool { return true }})
}

// Whether the global registry holds name
func registered(ctx context.Context, name string) bool {
	return tools.Default.Lookup(ctx, "", name) != nil
}

func TestDynamicDiscoveryRegisterExposesLiveServerAliases(t *testing.T) {
	ctx, env := refreshEnv(t)
	s := regServer(env, "my_srv", nil, &Tool{Name: "my_tool", Description: "desc"})
	names, err := registerServerTools(ctx, "my_srv", s, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "mcp__my_srv__my_tool") || !registered(ctx, "mcp__my_srv__my_tool") {
		t.Fatalf("registered %v", names)
	}
	if !tools.Default.ValidateToolset(ctx, "", "my_srv") {
		t.Fatal("the live server alias must validate")
	}
	if !slices.Contains(tools.Default.ResolveToolset(ctx, "", "my_srv"), "mcp__my_srv__my_tool") {
		t.Fatal("the live server alias must resolve to the server's tools")
	}
}

func TestDynamicDiscoveryCollidingStaticToolsetNameMergesBothToolSets(t *testing.T) {
	ctx, env := refreshEnv(t)
	static, ok := tools.Default.GetToolset(ctx, "", "browser", false)
	if !ok || len(static.Tools) == 0 {
		t.Fatal("browser must be a static toolset")
	}
	s := regServer(env, "browser", nil, &Tool{Name: "get_entities", Description: "List entities"})
	names, err := registerServerTools(ctx, "browser", s, map[string]any{})
	if err != nil || !slices.Contains(names, "mcp__browser__get_entities") {
		t.Fatalf("registered %v %v", names, err)
	}
	merged, _ := tools.Default.GetToolset(ctx, "", "browser", true)
	for _, name := range static.Tools {
		if !slices.Contains(merged.Tools, name) {
			t.Fatalf("static tool %s dropped from %v", name, merged.Tools)
		}
	}
	if !slices.Contains(merged.Tools, "mcp__browser__get_entities") {
		t.Fatalf("the MCP server's tools are shadowed %v", merged.Tools)
	}
	if !slices.Contains(tools.Default.ResolveToolset(ctx, "", "browser"), "mcp__browser__get_entities") {
		t.Fatal("resolving browser must include the MCP server's tools")
	}
}

func TestDynamicDiscoveryRefreshNukeAndRepave(t *testing.T) {
	ctx, env := refreshEnv(t)
	s := toolsOnlyServer(env, "live_srv")
	seedRegistryTool(t, "mcp__live_srv__old_tool", "mcp-live_srv")
	s.toolNames = []string{"mcp__live_srv__old_tool"}
	s.setSession(listingSession(t, "live_srv", "new_tool"))
	if err := s.refreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	resolved := tools.Default.ResolveToolset(ctx, "", "live_srv")
	if registered(ctx, "mcp__live_srv__old_tool") || slices.Contains(resolved, "mcp__live_srv__old_tool") {
		t.Fatal("the old tool survived the refresh")
	}
	if !registered(ctx, "mcp__live_srv__new_tool") || !slices.Contains(resolved, "mcp__live_srv__new_tool") {
		t.Fatalf("the new tool is missing %v", resolved)
	}
	if got := s.registeredToolNames(); !slices.Equal(got, []string{"mcp__live_srv__new_tool"}) {
		t.Fatalf("owned names %v", got)
	}
}

func TestDynamicDiscoveryRestartWithNoneSessionSkipsWithoutCrash(t *testing.T) {
	ctx, env := refreshEnv(t)
	s := regServer(env, "restart_srv", capsOf(false, false), &Tool{Name: "old_tool"})
	s.config = map[string]any{}
	names, err := registerServerTools(ctx, "restart_srv", s, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	s.toolNames = names
	s.setSession(nil)
	if err := s.refreshTools(ctx); err != nil {
		t.Fatalf("refresh without a session %v", err)
	}
	if !registered(ctx, "mcp__restart_srv__old_tool") || !slices.Contains(tools.Default.ResolveToolset(ctx, "", "restart_srv"), "mcp__restart_srv__old_tool") {
		t.Fatal("the previous registration must stay intact")
	}
	if got := s.registeredToolNames(); !slices.Equal(got, []string{"mcp__restart_srv__old_tool"}) || len(s.tools) != 1 {
		t.Fatalf("owned names %v tools %d", got, len(s.tools))
	}
}

func TestDynamicDiscoveryBackgroundRefreshSurvivesRestartReconnectCycle(t *testing.T) {
	ctx, env := refreshEnv(t)
	logs := captureLogs(t)
	s := toolsOnlyServer(env, "reconnect_srv")
	s.tools = []*Tool{{Name: "old_tool"}}
	s.toolNames = []string{"mcp__reconnect_srv__old_tool"}
	seedRegistryTool(t, "mcp__reconnect_srv__old_tool", "mcp-reconnect_srv")
	s.setSession(nil)
	s.refreshMu.Lock()
	s.scheduleToolsRefresh()
	finished := make(chan struct{})
	go func() {
		s.refreshWG.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("the scheduled refresh must stay tracked until it finishes")
	case <-time.After(50 * time.Millisecond):
	}
	s.refreshMu.Unlock()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduled refresh never finished")
	}
	for _, r := range logs.all() {
		if r.level >= slog.LevelError {
			t.Fatalf("refresh logged an error %q", r.msg)
		}
	}
	if got := s.registeredToolNames(); !slices.Equal(got, []string{"mcp__reconnect_srv__old_tool"}) {
		t.Fatalf("owned names after the skipped refresh %v", got)
	}
	s.setSession(listingSession(t, "reconnect_srv", "new_tool"))
	if err := s.refreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.registeredToolNames(); !slices.Equal(got, []string{"mcp__reconnect_srv__new_tool"}) {
		t.Fatalf("owned names after reconnect %v", got)
	}
	if registered(ctx, "mcp__reconnect_srv__old_tool") {
		t.Fatal("the old tool survived the live refresh")
	}
}

func TestDynamicDiscoveryRefreshAfterSessionRestoredSucceeds(t *testing.T) {
	ctx, env := refreshEnv(t)
	s := toolsOnlyServer(env, "restored_srv")
	s.setSession(listingSession(t, "restored_srv", "live_tool"))
	if err := s.refreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	if !registered(ctx, "mcp__restored_srv__live_tool") || !slices.Contains(tools.Default.ResolveToolset(ctx, "", "restored_srv"), "mcp__restored_srv__live_tool") {
		t.Fatal("the live tool must be published")
	}
	if got := s.registeredToolNames(); !slices.Equal(got, []string{"mcp__restored_srv__live_tool"}) {
		t.Fatalf("owned names %v", got)
	}
}

// Transport ending with err once closed, delivering only what the test queues
type endingTransport struct {
	handlerTransport
	err error
}

func (e *endingTransport) Err() error { return e.err }

// Feeds msgs through a session built from the task's own options, returning the refreshes it scheduled
func deliverToTask(t *testing.T, s *serverTask, end error, msgs ...*Message) int {
	t.Helper()
	logs := captureLogs(t)
	tr := &endingTransport{handlerTransport: handlerTransport{in: make(chan *Message, len(msgs)), respond: func(string, map[string]any) (any, error) {
		return map[string]any{}, nil
	}}, err: end}
	sess := NewSession(tr, s.sessionOptions())
	for _, m := range msgs {
		tr.in <- m
	}
	_ = tr.Close()
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session never drained its messages")
	}
	s.refreshWG.Wait()
	return logs.count("MCP server '" + s.name + "': skipping dynamic tool refresh; session not connected")
}

func TestMessageHandlerDispatchesToolListChanged(t *testing.T) {
	_, env := refreshEnv(t)
	s := newServerTask("notif_srv", env)
	if n := deliverToTask(t, s, nil, &Message{JSONRPC: "2.0", Method: methodToolsListChanged}); n != 1 {
		t.Fatalf("refreshes scheduled %d", n)
	}
}

func TestMessageHandlerIgnoresExceptionsAndOtherMessages(t *testing.T) {
	_, env := refreshEnv(t)
	s := newServerTask("notif_srv", env)
	stray := &Message{JSONRPC: "2.0", Result: json.RawMessage(`"ok"`)}
	others := []*Message{stray, {JSONRPC: "2.0", Method: methodPromptsListChanged}, {JSONRPC: "2.0", Method: methodResourcesListChanged}}
	if n := deliverToTask(t, s, errors.New("connection dead"), others...); n != 0 {
		t.Fatalf("refreshes scheduled %d", n)
	}
}

func TestCacheLoadDescriptionScanScanRunsOnCacheLoadPath(t *testing.T) {
	ctx, env := refreshEnv(t)
	logs := captureLogs(t)
	entry := map[string]any{"fingerprint": "abc", "utility_tools": []any{}, "tools": []any{map[string]any{
		"name": "browser_navigate", "description": "Navigate <system>", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
	cfg := map[string]any{"command": "npx", "args": []any{}, "lazy": true}
	if _, err := registerFromCacheSync(ctx, env, "playwright", cfg, entry); err != nil {
		t.Fatal(err)
	}
	if n := logs.count("MCP server 'playwright' tool 'browser_navigate': suspicious description content", "Description: Navigate <system>"); n != 1 {
		t.Fatalf("description scans on the cache load path %d", n)
	}
}
