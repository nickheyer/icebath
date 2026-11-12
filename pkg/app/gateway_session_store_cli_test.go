package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session info every scripted snapshot of the store cases carries
func storeCaseInfo() map[string]any {
	return map[string]any{"model": "fake-model", "cwd": "/w", "context_length": 128000, "enabled_toolsets": []any{"file"},
		"tools": map[string]any{"file": []any{"read_file", "write_file"}}, "skills": map[string]any{"devops": []any{"sdlc-review"}},
		"mcp_servers": []any{}, "tool_availability": map[string]any{"available_toolsets": []any{"file"}, "unavailable_toolsets": []any{},
			"lazy_tools": []any{}, "disabled_tools": []any{}}}
}

// Snapshot of a known session at the given revision
func storeCaseSnapshot(sid string) map[string]any {
	return map[string]any{"session_id": sid, "stored_session_id": sid, "messages": []any{}, "revision": 3,
		"execution_generation": 0, "subscription_id": "sub-" + sid, "info": storeCaseInfo()}
}

// Scripted gateway of the store cases, creating local-new and resuming only the sessions it knows
type storeCaseGateway struct {
	mu       sync.Mutex
	known    map[string]bool
	calls    [][2]any
	override func(method string, params map[string]any) (any, bool, error)
}

func (g *storeCaseGateway) answer(method string, raw map[string]any) (any, error) {
	params, _ := pyjson.Plain(raw).(map[string]any)
	g.mu.Lock()
	override := g.override
	g.mu.Unlock()
	if override != nil {
		if answer, handled, err := override(method, params); handled {
			return answer, err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, [2]any{method, params})
	switch method {
	case "session.create":
		g.known["local-new"] = true
		return storeCaseSnapshot("local-new"), nil
	case "session.resume":
		sid, _ := params["session_id"].(string)
		if !g.known[sid] {
			return nil, &gateway.GatewayClientError{Msg: "not_found"}
		}
		return storeCaseSnapshot(sid), nil
	}
	return map[string]any{"status": "ok", "detached": true}, nil
}

// Params of every recorded call of a method in order
func (g *storeCaseGateway) callsOf(method string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for _, call := range g.calls {
		if call[0] == method {
			out = append(out, call[1].(map[string]any))
		}
	}
	return out
}

// Gateway session store over the scripted gateway, local-old already known to it
func storeCase(t *testing.T) (*gatewaycli.GatewaySessionStore, *storeCaseGateway) {
	t.Helper()
	g := &storeCaseGateway{known: map[string]bool{"local-old": true}}
	connection, _ := fakeGatewayConnection(t, func(method string, params map[string]any) (any, error) {
		answer, err := g.answer(method, params)
		if err != nil {
			return nil, err
		}
		return wireValue(t, answer), nil
	})
	if err := connection.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return gatewaycli.NewGatewaySessionStore(connection), g
}

func TestNewSessionIdIsAFreshGatewaySession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SESSION_SOURCE", "")
	os.Unsetenv("ICEBATH_SESSION_SOURCE")
	db, g := storeCase(t)
	c := bareCLI()
	c.sessionID, c.resumed, c.sessionDB, c.model, c.explicitModelOverride = "local-old", true, db, "m-explicit", true
	c.enabledToolsets, c.explicitToolsets, c.gatewaySessionOpen = []string{"file"}, true, true
	if _, err := db.OpenSession(c.ctx, gatewaycli.OpenSessionCLI{SessionID: "local-old", Resumed: true}, nil); err != nil {
		t.Fatal(err)
	}
	sid, err := c.newSessionID(time.Now(), "", "")
	if err != nil || sid != "local-new" || c.sessionID != "local-new" {
		t.Fatalf("sid %q session %q err %v", sid, c.sessionID, err)
	}
	creates := g.callsOf("session.create")
	params := creates[len(creates)-1]
	if _, present := params["model"]; present || !reflect.DeepEqual(params["toolsets"], []any{"file"}) {
		t.Fatalf("session.create %v", params)
	}
	c.explicitToolsets = false
	if _, err := c.newSessionID(time.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	creates = g.callsOf("session.create")
	if toolsets := creates[len(creates)-1]["toolsets"]; toolsets != nil {
		t.Fatalf("toolsets %v", toolsets)
	}
}

func TestBranchSessionIdIsTheGatewayBranchChild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, g := storeCase(t)
	g.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.mutate" || params["operation"] != "branch" {
			return nil, false, nil
		}
		g.mu.Lock()
		g.calls = append(g.calls, [2]any{method, params})
		g.known["local-child"] = true
		g.mu.Unlock()
		return map[string]any{"branched_session_id": "local-child", "copied_messages": 2}, true, nil
	}
	if _, err := db.OpenSession(context.Background(), gatewaycli.OpenSessionCLI{SessionID: "local-old", Resumed: true}, nil); err != nil {
		t.Fatal(err)
	}
	c := bareCLI()
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-old", db, true
	child, err := c.newSessionID(time.Now(), "local-old", "fork")
	if err != nil || child != "local-child" {
		t.Fatalf("child %q err %v", child, err)
	}
	mutates := g.callsOf("session.mutate")
	mutate := mutates[len(mutates)-1]
	if _, present := mutate["expected_generation"]; !present || !reflect.DeepEqual(mutate["payload"], map[string]any{"title": "fork"}) {
		t.Fatalf("mutate %v", mutate)
	}
	resumes := len(g.callsOf("session.resume"))
	if _, err := db.Attach(context.Background(), "local-child", false); err != nil || len(g.callsOf("session.resume")) != resumes {
		t.Fatalf("local-child is not among the store's snapshots: %v", err)
	}
}

func TestNewSessionBeforeTheGatewaySessionOpenedIsMainsPlaceholder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, g := storeCase(t)
	c := bareCLI()
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "x", db, false
	sid, err := c.newSessionID(time.Date(2026, 10, 12, 1, 2, 3, 0, time.Local), "", "")
	if err != nil || !strings.HasPrefix(sid, "20261012_010203_") {
		t.Fatalf("sid %q err %v", sid, err)
	}
	if creates := g.callsOf("session.create"); len(creates) != 0 {
		t.Fatalf("session.create sent %v", creates)
	}
}

func TestCliStartupNoticesRenderFromTheSnapshot(t *testing.T) {
	db, _ := storeCase(t)
	if _, err := db.OpenSession(context.Background(), gatewaycli.OpenSessionCLI{SessionID: "local-old", Resumed: true}, nil); err != nil {
		t.Fatal(err)
	}
	info, _ := db.Current().Get("info")
	info.(*pyjson.Object).Set("tool_availability_warnings", []any{"[yellow]⚠ terminal off[/]"})
	c, rec := infoCLI(t)
	c.sessionDB, c.sessionID, c.compact, c.enabledToolsets = db, "local-old", true, []string{"x_search"}
	t.Setenv("XAI_API_KEY", "")
	c.env = &tools.Env{Home: os.Getenv("ICEBATH_HOME")}
	c.showBanner()
	printed := rec.plain()
	if !slices.Contains(printed, "⚠ terminal off") {
		t.Fatalf("printed %q", printed)
	}
	rec.reset()
	c.showToolAvailabilityWarnings()
	local := slices.DeleteFunc(rec.plain(), func(line string) bool { return line == "" })
	if len(local) == 0 {
		t.Fatal("the local warning path prints nothing for this home")
	}
	for _, line := range local {
		if slices.Contains(printed, line) {
			t.Fatalf("the banner enumerated local tools %q", line)
		}
	}
}

// Params of the newest session.create the scripted gateway saw
func lastCreate(t *testing.T, g *storeCaseGateway) map[string]any {
	t.Helper()
	creates := g.callsOf("session.create")
	if len(creates) == 0 {
		t.Fatal("no session.create reached the gateway")
	}
	return creates[len(creates)-1]
}

// Unsets an environment variable for the case, restoring it afterwards
func unsetCaseEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestRowCwdIsMainsStampWhileTheToolsRunInTerminalCwd(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	db, g := storeCase(t)
	gatewaycli.SharedGatewayConnection().Close(ctx)
	previous := gatewaycli.DefaultConnect
	gatewaycli.DefaultConnect = func(context.Context) (gatewaycli.ConnectionClient, error) {
		return &fakeGatewayClient{handler: func(method string, params map[string]any) (any, error) {
			answer, err := g.answer(method, params)
			if err != nil {
				return nil, err
			}
			return wireValue(t, answer), nil
		}, events: gateway.NewEventQueue(0)}, nil
	}
	t.Cleanup(func() {
		gatewaycli.SharedGatewayConnection().Close(context.Background())
		gatewaycli.DefaultConnect = previous
	})
	root := t.TempDir()
	repo, tree := filepath.Join(root, "repo"), filepath.Join(root, "repo", ".worktrees", "wt")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	t.Setenv("TERMINAL_CWD", tree)
	unsetCaseEnv(t, "TERMINAL_ENV")
	unsetCaseEnv(t, "ICEBATH_SESSION_SOURCE")
	if _, err := db.OpenSession(ctx, gatewaycli.OpenSessionCLI{SessionID: "20261012_000000_abcdef"}, map[string]any{"toolsets": []any{"file"}}); err != nil {
		t.Fatal(err)
	}
	resolvedTree, _ := filepath.EvalSymlinks(tree)
	resolvedRepo, _ := filepath.EvalSymlinks(repo)
	if first := lastCreate(t, g); first["cwd"] != resolvedTree || first["row_cwd"] != resolvedRepo {
		t.Fatalf("first create %v", first)
	}
	c := bareCLI()
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-new", db, true
	if _, err := c.newSessionID(time.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	if row := lastCreate(t, g)["row_cwd"]; row != "" {
		t.Fatalf("/new stamped %v", row)
	}
	if _, err := gatewaycli.CreateTitledSession(ctx, "Fresh Named"); err != nil {
		t.Fatal(err)
	}
	if row := lastCreate(t, g)["row_cwd"]; row != "" {
		t.Fatalf("a titled create stamped %v", row)
	}
	t.Setenv("TERMINAL_ENV", "docker")
	if _, err := db.Create(ctx, map[string]any{"toolsets": []any{"file"}}, "", false); err != nil {
		t.Fatal(err)
	}
	if row := lastCreate(t, g)["row_cwd"]; row != "" {
		t.Fatalf("a remote backend stamped %v", row)
	}
}

// Launch host of the init cases over a fresh store of the case's connection with the run's flags applied
func launchHost(db *gatewaycli.GatewaySessionStore, maxTurns *int, checkpoints bool, reasoning string) *CLI {
	c := bareCLI()
	c.model, c.requestedProvider, c.enabledToolsets = "m1", "auto", []string{"terminal"}
	c.sessionID, c.sessionDB = "20261012_000000_abcdef", gatewaycli.NewGatewaySessionStore(db.Connection)
	c.initTurnLimits(maxTurns, nil)
	c.initCheckpointsAndRules(checkpoints, false, false)
	c.initPromptAndReasoning(reasoning)
	return c
}

// Params of the newest call the scripted gateway saw
func lastCall(t *testing.T, g *storeCaseGateway) map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.calls) == 0 {
		t.Fatal("no call reached the gateway")
	}
	return g.calls[len(g.calls)-1][1].(map[string]any)
}

func TestExplicitReasoningMaxTurnsAndCheckpointsReachSessionCreate(t *testing.T) {
	caseHome(t)
	db, g := storeCase(t)
	keep := tools.GatewayPluginToolsets
	t.Cleanup(func() { tools.GatewayPluginToolsets = keep })
	one := 1
	if err := launchHost(db, &one, true, "high").ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	params := lastCall(t, g)
	if params["reasoning"] != "high" || fmt.Sprint(params["max_turns"]) != "1" || params["checkpoints"] != true {
		t.Fatalf("params %v", params)
	}
	if err := launchHost(db, nil, false, "").ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	params = lastCall(t, g)
	for _, key := range []string{"reasoning", "max_turns", "checkpoints"} {
		if _, sent := params[key]; sent {
			t.Fatalf("an unset flag reached the gateway %v", params)
		}
	}
	if err := launchHost(db, nil, false, "bogus").ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	if _, sent := lastCall(t, g)["reasoning"]; sent {
		t.Fatalf("an unparseable reasoning reached the gateway %v", lastCall(t, g))
	}
}

func TestResumeHandsTheCliLeaseOverWhileTheGatewayImportsTheRow(t *testing.T) {
	caseHome(t)
	db, g := storeCase(t)
	keep := tools.GatewayPluginToolsets
	t.Cleanup(func() { tools.GatewayPluginToolsets = keep })
	c := bareCLI()
	c.model, c.requestedProvider, c.enabledToolsets = "m1", "auto", []string{"terminal"}
	c.sessionID, c.resumed, c.sessionDB = "local-old", true, gatewaycli.NewGatewaySessionStore(db.Connection)
	if !c.claimActiveSession("cli", true) || c.activeSessionLease == nil {
		t.Fatal("the CLI did not claim its lease")
	}
	held := c.activeSessionLease
	t.Cleanup(c.releaseActiveSession)
	var events [][2]any
	g.override = func(method string, params map[string]any) (any, bool, error) {
		events = append(events, [2]any{method, c.activeSessionLease == nil})
		return nil, false, nil
	}
	if err := c.ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	if want := [][2]any{{"session.resume", true}}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v", events)
	}
	if c.activeSessionLease == nil || c.activeSessionLease == held {
		t.Fatalf("the lease was not claimed again for local-old %v", c.activeSessionLease)
	}
}

func TestSkillsFlagReachesSessionCreateWithoutAutoLoadNames(t *testing.T) {
	caseHome(t)
	db, g := storeCase(t)
	keep := tools.GatewayPluginToolsets
	t.Cleanup(func() { tools.GatewayPluginToolsets = keep })
	SetCLIConfig(map[string]any{"skills": map[string]any{"auto_load": []any{"pinned"}}})
	t.Cleanup(func() { SetCLIConfig(nil) })
	c := bareCLI()
	c.model, c.requestedProvider, c.enabledToolsets = "m1", "auto", []string{"file"}
	c.sessionID, c.sessionDB = "20261012_000000_abcdef", db
	c.preloadSkillsRequested = []string{"pinned", "arxiv"}
	if err := c.ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	if skills := lastCreate(t, g)["skills"]; !reflect.DeepEqual(skills, []any{"arxiv"}) {
		t.Fatalf("skills %v", skills)
	}
}

// Makes every resume answer carry the given frozen launch request
func (g *storeCaseGateway) freezeLaunch(launch map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.override = func(method string, params map[string]any) (any, bool, error) {
		if method != "session.resume" {
			return nil, false, nil
		}
		sid, _ := params["session_id"].(string)
		g.mu.Lock()
		g.calls = append(g.calls, [2]any{method, params})
		known := g.known[sid]
		g.mu.Unlock()
		if !known {
			return nil, true, &gateway.GatewayClientError{Msg: "not_found"}
		}
		snap := storeCaseSnapshot(sid)
		frozen := map[string]any{}
		for k, v := range launch {
			frozen[k] = v
		}
		snap["info"].(map[string]any)["launch_request"] = frozen
		return snap, true, nil
	}
}

// Sets the scripted gateway's override under its lock
func (g *storeCaseGateway) setOverride(fn func(method string, params map[string]any) (any, bool, error)) {
	g.mu.Lock()
	g.override = fn
	g.mu.Unlock()
}

// Fails unless err is the resume policy refusal
func wantResumeRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "Resume retains gateway session policy") {
		t.Fatalf("not refused: %v", err)
	}
}

func TestUnchangedModelFlagOnResumeIgnoresTheConfiguredProvider(t *testing.T) {
	caseHome(t)
	keep := tools.GatewayPluginToolsets
	t.Cleanup(func() { tools.GatewayPluginToolsets = keep })
	SetCLIConfig(map[string]any{"model": map[string]any{"default": "cfg-model", "provider": "openrouter"}})
	t.Cleanup(func() { SetCLIConfig(nil) })
	db, g := storeCase(t)
	g.freezeLaunch(map[string]any{"model": "my-model"})
	host := func(provider string, resumed bool) *CLI {
		c := bareCLI()
		c.sessionID, c.resumed, c.enabledToolsets = "local-old", resumed, []string{"file"}
		c.sessionDB, c.config = gatewaycli.NewGatewaySessionStore(db.Connection), CLIConfig(c.ctx)
		if err := c.initModelAndProvider("my-model", provider, "", ""); err != nil {
			t.Fatal(err)
		}
		return c
	}
	repeat := host("", true)
	if repeat.requestedProvider != "openrouter" {
		t.Fatalf("requested provider %q", repeat.requestedProvider)
	}
	if err := repeat.ensureGatewaySession(); err != nil || repeat.sessionID != "local-old" {
		t.Fatalf("repeat %v %q", err, repeat.sessionID)
	}
	wantResumeRefusal(t, host("anthropic", true).ensureGatewaySession())
	if err := host("anthropic", false).ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	if provider := lastCreate(t, g)["provider"]; provider != "anthropic" {
		t.Fatalf("provider %v", provider)
	}
	if err := host("", false).ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	created := lastCreate(t, g)
	if _, sent := created["provider"]; created["model"] != "my-model" || sent {
		t.Fatalf("created %v", created)
	}
}

func TestNewSessionKeepsTheRunsLaunchPolicy(t *testing.T) {
	caseHome(t)
	SetCLIConfig(map[string]any{})
	t.Cleanup(func() { SetCLIConfig(nil) })
	db, g := storeCase(t)
	if _, err := db.OpenSession(context.Background(), gatewaycli.OpenSessionCLI{SessionID: "local-old", Resumed: true}, nil); err != nil {
		t.Fatal(err)
	}
	c := bareCLI()
	three, budget := 3, 60.0
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-old", db, true
	c.model, c.explicitModelOverride, c.requestedProvider = "m-explicit", true, "openrouter"
	c.enabledToolsets, c.explicitToolsets, c.explicitMaxTurns, c.explicitCheckpoints = []string{"file"}, true, &three, true
	c.explicitRunBudget, c.verbose, c.preloadSkillsRequested = &budget, true, []string{"arxiv"}
	if _, err := c.newSessionID(time.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	params := lastCreate(t, g)
	got := []any{fmt.Sprint(params["max_turns"]), params["checkpoints"], fmt.Sprint(params["run_budget"]), params["verbose"], params["skills"], params["toolsets"]}
	if want := []any{"3", true, "60", true, []any{"arxiv"}, []any{"file"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("params %v", params)
	}
	_, model := params["model"]
	_, provider := params["provider"]
	if model || provider {
		t.Fatalf("the explicit route reached /new %v", params)
	}
}

// Scripted branch answers of the yolo cases, the child id from the counter when named is empty
func branchOverride(g *storeCaseGateway, named string, refuseConfig bool) func(string, map[string]any) (any, bool, error) {
	return func(method string, params map[string]any) (any, bool, error) {
		if method == "session.mutate" && params["operation"] == "branch" {
			g.mu.Lock()
			defer g.mu.Unlock()
			child := named
			if child == "" {
				g.calls = append(g.calls, [2]any{method, params})
				child = fmt.Sprintf("local-child-%d", len(g.calls))
			}
			g.known[child] = true
			return map[string]any{"branched_session_id": child, "copied_messages": 0}, true, nil
		}
		if refuseConfig && method == "config.set" {
			return nil, true, &gateway.GatewayClientError{Msg: "Gateway did not answer config.set"}
		}
		return nil, false, nil
	}
}

func TestBranchCarriesALiveYoloOntoTheGatewayChild(t *testing.T) {
	caseHome(t)
	db, g := storeCase(t)
	g.setOverride(branchOverride(g, "", false))
	if _, err := db.OpenSession(context.Background(), gatewaycli.OpenSessionCLI{SessionID: "local-old", Resumed: true}, nil); err != nil {
		t.Fatal(err)
	}
	c := bareCLI()
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-old", db, true
	plain, err := c.newSessionID(time.Now(), "local-old", "")
	if err != nil {
		t.Fatal(err)
	}
	if sets := g.callsOf("config.set"); len(sets) != 0 {
		t.Fatalf("a plain branch set %v", sets)
	}
	tools.EnableSessionYolo("local-old")
	child, err := c.newSessionID(time.Now(), "local-old", "")
	for _, key := range []string{"local-old", plain, child} {
		tools.DisableSessionYolo(key)
	}
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"session_id": child, "key": "yolo", "value": "1"}}
	if sets := g.callsOf("config.set"); !reflect.DeepEqual(sets, want) {
		t.Fatalf("config.set %v", sets)
	}
}

func TestBranchNeverShowsYoloTheGatewayRefused(t *testing.T) {
	caseHome(t)
	db, g := storeCase(t)
	g.setOverride(branchOverride(g, "local-child", true))
	if _, err := db.OpenSession(context.Background(), gatewaycli.OpenSessionCLI{SessionID: "local-old", Resumed: true}, nil); err != nil {
		t.Fatal(err)
	}
	c := bareCLI()
	c.sessionID, c.sessionDB, c.gatewaySessionOpen = "local-old", db, true
	tools.EnableSessionYolo("local-old")
	t.Cleanup(func() {
		tools.DisableSessionYolo("local-old")
		tools.DisableSessionYolo("local-child")
	})
	if _, err := c.newSessionID(time.Now(), "local-old", ""); err == nil || !strings.Contains(err.Error(), "config.set") {
		t.Fatalf("the refusal did not surface %v", err)
	}
	if tools.IsSessionYoloEnabled("local-child") {
		t.Fatal("the child shows a yolo the gateway refused")
	}
}

func TestExportedIgnoreRulesNeverRefusesABareResume(t *testing.T) {
	caseHome(t)
	t.Setenv("ICEBATH_IGNORE_RULES", "1")
	db, g := storeCase(t)
	g.freezeLaunch(map[string]any{"model": "m-frozen"})
	host := func(typed bool) *CLI {
		c := bareCLI()
		c.sessionID, c.resumed = "local-old", true
		c.initCheckpointsAndRules(false, false, typed)
		return c
	}
	bare := host(false)
	if !bare.ignoreRules || gatewayLaunch(bare)["ignore_rules"] != true {
		t.Fatalf("ignore rules %v launch %v", bare.ignoreRules, gatewayLaunch(bare))
	}
	sid, err := gatewaycli.NewGatewaySessionStore(db.Connection).OpenSession(context.Background(), bare.gatewayOpenRequest(), gatewayLaunch(bare))
	if err != nil || sid != "local-old" {
		t.Fatalf("bare resume %q %v", sid, err)
	}
	typed := host(true)
	_, err = gatewaycli.NewGatewaySessionStore(db.Connection).OpenSession(context.Background(), typed.gatewayOpenRequest(), gatewayLaunch(typed))
	wantResumeRefusal(t, err)
}

func TestClearBannerShowsTheNewGatewaySessionsToolsAndShortLabel(t *testing.T) {
	caseHome(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("approvals:\n  destructive_slash_confirm: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COLUMNS", "200")
	t.Setenv("LINES", "50")
	keep := tools.GatewayPluginToolsets
	t.Cleanup(func() { tools.GatewayPluginToolsets = keep })
	db, g := storeCase(t)
	canonical := "local-ab12cd34" + strings.Repeat("e", 56)
	created := storeCaseSnapshot(canonical)
	info := storeCaseInfo()
	info["tools"] = map[string]any{"file": []any{"read_file"}, "mcp-zeta": []any{"mcp__zeta__ro_probe"}}
	created["info"] = info
	g.setOverride(func(method string, params map[string]any) (any, bool, error) {
		g.mu.Lock()
		g.calls = append(g.calls, [2]any{method, params})
		g.mu.Unlock()
		if method == "session.create" {
			return created, true, nil
		}
		return map[string]any{}, true, nil
	})
	var banners []welcomeBanner
	swap(t, &welcomeBannerPrinter, func(_ context.Context, _ *pyrich.Console, kw welcomeBanner) error {
		banners = append(banners, kw)
		return nil
	})
	swap(t, &toolDefinitionsFor, func(*CLI, bool) []map[string]any {
		t.Error("walked the CLI's registry")
		return nil
	})
	captureStreams(t)
	c := bareCLI()
	c.sessionDB, c.sessionID, c.gatewaySessionOpen = db, "local-old", true
	c.compact, c.model, c.provider = false, "fake-model", "custom"
	c.enabledToolsets, c.disabledToolsets, c.app = []string{"icebath-cli"}, []string{}, &fakeTUIApp{}
	c.cmdClear("/clear")
	if len(banners) != 1 {
		t.Fatalf("%d banners", len(banners))
	}
	if banners[0].SessionID != "ab12cd34" {
		t.Fatalf("label %q", banners[0].SessionID)
	}
	var names []string
	listed, _ := pyjson.Plain(banners[0].Tools).([]any)
	for _, tool := range listed {
		fn, _ := tool.(map[string]any)["function"].(map[string]any)
		names = append(names, strOrEmpty(fn["name"]))
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, []string{"mcp__zeta__ro_probe", "read_file"}) {
		t.Fatalf("tools %v", banners[0].Tools)
	}
}
