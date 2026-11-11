package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

type fakeMCP struct {
	config     map[string]any
	configErr  error
	tools      map[string][]string
	failing    map[string]error
	status     []map[string]any
	registered []string
}

func (f *fakeMCP) LoadConfig(context.Context) (map[string]any, error) { return f.config, f.configErr }
func (f *fakeMCP) FilterSuspicious(s map[string]any) map[string]any   { return s }
func (f *fakeMCP) RegisterServers(_ context.Context, s map[string]any) ([]string, error) {
	for name := range s {
		if err := f.failing[name]; err != nil {
			return nil, err
		}
		f.registered = append(f.registered, name)
	}
	return nil, nil
}
func (f *fakeMCP) RegisteredToolNames(_ context.Context, server string) ([]string, error) {
	return f.tools[server], nil
}
func (f *fakeMCP) UtilityKinds() []string {
	return []string{"list_resources", "read_resource", "list_prompts", "get_prompt", "list_event_types", "subscribe_event", "unsubscribe_event"}
}
func (f *fakeMCP) Status(context.Context) ([]map[string]any, error) { return f.status, nil }
func (f *fakeMCP) SanitizeNameComponent(v string) string            { return strings.ReplaceAll(v, "-", "_") }
func (f *fakeMCP) ToolNamePrefix() string                           { return "mcp__" }

type fakeRendezvous struct {
	records    map[string]*HostRecord
	consistent map[string]bool
	tokens     map[string]string
	host       string
}

func (f *fakeRendezvous) ReadRecord(role string) (*HostRecord, error) { return f.records[role], nil }
func (f *fakeRendezvous) RecordTokenIsConsistent(r *HostRecord) bool  { return f.consistent[r.Role] }
func (f *fakeRendezvous) ReadToken(role string) (string, error)       { return f.tokens[role], nil }
func (f *fakeRendezvous) DialHost(*HostRecord) string                 { return f.host }

type gatewayCall struct{ target, profileHome string }

// Installs activation services for one test, restoring the previous set at cleanup
func installActivationServices(t *testing.T, s *Services) {
	t.Helper()
	previous := installedServices()
	InstallServices(s)
	t.Cleanup(func() { InstallServices(previous) })
}

// Gateway answering the own socket with answer and the multiplexer socket with shared
func fakeGateway(answer, shared map[string]any, calls *[]gatewayCall) func(context.Context, string, string) (map[string]any, error) {
	return func(_ context.Context, target, profileHome string) (map[string]any, error) {
		*calls = append(*calls, gatewayCall{target, profileHome})
		if profileHome == "" {
			return answer, nil
		}
		return shared, nil
	}
}

const liveSkill = "blender:blender"

var liveTools = []any{"mcp__blender__execute_blender_code", "mcp__blender__get_objects_summary", "mcp__blender__render_viewport_to_path"}

func liveActivation(servers []any, skills ...string) map[string]any {
	items := []any{}
	for _, s := range skills {
		items = append(items, map[string]any{"name": s, "description": ""})
	}
	return map[string]any{"name": "blender", "key": "blender", "deferred": map[string]any{},
		"live_now": map[string]any{"mcp_servers": servers, "skills": items}}
}

// Port of test_the_notice_names_the_skill_and_the_tool_prefix_and_lists_no_tool
func TestLiveNoticeNamesSkillAndToolPrefixAndListsNoTool(t *testing.T) {
	installActivationServices(t, &Services{MCP: &fakeMCP{}})
	note, err := liveNotice(liveActivation([]any{
		map[string]any{"name": "blender", "connected": true, "tools": liveTools},
		map[string]any{"name": "blender-render", "connected": false, "tools": []any{}, "error": "spawn failed"},
	}, liveSkill))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{liveSkill + ": load it with skill_view", "MCP server blender: 3 tools, mcp__blender__*.",
		"MCP server blender-render: not connected (spawn failed).", "tool_search", "tool_describe", "tool_call"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note lacks %q:\n%s", want, note)
		}
	}
	for _, tool := range liveTools {
		if strings.Contains(note, tool.(string)) {
			t.Fatalf("note lists tool %s", tool)
		}
	}
	if !strings.HasPrefix(note, "[System note:") || !strings.HasSuffix(note, "]") || len(note) >= 600 {
		t.Fatalf("note is not a short control frame: %q", note)
	}
}

// Port of test_a_plugin_with_only_skills_is_still_announced_and_one_with_nothing_live_is_not
func TestLiveNoticeAnnouncesSkillsOnlyAndNothingWhenNothingLive(t *testing.T) {
	note, err := liveNotice(liveActivation([]any{}, liveSkill))
	if err != nil || !strings.Contains(note, liveSkill) || strings.Contains(note, "tool_search") {
		t.Fatalf("note = %q, %v", note, err)
	}
	if note, err := liveNotice(liveActivation([]any{})); note != "" || err != nil {
		t.Fatalf("nothing live still announced: %q, %v", note, err)
	}
}

func TestLiveNoticeCountsDeferredToolsAndPromptSections(t *testing.T) {
	a := liveActivation([]any{}, liveSkill)
	a["deferred"] = map[string]any{"tools": []any{"a", "b"}, "prompt": []any{"p"}}
	note, _ := liveNotice(a)
	if !strings.Contains(note, "Next session: 2 Python tools, 1 prompt sections.") {
		t.Fatalf("note = %q", note)
	}
}

func TestConnectPluginMCPReportsEveryServerOnce(t *testing.T) {
	mcp := &fakeMCP{config: map[string]any{"configured": map[string]any{"command": "x"}},
		tools:   map[string][]string{"configured": {"mcp__configured__run", "mcp__configured__list_resources"}, "portable": {}},
		failing: map[string]error{"broken": errors.New("spawn failed")},
		status:  []map[string]any{{"name": "portable", "error": "handshake timed out"}}}
	installActivationServices(t, &Services{MCP: mcp})
	activation := map[string]any{"deferred": map[string]any{"mcp_servers": []any{"configured", "portable", "broken", "absent"}}}
	portable := map[string]map[string]any{"portable": {"command": "y"}, "broken": {"command": "z"}}
	rows := connectPluginMCP(context.Background(), activation, portable)
	want := []any{
		map[string]any{"name": "configured", "connected": true, "tools": []any{"mcp__configured__run"}},
		map[string]any{"name": "portable", "connected": false, "tools": []any{}, "error": "handshake timed out"},
		map[string]any{"name": "broken", "connected": false, "tools": []any{}, "error": "spawn failed"},
		map[string]any{"name": "absent", "connected": false, "tools": []any{}, "error": "no MCP configuration"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %#v", rows)
	}
	mcp.configErr = errors.New("bad yaml")
	rows = connectPluginMCP(context.Background(), map[string]any{"deferred": map[string]any{"mcp_servers": []any{"configured"}}}, nil)
	if !reflect.DeepEqual(rows, []any{map[string]any{"name": "configured", "connected": false, "tools": []any{}, "error": "bad yaml"}}) {
		t.Fatalf("config failure rows = %#v", rows)
	}
	if rows := connectPluginMCP(context.Background(), map[string]any{}, nil); len(rows) != 0 {
		t.Fatalf("no deferred servers still connected %v", rows)
	}
}

func TestPluginActivationSummaryReadsWhatThePluginRegistered(t *testing.T) {
	m := NewManager(t.TempDir())
	mf := &Manifest{Name: "probe", Key: "group/probe", ProvidesTools: []string{"declared_tool"}}
	m.plugins["group/probe"] = &LoadedPlugin{Manifest: mf, ToolsRegistered: []string{"loaded_tool"}, Enabled: true}
	m.plugins["broken"] = &LoadedPlugin{Manifest: &Manifest{Name: "broken", Key: "broken"}, Error: "boom"}
	for _, r := range [][2]string{{"command", "zeta"}, {"command", "alpha"}, {"hook", "pre_command"}, {"hook", "on_session_start"},
		{"hook", "pre_command"}, {"slack_action_handler", "approve"}, {"locale", "fr.cli"}, {"tool", "ledger_tool"},
		{"system_prompt_section", "intro"}, {"portable_mcp", "demo"}} {
		m.trackRegistration(mf, r[0], r[1], nil, false)
	}
	gone := m.trackRegistration(mf, "command", "released", nil, false)
	gone.disposed = true
	m.platformHandlerFactories["telegram"] = []*PlatformHandlerFactory{{Plugin: "other"}, {Plugin: "probe"}}
	m.platformHandlerFactories["discord"] = []*PlatformHandlerFactory{{Plugin: "other"}}
	m.portableMCPServerPlugins["owned"] = "group/probe"
	got := ActivationSummaries(context.Background(), m)
	want := []map[string]any{{"name": "probe", "key": "group/probe",
		"activated_now": map[string]any{"gateway_commands": []any{"alpha", "zeta"}, "gateway_transforms": []any{"pre_command"},
			"hooks": []any{"on_session_start"}, "callbacks": []any{"telegram", "slack:approve"}, "locales": []any{"fr.cli"}},
		"deferred": map[string]any{"tools": []any{"declared_tool", "ledger_tool", "loaded_tool"}, "prompt": []any{"intro"},
			"mcp_servers": []any{"demo", "owned"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries = %#v", got)
	}
	if found, _ := findActivation(summaryItems(got), "probe"); found == nil {
		t.Fatal("find by leaf failed")
	}
	if found, _ := findActivation(summaryItems(got), "group/probe"); found == nil {
		t.Fatal("find by key failed")
	}
	if found, _ := findActivation(summaryItems(got), "other"); found != nil {
		t.Fatal("found an unknown plugin")
	}
	if _, err := findActivation([]any{"not a dict"}, "x"); err == nil {
		t.Fatal("a non dict entry must raise like dict.get on a str")
	}
}

// Port of the summary assertion of test_discovery_says_restart_when_the_selected_generation_has_the_dependency
func TestActivationSummaryCarriesMissingDependencies(t *testing.T) {
	selectGeneration(t, true, "1.0")
	t.Setenv("PYTHONPATH", "")
	m := NewManager(t.TempDir())
	m.plugins["depprobe"] = &LoadedPlugin{Manifest: &Manifest{Name: "depprobe", Key: "depprobe", PythonDeps: []string{depReq}}}
	summary := pluginActivationSummary(context.Background(), m, "depprobe")
	if !reflect.DeepEqual(summary["missing_dependencies"], []any{depReq}) {
		t.Fatalf("summary = %#v", summary)
	}
	m.plugins["depprobe"].Error = "ModuleNotFoundError"
	blocked := DependencyBlockedSummaries(context.Background(), m)
	if len(blocked) != 1 || !reflect.DeepEqual(blocked[0]["missing_dependencies"], []any{depReq}) || len(ActivationSummaries(context.Background(), m)) != 0 {
		t.Fatalf("blocked = %#v", blocked)
	}
}

// Gateway answering with the given activations and no serve backend
func depGatewayAnswers(t *testing.T, activations []any, backend *fakeRendezvous) {
	calls := []gatewayCall{}
	if backend == nil {
		backend = &fakeRendezvous{}
	}
	installActivationServices(t, &Services{HostRendezvous: backend,
		ReloadGatewayPlugins: fakeGateway(map[string]any{"reloaded": true, "plugins": []any{"depprobe"}, "activations": activations}, nil, &calls)})
}

func depEntry(missing ...string) map[string]any {
	e := map[string]any{"name": "depprobe", "key": "depprobe", "activated_now": map[string]any{}, "deferred": map[string]any{}}
	if len(missing) > 0 {
		e["missing_dependencies"] = anyList(missing)
	}
	return e
}

func activateHint(t *testing.T) (map[string]any, string) {
	t.Helper()
	ctx := context.Background()
	result, err := activatePluginNowFrom(ctx, "depprobe", false)
	if err != nil {
		t.Fatal(err)
	}
	hint, err := activationHint(ctx, result)
	if err != nil {
		t.Fatal(err)
	}
	return result, hint
}

// Port of test_enable_reports_restart_when_the_running_gateway_cannot_import_the_dependency
func TestEnableReportsRestartWhenTheRunningGatewayCannotImportTheDependency(t *testing.T) {
	selectGeneration(t, true, "1.0")
	depGatewayAnswers(t, []any{depEntry(depReq)}, nil)
	result, hint := activateHint(t)
	act := result["activation"].(map[string]any)
	if result["gateway_reloaded"] != true || result["restart_required"] != true ||
		!reflect.DeepEqual(act["missing_dependencies"], []any{depReq}) || !reflect.DeepEqual(act["missing_in"], []any{"gateway"}) {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(hint, "nothing of this plugin needs") || !strings.Contains(hint, "installed after the running gateway") ||
		!strings.Contains(hint, "icebath gateway restart") {
		t.Fatalf("hint = %q", hint)
	}
}

// Serve backend answering every activation request with body, recording the token header
func serveBackend(t *testing.T, body map[string]any, seen *[]string) *fakeRendezvous {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path+" "+r.Header.Get("X-Icebath-Session-Token"))
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		*seen = append(*seen, req["name"].(string))
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return &fakeRendezvous{records: map[string]*HostRecord{hostRoleServe: {Role: hostRoleServe, Port: port},
		hostRoleDesktopServe: {Role: hostRoleDesktopServe, Port: port}}, consistent: map[string]bool{hostRoleDesktopServe: true},
		tokens: map[string]string{hostRoleDesktopServe: "desktop-spawn-token"}, host: u.Hostname()}
}

// Port of test_restart_names_only_the_process_that_cannot_import
func TestRestartNamesOnlyTheProcessThatCannotImport(t *testing.T) {
	selectGeneration(t, true, "1.0")
	t.Setenv("no_proxy", "*")
	seen := []string{}
	backend := serveBackend(t, map[string]any{"activation": depEntry(depReq)}, &seen)
	depGatewayAnswers(t, []any{depEntry()}, backend)
	result, hint := activateHint(t)
	act := result["activation"].(map[string]any)
	if result["restart_required"] != true || !reflect.DeepEqual(act["missing_in"], []any{"backend"}) {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(hint, "desktop app") || strings.Contains(hint, "icebath gateway restart") {
		t.Fatalf("hint = %q", hint)
	}
	if !reflect.DeepEqual(seen, []string{"/api/dashboard/agent-plugins/activate desktop-spawn-token", "depprobe"}) {
		t.Fatalf("backend saw %v", seen)
	}
}

// Port of the notify half of test_desktop_backend_does_not_claim_the_host_serve_record
func TestNotifyServeBackendFindsTheDesktopRecord(t *testing.T) {
	t.Setenv("no_proxy", "*")
	seen := []string{}
	backend := serveBackend(t, map[string]any{"ok": true}, &seen)
	delete(backend.records, hostRoleServe)
	installActivationServices(t, &Services{HostRendezvous: backend})
	got := notifyServeBackend(context.Background(), "demo", t.TempDir())
	if !reflect.DeepEqual(got, map[string]any{"ok": true}) || len(seen) != 2 || seen[0] != "/api/dashboard/agent-plugins/activate desktop-spawn-token" {
		t.Fatalf("got %v, seen %v", got, seen)
	}
	installActivationServices(t, &Services{HostRendezvous: &fakeRendezvous{}})
	if got := notifyServeBackend(context.Background(), "demo", t.TempDir()); got != nil {
		t.Fatalf("no backend answered %v", got)
	}
}

// Port of test_enable_reports_repair_when_the_dependency_was_never_installed
func TestEnableReportsRepairWhenTheDependencyWasNeverInstalled(t *testing.T) {
	selectGeneration(t, false, "1.0")
	depGatewayAnswers(t, []any{depEntry(depReq)}, nil)
	if _, hint := activateHint(t); !strings.Contains(hint, "pm repair") {
		t.Fatalf("hint = %q", hint)
	}
}

// Port of test_enable_needs_no_restart_when_the_gateway_imports_everything
func TestEnableNeedsNoRestartWhenTheGatewayImportsEverything(t *testing.T) {
	selectGeneration(t, true, "1.0")
	depGatewayAnswers(t, []any{depEntry()}, nil)
	result, hint := activateHint(t)
	if result["restart_required"] != false || !strings.Contains(hint, "nothing of this plugin needs") {
		t.Fatalf("result = %#v hint = %q", result, hint)
	}
}

// Profile home under a root whose own gateway answers answer and the shared one shared
func reloadUnder(t *testing.T, answer, shared map[string]any) (string, string, []gatewayCall, string) {
	root := filepath.Join(t.TempDir(), "shared-root")
	h := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	calls := []gatewayCall{}
	installActivationServices(t, &Services{ReloadGatewayPlugins: fakeGateway(answer, shared, &calls)})
	hint := gatewayReloadHint(reloadRunningGateway(context.Background()), "probe")
	return root, h, calls, strings.Join(strings.Fields(hint), " ")
}

var unservedAnswer = map[string]any{"reloaded": false, "error": "home is not served by this gateway"}

// Ports of the reload and hint halves of the test_plugins_disable_reaches_gateway.py disable cases
func TestDisableHintFollowsTheGatewayAnswer(t *testing.T) {
	_, h, calls, out := reloadUnder(t, map[string]any{"reloaded": true, "plugins": []any{}}, nil)
	if len(calls) != 1 || calls[0].target != h || !strings.Contains(out, "running gateway reloaded its plugins and unloaded probe") ||
		strings.Contains(out, "new sessions") || !strings.Contains(out, "platform hooks it already wired up stay until") ||
		!strings.Contains(out, "icebath gateway restart") || strings.Contains(out, "next session") {
		t.Fatalf("calls %v out %q", calls, out)
	}
	_, _, _, out = reloadUnder(t, unservedAnswer, nil)
	if !strings.Contains(out, "refused") || !strings.Contains(out, "home is not served by this gateway") ||
		!strings.Contains(out, "saved config") || strings.Contains(out, "No running gateway answered") {
		t.Fatalf("refusal out %q", out)
	}
	root, h, calls, out := reloadUnder(t, nil, map[string]any{"reloaded": true, "plugins": []any{}})
	if !reflect.DeepEqual(calls, []gatewayCall{{h, ""}, {root, h}}) || !strings.Contains(out, "running gateway reloaded") {
		t.Fatalf("shared calls %v out %q", calls, out)
	}
	_, _, calls, out = reloadUnder(t, nil, unservedAnswer)
	if len(calls) != 2 || !strings.Contains(out, "home is not served by this gateway") || !strings.Contains(out, "saved config") ||
		strings.Contains(out, "No running gateway answered") {
		t.Fatalf("shared refusal calls %v out %q", calls, out)
	}
	_, _, _, out = reloadUnder(t, nil, nil)
	if strings.Contains(out, "next session") || !strings.Contains(out, "icebath gateway restart") {
		t.Fatalf("no answer out %q", out)
	}
}

// Port of the activation hint half of test_enable_prints_only_the_outcome_that_applies
func TestEnableHintPrintsOnlyTheOutcomeThatApplies(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installActivationServices(t, &Services{
		GatewayRunningPid:           func(string, bool) (int, bool, error) { return 4242, true, nil },
		InstalledGatewayServiceKind: func(context.Context) (string, bool, error) { return "", false, nil },
	})
	for _, reloaded := range []bool{true, false} {
		var activation any
		if reloaded {
			activation = map[string]any{"activated_now": map[string]any{"hooks": 1}}
		}
		out, err := activationHint(context.Background(), map[string]any{"gateway_reloaded": reloaded, "restart_required": !reloaded, "activation": activation})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "next session") || strings.Contains(out, "active in the running gateway now") != reloaded ||
			strings.Contains(out, "icebath gateway restart") == reloaded {
			t.Fatalf("reloaded=%v out %q", reloaded, out)
		}
	}
	installActivationServices(t, &Services{
		GatewayRunningPid:           func(string, bool) (int, bool, error) { return 0, false, nil },
		InstalledGatewayServiceKind: func(context.Context) (string, bool, error) { return "", false, nil },
	})
	if out, err := activationHint(context.Background(), map[string]any{"gateway_reloaded": false}); out != "" || err != nil {
		t.Fatalf("first time setup still printed %q, %v", out, err)
	}
}

func TestReportActivationPrintsTheLiveCountsAndFailures(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("no_proxy", "*")
	seen := []string{}
	backend := serveBackend(t, map[string]any{"activation": map[string]any{"name": "blender", "key": "blender",
		"activated_now": map[string]any{}, "deferred": map[string]any{"tools": []any{"t"}},
		"live_now": map[string]any{"skills": []any{map[string]any{"name": liveSkill}}, "mcp_servers": []any{
			map[string]any{"name": "blender", "connected": true, "tools": liveTools},
			map[string]any{"name": "render", "connected": false, "tools": []any{}}}}}}, &seen)
	calls := []gatewayCall{}
	installActivationServices(t, &Services{HostRendezvous: backend, ReloadGatewayPlugins: fakeGateway(map[string]any{"reloaded": true,
		"activations": []any{map[string]any{"name": "blender", "key": "blender", "activated_now": map[string]any{"hooks": []any{"x"}}, "deferred": map[string]any{}}}}, nil, &calls)})
	var buf strings.Builder
	if err := reportActivation(context.Background(), &Console{Out: &buf}, "blender"); err != nil {
		t.Fatal(err)
	}
	want := "Live in open chats now: 3 MCP tools, 1 skills.\nMCP server render not connected: unknown error\n" +
		"Gateway reloaded plugins — deferred: tools (next session).\n"
	if buf.String() != want {
		t.Fatalf("printed %q", buf.String())
	}
}

// Port of the goLive path for a portable plugin whose MCP server connects and reaches the open chats
func TestLoadAndGoLiveConnectsPortableServersAndRefreshesChats(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	dir := filepath.Join(h, "plugins", "demo-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"$schema": PluginSchemaV1, "name": "demo-plugin"})
	mcpJSON, _ := json.Marshal(map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{"demo": map[string]any{"type": "stdio", "command": "demo-server"}}})
	writeFile(t, filepath.Join(dir, "plugin.json"), string(manifest))
	writeFile(t, filepath.Join(dir, "mcp.json"), string(mcpJSON))
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [demo-plugin]\n")
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	mcp := &fakeMCP{config: map[string]any{}, tools: map[string][]string{"demo": {"mcp__demo__run"}}}
	var refreshed []string
	installActivationServices(t, &Services{MCP: mcp, RefreshPluginSessions: func(_ context.Context, hh, key, note string) error {
		refreshed = append(refreshed, hh, key, note)
		return nil
	}})
	ctx := home.WithOverride(context.Background(), h)
	activation, err := LoadAndGoLive(ctx, "demo-plugin")
	if err != nil || activation == nil {
		t.Fatalf("activation = %v, %v", activation, err)
	}
	live := activation["live_now"].(map[string]any)
	if !reflect.DeepEqual(live["mcp_servers"], []any{map[string]any{"name": "demo", "connected": true, "tools": []any{"mcp__demo__run"}}}) {
		t.Fatalf("live = %#v", live)
	}
	if _, kept := activation["deferred"].(map[string]any)["mcp_servers"]; kept || !reflect.DeepEqual(mcp.registered, []string{"demo"}) {
		t.Fatalf("activation = %#v registered %v", activation, mcp.registered)
	}
	if len(refreshed) != 3 || refreshed[0] != h || !strings.Contains(refreshed[2], "MCP server demo: 1 tools, mcp__demo__*.") {
		t.Fatalf("refreshed = %q", refreshed)
	}
	if missing, err := LoadAndGoLive(ctx, "absent"); missing != nil || err != nil {
		t.Fatalf("an unknown plugin went live: %v, %v", missing, err)
	}
}
