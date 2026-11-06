package mcp

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type snapAgent struct {
	mu                 sync.Mutex
	tools              []map[string]any
	names              map[string]bool
	enabled, disabled  []string
	configuredDisabled []string
	platform           string
	gen                int64
	genSet             bool
	sel                string
	selSet             bool
	engineNames        map[string]bool
	db                 ToolPinStore
	sessionID          string
	backend            string
	memSchemas         func() ([]any, error)
	engineSchemas      func() ([]any, error)
	env                *tools.Env
	drops              map[string]bool
	authorized         bool
	delegationAdopted  int
}

func (a *snapAgent) ToolDefs() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tools
}

func (a *snapAgent) SetToolSnapshot(defs []map[string]any, names map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools, a.names = defs, names
}

func (a *snapAgent) pair() ([]map[string]any, map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tools, a.names
}

func (a *snapAgent) EnabledToolsets() []string  { return a.enabled }
func (a *snapAgent) DisabledToolsets() []string { return a.disabled }
func (a *snapAgent) SetToolsets(enabled, disabled []string) {
	a.enabled, a.disabled = enabled, disabled
}
func (a *snapAgent) SetConfiguredDisabledToolsets(disabled []string) { a.configuredDisabled = disabled }
func (a *snapAgent) ConfiguredDisabledToolsets() []string            { return a.configuredDisabled }
func (a *snapAgent) Platform() string                                { return a.platform }

func (a *snapAgent) SnapshotGeneration() (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen, a.genSet
}

func (a *snapAgent) SnapshotSelection() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sel, a.selSet
}

func (a *snapAgent) RecordSnapshot(generation int64, selection string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gen, a.genSet, a.sel, a.selSet = generation, true, selection, true
}

func (a *snapAgent) PublishContextEngineToolNames(names map[string]bool) {
	if a.engineNames == nil {
		return
	}
	clear(a.engineNames)
	for n := range names {
		a.engineNames[n] = true
	}
}

func (a *snapAgent) ToolPinStore() ToolPinStore                      { return a.db }
func (a *snapAgent) SessionID() string                               { return a.sessionID }
func (a *snapAgent) DelegationBackend() string                       { return a.backend }
func (a *snapAgent) MemoryToolSchemas() func() ([]any, error)        { return a.memSchemas }
func (a *snapAgent) ContextEngineToolSchemas() func() ([]any, error) { return a.engineSchemas }
func (a *snapAgent) ToolEnv() *tools.Env                             { return a.env }
func (a *snapAgent) ToolDrops() map[string]bool                      { return a.drops }
func (a *snapAgent) IsOneshot() bool                                 { return false }
func (a *snapAgent) SavedDelegationBackend() string                  { return "" }
func (a *snapAgent) AdoptBuiltBackend(defs []any, backend any)       {}
func (a *snapAgent) AdoptSessionDelegationMode()                     { a.delegationAdopted++ }
func (a *snapAgent) MessageAgentAuthorized() bool                    { return a.authorized }
func (a *snapAgent) MessageAgentToolSchema() map[string]any          { return snapTool(messageAgentToolName) }

func (a *snapAgent) ToolSurface(defs []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, d := range defs {
		if !a.drops[snapshotDefName(d)] {
			out = append(out, d)
		}
	}
	return out
}

func (a *snapAgent) MemoryProviderToolsEnabled(enabled, disabled []string, memoryToolPresent bool) bool {
	return !containsString(disabled, "memory")
}

func snapTool(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "", "parameters": map[string]any{}}}
}

func describedTool(name, description string, params map[string]any) map[string]any {
	tool := snapTool(name)
	fn := tool["function"].(map[string]any)
	fn["description"] = description
	fn["parameters"] = map[string]any{"type": "object", "properties": params}
	return tool
}

func newSnapAgent(t *testing.T, names []string, enabled, disabled []string) *snapAgent {
	a := &snapAgent{names: map[string]bool{}, enabled: enabled, disabled: disabled, env: &tools.Env{Home: t.TempDir()}}
	for _, n := range names {
		a.tools = append(a.tools, snapTool(n))
		a.names[n] = true
	}
	return a
}

func snapNames(defs []map[string]any) []string {
	out := []string{}
	for _, d := range defs {
		out = append(out, snapshotDefName(d))
	}
	return out
}

func snapSet(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Replaces the tool definition source of the snapshot refresh for one test
func serveSnapshotDefs(t *testing.T, fn func() []map[string]any) {
	prev := agentToolDefinitions
	agentToolDefinitions = func(context.Context, *tools.Env, []string, []string, bool) []map[string]any { return fn() }
	t.Cleanup(func() { agentToolDefinitions = prev })
}

func serveSnapshotList(t *testing.T, defs ...map[string]any) {
	serveSnapshotDefs(t, func() []map[string]any { return append([]map[string]any(nil), defs...) })
}

// Points the snapshot code at a fresh registry holding exactly the given schemas
func useSnapshotRegistry(t *testing.T, toolsets map[string]string, schemas ...map[string]any) *tools.Registry {
	reg := tools.NewRegistry()
	for _, schema := range schemas {
		name := schema["name"].(string)
		ts := toolsets[name]
		if ts == "" {
			ts = "snapshot_test"
		}
		reg.Register(tools.Def{Name: name, Toolset: ts, Schema: schema,
			Handler: func(context.Context, *tools.Call) string { return "" }})
	}
	prev := agentToolRegistry
	agentToolRegistry = func() *tools.Registry { return reg }
	t.Cleanup(func() { agentToolRegistry = prev })
	agentToolsLock.Lock()
	pinnedSnapshots, pinnedSnapshotsOrder = map[pinnedKey]pinnedAgainst{}, nil
	agentToolsLock.Unlock()
	return reg
}

func registeredNames(t *testing.T, names ...string) *tools.Registry {
	schemas := []map[string]any{}
	for _, n := range names {
		schemas = append(schemas, snapTool(n)["function"].(map[string]any))
	}
	return useSnapshotRegistry(t, nil, schemas...)
}

func mustDump(t *testing.T, v any) string {
	t.Helper()
	s, err := pyjson.Dumps(v, pyjson.SortKeys(true))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRefreshAddsLateLandingTools(t *testing.T) {
	registeredNames(t)
	agent := newSnapAgent(t, []string{"read_file", "terminal"}, nil, nil)
	serveSnapshotList(t, snapTool("read_file"), snapTool("terminal"), snapTool("mcp_granola_get_account_info"))
	added := RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true})
	if !reflect.DeepEqual(added, snapSet("mcp_granola_get_account_info")) || !agent.names["mcp_granola_get_account_info"] || len(agent.tools) != 3 {
		t.Fatalf("added=%v names=%v tools=%d", added, agent.names, len(agent.tools))
	}
	side := newSnapAgent(t, []string{"read_file", "terminal"}, nil, nil)
	side.drops = snapSet("manage_connections")
	serveSnapshotList(t, snapTool("read_file"), snapTool("terminal"), snapTool("mcp_granola_get_account_info"), snapTool("manage_connections"))
	RefreshAgentMCPTools(context.Background(), side, RefreshOptions{Quiet: true})
	if side.names["manage_connections"] || containsString(snapNames(side.tools), "manage_connections") {
		t.Fatalf("side agent carries manage_connections: %v", snapNames(side.tools))
	}
}

func TestRefreshPreservesMemoryProviderAndContextEngineTools(t *testing.T) {
	registeredNames(t)
	agent := newSnapAgent(t, []string{"read_file", "memory_search", "lcm_grep"}, nil, nil)
	agent.memSchemas = func() ([]any, error) {
		return []any{map[string]any{"name": "memory_search", "description": "", "parameters": map[string]any{}}}, nil
	}
	agent.engineSchemas = func() ([]any, error) {
		return []any{map[string]any{"name": "lcm_grep", "description": "", "parameters": map[string]any{}}}, nil
	}
	agent.engineNames = snapSet("lcm_grep")
	serveSnapshotList(t, snapTool("read_file"), snapTool("mcp_new_server_tool"))
	added := RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true})
	for _, n := range []string{"mcp_new_server_tool", "memory_search", "lcm_grep"} {
		if !agent.names[n] {
			t.Fatalf("%s missing from %v", n, agent.names)
		}
	}
	if !reflect.DeepEqual(added, snapSet("mcp_new_server_tool")) || !reflect.DeepEqual(agent.engineNames, snapSet("lcm_grep")) {
		t.Fatalf("added=%v engine=%v", added, agent.engineNames)
	}
}

func TestRefreshDoesNotReinjectDisabledMemoryProviderTools(t *testing.T) {
	registeredNames(t)
	agent := newSnapAgent(t, []string{"read_file", "memory_search"}, []string{"all"}, []string{"memory"})
	agent.memSchemas = func() ([]any, error) {
		return []any{map[string]any{"name": "memory_search", "description": "", "parameters": map[string]any{}}}, nil
	}
	serveSnapshotList(t, snapTool("read_file"))
	RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true})
	if agent.names["memory_search"] || containsString(snapNames(agent.tools), "memory_search") {
		t.Fatalf("disabled memory provider tool re-injected: %v", snapNames(agent.tools))
	}
}

func TestRefreshRespectsContextEngineToolsetGate(t *testing.T) {
	registeredNames(t)
	agent := newSnapAgent(t, []string{"read_file"}, []string{"coding"}, nil)
	agent.engineSchemas = func() ([]any, error) {
		return []any{map[string]any{"name": "lcm_grep", "description": "", "parameters": map[string]any{}}}, nil
	}
	agent.engineNames = map[string]bool{}
	serveSnapshotList(t, snapTool("read_file"), snapTool("mcp_new_tool"))
	RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true})
	if !agent.names["mcp_new_tool"] || agent.names["lcm_grep"] {
		t.Fatalf("names=%v", agent.names)
	}
}

func TestRefreshSkipsAFailingSchemaGetter(t *testing.T) {
	registeredNames(t)
	agent := newSnapAgent(t, []string{"read_file"}, nil, nil)
	agent.memSchemas = func() ([]any, error) { return nil, errors.New("provider down") }
	agent.engineSchemas = func() ([]any, error) { return nil, errors.New("engine down") }
	serveSnapshotList(t, snapTool("read_file"), snapTool("mcp_new_tool"))
	if added := RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true}); !reflect.DeepEqual(added, snapSet("mcp_new_tool")) {
		t.Fatalf("added=%v", added)
	}
}

func TestRefreshIsThreadSafeUnderConcurrentCalls(t *testing.T) {
	registeredNames(t)
	agent := newSnapAgent(t, []string{"a"}, nil, nil)
	sets := [][]map[string]any{{snapTool("a"), snapTool("b")}, {snapTool("a"), snapTool("c")}}
	var flipMu sync.Mutex
	flip := 0
	serveSnapshotDefs(t, func() []map[string]any {
		flipMu.Lock()
		defer flipMu.Unlock()
		flip++
		return append([]map[string]any(nil), sets[flip%2]...)
	})
	ab, ac := snapSet("a", "b"), snapSet("a", "c")
	var wg sync.WaitGroup
	errs := make(chan string, 400)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true})
				defs, names := agent.pair()
				got := defNameSet(defs)
				if !reflect.DeepEqual(names, got) || !(reflect.DeepEqual(got, ab) || reflect.DeepEqual(got, ac)) {
					errs <- "torn snapshot"
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if _, names := agent.pair(); !(reflect.DeepEqual(names, ab) || reflect.DeepEqual(names, ac)) {
		t.Fatalf("final names %v", names)
	}
}

func TestWaitReturnsInstantlyWhenNoDiscoveryRun(t *testing.T) {
	dir := t.TempDir()
	writeStartupConfig(t, dir, "mcp_discovery_timeout: 999.0\n")
	start := time.Now()
	WaitForDiscovery(context.Background(), &tools.Env{Home: dir}, nil, false)
	if time.Since(start) >= 200*time.Millisecond {
		t.Fatalf("wait blocked %s with nothing pending", time.Since(start))
	}
}

func TestPreservePrefixCarriesAFlappingToolForward(t *testing.T) {
	agent := newSnapAgent(t, []string{"read_file", "browser_navigate", "terminal"}, nil, nil)
	before := mustDump(t, agent.tools)
	serveSnapshotList(t, snapTool("read_file"), snapTool("terminal"))
	registeredNames(t, "read_file", "browser_navigate", "terminal")
	added := RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true, PreservePrefix: true})
	if len(added) != 0 || mustDump(t, agent.tools) != before || !agent.names["browser_navigate"] {
		t.Fatalf("added=%v tools=%v", added, snapNames(agent.tools))
	}
}

func TestPreservePrefixAppendsLateArrivalsAtTheTail(t *testing.T) {
	agent := newSnapAgent(t, []string{"read_file", "terminal"}, nil, nil)
	serveSnapshotList(t, snapTool("aaa_mcp_late"), snapTool("read_file"), snapTool("terminal"))
	registeredNames(t, "aaa_mcp_late", "read_file", "terminal")
	added := RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true, PreservePrefix: true})
	if !reflect.DeepEqual(added, snapSet("aaa_mcp_late")) || !reflect.DeepEqual(snapNames(agent.tools), []string{"read_file", "terminal", "aaa_mcp_late"}) {
		t.Fatalf("added=%v order=%v", added, snapNames(agent.tools))
	}
}

func TestPreservePrefixKeepsSentToolSchemasByteIdentical(t *testing.T) {
	agent := newSnapAgent(t, []string{"read_file"}, nil, nil)
	agent.tools[0]["function"].(map[string]any)["description"] = "Read a file. schema v1"
	sent := mustDump(t, defsAsAny(agent.tools))
	fresh := describedTool("read_file", "Read a file. schema v2", map[string]any{"path": map[string]any{"type": "string"}})
	serveSnapshotList(t, fresh, snapTool("mcp_late"))
	registeredNames(t, "read_file", "mcp_late")
	RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true, PreservePrefix: true})
	if mustDump(t, defsAsAny(agent.tools[:1])) != sent || !reflect.DeepEqual(snapNames(agent.tools), []string{"read_file", "mcp_late"}) {
		t.Fatalf("tools=%v", agent.tools)
	}
}

func TestExplicitReloadReplacesASameNameSchema(t *testing.T) {
	agent := newSnapAgent(t, []string{"read_file"}, nil, nil)
	agent.tools[0]["function"].(map[string]any)["description"] = "Read a file. schema v1"
	fresh := snapTool("read_file")
	fresh["function"].(map[string]any)["description"] = "Read a file. schema v2"
	serveSnapshotList(t, fresh)
	registeredNames(t, "read_file")
	RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true})
	if got := agent.tools[0]["function"].(map[string]any)["description"]; got != "Read a file. schema v2" {
		t.Fatalf("description %v", got)
	}
}

func TestPreservePrefixKeepsTheBridgeToolsByteIdentical(t *testing.T) {
	built := snapTool("tool_search")
	built["function"].(map[string]any)["description"] = "Search 21 additional tools. connectors__ hint present."
	agent := newSnapAgent(t, []string{"read_file", "manage_connections"}, nil, nil)
	agent.tools = append(agent.tools, built)
	agent.names["tool_search"] = true
	before := mustDump(t, defsAsAny(agent.tools))
	freshBridge := snapTool("tool_search")
	freshBridge["function"].(map[string]any)["description"] = "Search 33 additional tools."
	serveSnapshotList(t, snapTool("read_file"), freshBridge, snapTool("mcp_late_tool"))
	registeredNames(t, "read_file", "manage_connections", "mcp_late_tool", "tool_search", "tool_describe", "tool_call")
	added := RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true, PreservePrefix: true})
	names := snapNames(agent.tools)
	if !reflect.DeepEqual(added, snapSet("mcp_late_tool")) || mustDump(t, defsAsAny(agent.tools[:3])) != before || names[len(names)-1] != "mcp_late_tool" {
		t.Fatalf("added=%v names=%v", added, names)
	}
}

func TestPreservePrefixKeepsADeactivatedBridgeToolInPlace(t *testing.T) {
	built := snapTool("tool_search")
	built["function"].(map[string]any)["description"] = "Search 21 additional tools."
	agent := newSnapAgent(t, []string{"read_file"}, nil, nil)
	agent.tools = append(agent.tools, built, snapTool("terminal"))
	agent.names["tool_search"], agent.names["terminal"] = true, true
	before := mustDump(t, defsAsAny(agent.tools))
	serveSnapshotList(t, snapTool("read_file"), snapTool("terminal"))
	registeredNames(t, "read_file", "terminal")
	RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true, PreservePrefix: true})
	if mustDump(t, defsAsAny(agent.tools)) != before {
		t.Fatalf("tools moved: %v", snapNames(agent.tools))
	}
}

func TestEvictionRebuildRestoresTheSessionsSavedToolOrder(t *testing.T) {
	saved := []string{"read_file", "browser_exec", "terminal"}
	registeredNames(t, saved...)
	rebuilt := newSnapAgent(t, []string{"read_file", "terminal"}, nil, nil)
	if !RestoreAgentToolPrefix(context.Background(), rebuilt, saved) {
		t.Fatal("restore reported no change")
	}
	if !reflect.DeepEqual(snapNames(rebuilt.tools), saved) || !reflect.DeepEqual(rebuilt.names, snapSet(saved...)) {
		t.Fatalf("order=%v names=%v", snapNames(rebuilt.tools), rebuilt.names)
	}
	if rebuilt.delegationAdopted != 1 {
		t.Fatalf("session delegation mode adopted %d times", rebuilt.delegationAdopted)
	}
}

type memPinStore struct {
	mu   sync.Mutex
	pins map[string]string
}

func (s *memPinStore) UpdateSessionToolNames(sessionID string, pin any) error {
	text, err := pyjson.Dumps(pin)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pins[sessionID] = text
	return nil
}

func (s *memPinStore) load(t *testing.T, sessionID string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := pyjson.Loads(s.pins[sessionID])
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestResumeOnAnotherSurfaceRestoresThePinnedToolBytes(t *testing.T) {
	static := describedTool("skill_manage", "lands in the profile's skills dir", map[string]any{})["function"].(map[string]any)
	useSnapshotRegistry(t, nil, snapTool("read_file")["function"].(map[string]any),
		snapTool("delegate_task")["function"].(map[string]any), static)
	sent := newSnapAgent(t, nil, nil, nil)
	sent.tools = []map[string]any{snapTool("read_file"),
		describedTool("delegate_task", "delegate", map[string]any{"group": map[string]any{"type": "string"}}),
		describedTool("skill_manage", "lands in /home/u/.icebath/skills", map[string]any{}),
		describedTool("tool_search", "Search 6 additional tools.", map[string]any{})}
	thisSurface := []map[string]any{snapTool("read_file"), describedTool("delegate_task", "delegate", map[string]any{}),
		describedTool("tool_search", "Search 5 additional tools.", map[string]any{})}
	db := &memPinStore{pins: map[string]string{}}
	sent.db = db
	for _, sid := range []string{"s1", "s2"} {
		sent.sessionID = sid
		PersistAgentToolNames(context.Background(), sent)
	}
	if db.pins["s1"] != db.pins["s2"] {
		t.Fatalf("identical tool arrays pinned different bytes:\n%s\n%s", db.pins["s1"], db.pins["s2"])
	}
	resumed := newSnapAgent(t, nil, nil, nil)
	resumed.tools, resumed.db, resumed.sessionID = append([]map[string]any(nil), thisSurface...), db, "s1"
	pinBefore := db.pins["s1"]
	RestoreAgentToolPrefix(context.Background(), resumed, db.load(t, "s1"))
	repinned := db.pins["s1"]
	prev := toolPinVersion
	toolPinVersion = func() string { return "sha-after-update" }
	t.Cleanup(func() { toolPinVersion = prev })
	updated := newSnapAgent(t, nil, nil, nil)
	updated.tools, updated.db, updated.sessionID = append([]map[string]any(nil), thisSurface...), db, "s2"
	RestoreAgentToolPrefix(context.Background(), updated, db.load(t, "s2"))
	if mustDump(t, defsAsAny(resumed.tools)) != mustDump(t, defsAsAny(sent.tools)) {
		t.Fatalf("resumed tools %v", resumed.tools)
	}
	if !reflect.DeepEqual(resumed.names, snapSet("read_file", "delegate_task", "skill_manage", "tool_search")) {
		t.Fatalf("resumed names %v", resumed.names)
	}
	if repinned != pinBefore || mustDump(t, db.load(t, "s1").(*pyjson.Object).Vals["tools"]) != mustDump(t, defsAsAny(sent.tools)) {
		t.Fatalf("same code pin rewritten: %s", repinned)
	}
	wantUpdated := []map[string]any{thisSurface[0], thisSurface[1], {"type": "function", "function": static}, thisSurface[2]}
	if mustDump(t, defsAsAny(updated.tools)) != mustDump(t, defsAsAny(wantUpdated)) {
		t.Fatalf("updated tools %v", updated.tools)
	}
	upgraded := db.load(t, "s2").(*pyjson.Object)
	if !reflect.DeepEqual(upgraded.Keys, []string{"version", "tools"}) || upgraded.Vals["version"] != "sha-after-update" ||
		mustDump(t, upgraded.Vals["tools"]) != mustDump(t, defsAsAny(updated.tools)) {
		t.Fatalf("upgraded pin %s", db.pins["s2"])
	}
}

func TestAPinNeverReAddsAToolThisSessionsConfigExcludes(t *testing.T) {
	useSnapshotRegistry(t, map[string]string{"read_file": "file", "terminal": "terminal", "browser_exec": "browser", "focus_pane": "desktop_ui"},
		snapTool("read_file")["function"].(map[string]any), snapTool("terminal")["function"].(map[string]any),
		snapTool("browser_exec")["function"].(map[string]any), snapTool("focus_pane")["function"].(map[string]any))
	prev := persistAgentToolNamesFn
	persistAgentToolNamesFn = func(context.Context, AgentToolSnapshot) {}
	t.Cleanup(func() { persistAgentToolNamesFn = prev })
	pin := map[string]any{"version": ToolPinVersion(), "tools": defsAsAny([]map[string]any{
		snapTool("read_file"), snapTool("terminal"), snapTool("browser_exec"), snapTool("focus_pane")})}
	agent := newSnapAgent(t, []string{"read_file"}, []string{"icebath-cli"}, []string{"terminal"})
	RestoreAgentToolPrefix(context.Background(), agent, pin)
	if !reflect.DeepEqual(snapNames(agent.tools), []string{"read_file", "focus_pane"}) || !reflect.DeepEqual(agent.names, snapSet("read_file", "focus_pane")) {
		t.Fatalf("tools=%v names=%v", snapNames(agent.tools), agent.names)
	}
}

func TestReprobeToolAvailabilityDropsCachedCheckVerdicts(t *testing.T) {
	reg := tools.NewRegistry()
	var verdictMu sync.Mutex
	verdict := false
	reg.Register(tools.Def{Name: "probe_tool", Toolset: "probe_toolset", Schema: map[string]any{"name": "probe_tool", "description": "", "parameters": map[string]any{}},
		Handler: func(context.Context, *tools.Call) string { return "" },
		Check: func(*tools.Env) bool {
			verdictMu.Lock()
			defer verdictMu.Unlock()
			return verdict
		}})
	env := &tools.Env{Home: t.TempDir()}
	listed := func() bool {
		defs := reg.ToolDefinitions(context.Background(), env, tools.DefinitionOptions{Enabled: []string{"probe_toolset"}, Quiet: true})
		return containsString(snapNames(defs), "probe_tool")
	}
	if listed() {
		t.Fatal("failing probe listed")
	}
	verdictMu.Lock()
	verdict = true
	verdictMu.Unlock()
	if listed() {
		t.Fatal("cached verdict not replayed before the reprobe")
	}
	epoch := tools.CheckCacheEpoch()
	ReprobeToolAvailability()
	if tools.CheckCacheEpoch() == epoch || !listed() {
		t.Fatal("reprobe did not drop the cached verdict and the definitions memo")
	}
}

func TestAuthorizedMessageAgentSurvivesEverySnapshotRebuild(t *testing.T) {
	for _, rebuild := range []string{"compaction", "reload", "between_turns", "resume"} {
		t.Run(rebuild, func(t *testing.T) {
			registeredNames(t, "read_file")
			serveSnapshotList(t, snapTool("read_file"))
			agent := newSnapAgent(t, []string{"read_file", messageAgentToolName}, nil, nil)
			agent.authorized = true
			for range 2 {
				if rebuild == "resume" {
					agent.SetToolSnapshot([]map[string]any{snapTool("read_file")}, snapSet("read_file"))
					RestoreAgentToolPrefix(context.Background(), agent, []string{"read_file", messageAgentToolName})
				} else {
					RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true,
						ContentAware: rebuild == "compaction", PreservePrefix: rebuild == "between_turns"})
				}
				defs, names := agent.pair()
				count := 0
				for _, n := range snapNames(defs) {
					if n == messageAgentToolName {
						count++
					}
				}
				if count != 1 || !names[messageAgentToolName] || !reflect.DeepEqual(names, defNameSet(defs)) {
					t.Fatalf("count=%d names=%v", count, names)
				}
			}
		})
	}
}

func TestSnapshotRebuildNeverGrantsMessageAgentToUnauthorizedSessions(t *testing.T) {
	registeredNames(t, "read_file")
	serveSnapshotList(t, snapTool("read_file"))
	agent := newSnapAgent(t, []string{"read_file", messageAgentToolName}, nil, nil)
	for range 2 {
		RefreshAgentMCPTools(context.Background(), agent, RefreshOptions{Quiet: true, ContentAware: true})
		defs, names := agent.pair()
		if containsString(snapNames(defs), messageAgentToolName) || names[messageAgentToolName] || !reflect.DeepEqual(names, defNameSet(defs)) {
			t.Fatalf("unauthorized session kept message_agent: %v", snapNames(defs))
		}
	}
}

func compactionAgent(t *testing.T, desc string) *snapAgent {
	agent := newSnapAgent(t, nil, []string{"image_gen"}, nil)
	agent.tools = []map[string]any{describedTool("image_generate", desc, map[string]any{})}
	agent.names = snapSet("image_generate")
	agent.gen, agent.genSet = 0, true
	return agent
}

func compactionRefresh(t *testing.T, agent *snapAgent, desc string, opts RefreshOptions) {
	registeredNames(t, "image_generate")
	serveSnapshotList(t, describedTool("image_generate", desc, map[string]any{}))
	prev := reinjectPostBuildFn
	reinjectPostBuildFn = func(AgentToolSnapshot, *[]map[string]any, map[string]bool) map[string]bool { return map[string]bool{} }
	t.Cleanup(func() { reinjectPostBuildFn = prev })
	opts.Quiet = true
	RefreshAgentMCPTools(context.Background(), agent, opts)
}

func TestContentChangeSwapsWhenContentAware(t *testing.T) {
	agent := compactionAgent(t, "old capabilities text")
	compactionRefresh(t, agent, "new capabilities text", RefreshOptions{ContentAware: true})
	if agent.tools[0]["function"].(map[string]any)["description"] != "new capabilities text" {
		t.Fatalf("tools %v", agent.tools)
	}
}

func TestContentChangeIgnoredWhenPrefixPreserved(t *testing.T) {
	agent := compactionAgent(t, "old capabilities text")
	compactionRefresh(t, agent, "new capabilities text", RefreshOptions{PreservePrefix: true})
	if agent.tools[0]["function"].(map[string]any)["description"] != "old capabilities text" {
		t.Fatalf("tools %v", agent.tools)
	}
}

func TestIdenticalContentKeepsIdentity(t *testing.T) {
	agent := compactionAgent(t, "same text")
	before := agent.tools
	compactionRefresh(t, agent, "same text", RefreshOptions{ContentAware: true})
	if &agent.tools[0] != &before[0] {
		t.Fatal("identical content republished the array")
	}
}

func TestPinnedToolDefsKeepBytesAndResolveLegacyNames(t *testing.T) {
	registeredNames(t, "read_file")
	pinnedDef := describedTool("terminal", "pinned bytes", map[string]any{})
	got := PinnedToolDefs(context.Background(), nil, map[string]any{"version": "other", "tools": []any{pinnedDef, "read_file", "gone", 7}})
	if mustDump(t, defsAsAny(got)) != mustDump(t, defsAsAny([]map[string]any{pinnedDef, snapTool("read_file")})) {
		t.Fatalf("pinned defs %v", got)
	}
	names := AgentToolNames(&snapAgent{tools: []map[string]any{snapTool("b"), {"type": "function"}, snapTool("a")}})
	if !reflect.DeepEqual(names, []string{"b", "a"}) {
		t.Fatalf("agent tool names %v", names)
	}
}
