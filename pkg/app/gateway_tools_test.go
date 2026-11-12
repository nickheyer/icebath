package app

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

const gatewayToolsSID = "local-abc"

// Scripted gateway of the process cases with two processes, one running and one exited
type gatewayToolsGateway struct {
	mu        sync.Mutex
	calls     [][2]any
	processes []map[string]any
	stopGate  chan struct{}
}

func (g *gatewayToolsGateway) handle(method string, params map[string]any) (any, error) {
	g.mu.Lock()
	gate := g.stopGate
	g.mu.Unlock()
	if method == "process.stop" && gate != nil {
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, [2]any{method, params})
	switch method {
	case "tools.show":
		return map[string]any{"sections": []any{
			map[string]any{"name": "mcp-demo", "tools": []any{map[string]any{"name": "mcp_demo_ping", "description": "Ping."}}},
			map[string]any{"name": "terminal", "tools": []any{map[string]any{"name": "terminal", "description": "Run."}}}}, "total": 2}, nil
	case "skills.reload":
		return map[string]any{"output": "", "result": map[string]any{"added": []any{map[string]any{"name": "zeta-skill", "description": "Zeta."}},
			"removed": []any{}, "total": 3}}, nil
	case "session.resume":
		return map[string]any{"session_id": gatewayToolsSID, "stored_session_id": gatewayToolsSID, "revision": 1, "execution_generation": 0,
			"messages": []any{}, "subscription_id": "sub-" + gatewayToolsSID, "info": map[string]any{"model": "fake-model"}}, nil
	case "process.list":
		rows := make([]any, len(g.processes))
		for i, row := range g.processes {
			rows[i] = copyMap(row)
		}
		return map[string]any{"processes": rows}, nil
	case "process.stop":
		for _, row := range g.processes {
			if row["status"] == "running" {
				row["status"], row["exited_at"], row["completion_reason"], row["exit_code"] = "exited", nowSeconds(), "killed", -15
			}
		}
		return map[string]any{"killed": 1, "delegations": 0}, nil
	case "process.kill":
		return map[string]any{"status": "killed", "session_id": params["process_id"]}, nil
	}
	return pyjson.NewObject(), nil
}

// Whether the gateway was asked method with exactly the session id
func (g *gatewayToolsGateway) askedForSession(method string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.ContainsFunc(g.calls, func(call [2]any) bool {
		params, _ := call[1].(map[string]any)
		return call[0] == method && len(params) == 1 && params["session_id"] == gatewayToolsSID
	})
}

func (g *gatewayToolsGateway) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// Gateway agent on the scripted gateway with the process view bound to it
func gatewayToolsFixture(t *testing.T) (*GatewayAgent, *gatewayToolsGateway) {
	t.Helper()
	now := nowSeconds()
	gw := &gatewayToolsGateway{processes: []map[string]any{
		{"session_id": "proc_1", "command": "sleep 300", "status": "running", "exit_code": nil, "started_epoch": now - 5, "exited_at": nil,
			"completion_reason": nil, "output_tail": "tick 1\ntick 2\n"},
		{"session_id": "proc_2", "command": "echo hi", "status": "exited", "exit_code": int64(0), "started_epoch": now - 9, "exited_at": now - 3,
			"completion_reason": "exited", "output_tail": "hi\n"},
	}}
	connection, _ := fakeGatewayConnection(t, gw.handle)
	if err := connection.Start(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	connection.State(gatewayToolsSID)
	a, err := newGatewayAgent(context.Background(), gatewayAgentOptions{Connection: connection, SessionID: gatewayToolsSID, ToolProgressMode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	bindGatewayAgent(a)
	t.Cleanup(func() { bindGatewayView(nil, nil) })
	return a, gw
}

func TestDockRowsComeFromTheGatewaySession(t *testing.T) {
	recordPrints(t)
	_, gw := gatewayToolsFixture(t)
	c := bareCLI()
	rows := processRows(c.ctx, c.processRegistry(), nowSeconds())
	if len(rows) != 2 || rows[0]["id"] != "proc_1" || rows[0]["status"] != "running" || rows[1]["id"] != "proc_2" || rows[1]["status"] != "done" {
		t.Fatalf("rows %v", rows)
	}
	if rows[0]["command"] != "sleep 300" || rows[0]["detail"] != "tick 2" || rows[0]["elapsed"].(int) < 4 {
		t.Fatalf("first row %v", rows[0])
	}
	if rows[1]["exit_code"] != 0 || rows[1]["since_exit"].(int) < 2 {
		t.Fatalf("second row %v", rows[1])
	}
	if !gw.askedForSession("process.list") {
		t.Fatalf("calls %v", gw.calls)
	}
	if tail := processTail(c.ctx, c.processRegistry(), "proc_1"); tail != "tick 1\ntick 2\n" {
		t.Fatalf("tail %q", tail)
	}
}

func TestStatusBarCountReadsTheCachedGatewayCount(t *testing.T) {
	_, gw := gatewayToolsFixture(t)
	c := bareCLI()
	c.processRegistry().listSessions(c.ctx)
	polls := gw.callCount()
	if n := c.processRegistry().countRunning(); n != 1 {
		t.Fatalf("running %d", n)
	}
	if gw.callCount() != polls {
		t.Fatal("the render thread waited on an RPC")
	}
}

func TestStopKillsTheSessionProcessesThroughTheGateway(t *testing.T) {
	rec := recordPrints(t)
	_, gw := gatewayToolsFixture(t)
	c := bareCLI()
	c.handleStopCommand()
	out := strings.Join(rec.plain(), "\n")
	if !strings.Contains(out, "Stopping 1 background process(es)...") || !strings.Contains(out, "Stopped 1 process(es).") {
		t.Fatalf("output %q", out)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if !slices.ContainsFunc(gw.calls, func(call [2]any) bool { return call[0] == "process.stop" }) {
		t.Fatalf("calls %v", gw.calls)
	}
}

func TestAgentsListsTheGatewaySessionsProcessesAndSubagents(t *testing.T) {
	rec := recordPrints(t)
	a, gw := gatewayToolsFixture(t)
	state := a.connection.State(gatewayToolsSID)
	state.Lock()
	state.Usage = map[string]any{"active_subagents": int64(2)}
	state.Unlock()
	c := bareCLI()
	c.handleAgentsCommand()
	out := strings.Join(rec.plain(), "\n")
	if !strings.Contains(out, "proc_1") || !strings.Contains(out, "sleep 300") || !strings.Contains(out, "Background delegations: 2") {
		t.Fatalf("output %q", out)
	}
	if !gw.askedForSession("process.list") {
		t.Fatalf("calls %v", gw.calls)
	}
}

func TestAgentsBeforeTheFirstTurnReadsTheOpenedSessionsProcesses(t *testing.T) {
	rec := recordPrints(t)
	a, gw := gatewayToolsFixture(t)
	bindGatewayView(nil, nil)
	previousToolsets := tools.GatewayPluginToolsets
	t.Cleanup(func() { tools.GatewayPluginToolsets = previousToolsets })
	c := bareCLI()
	c.sessionID, c.resumed, c.enabledToolsets = gatewayToolsSID, true, []string{"terminal"}
	c.sessionDB = gatewaycli.NewGatewaySessionStore(a.connection)
	t.Cleanup(func() { _ = c.sessionDB.CloseReader() })
	if err := c.ensureGatewaySession(); err != nil {
		t.Fatal(err)
	}
	c.handleAgentsCommand()
	out := strings.Join(rec.plain(), "\n")
	if !strings.Contains(out, "proc_1") || !strings.Contains(out, "sleep 300") {
		t.Fatalf("output %q", out)
	}
	if !gw.askedForSession("process.list") {
		t.Fatalf("calls %v", gw.calls)
	}
}

func TestStopTypedMidTurnInterruptsTheTurnThenStopsProcesses(t *testing.T) {
	rec := recordPrints(t)
	a, gw := gatewayToolsFixture(t)
	if gatewayProcessView() == nil {
		t.Fatal("no process view bound")
	}
	release := make(chan struct{})
	gw.mu.Lock()
	gw.stopGate = release
	gw.mu.Unlock()
	var stops []*GatewayAgent
	prev := requestHardInterrupt
	requestHardInterrupt = func(a *GatewayAgent) { stops = append(stops, a) }
	t.Cleanup(func() { requestHardInterrupt = prev })
	c := bareCLI()
	c.agentRunning, c.agent = true, a
	_, event := handlerRig()
	if !c.tuiEnterInlineCommand(event, "/stop", false) {
		t.Fatal("/stop was not dispatched inline")
	}
	stopped := func() bool {
		gw.mu.Lock()
		defer gw.mu.Unlock()
		return slices.ContainsFunc(gw.calls, func(call [2]any) bool { return call[0] == "process.stop" })
	}
	if len(stops) != 1 || stops[0] != a || stopped() {
		t.Fatalf("stops %v stopped %v", stops, stopped())
	}
	close(release)
	waitUntil(t, func() bool {
		return stopped() && strings.Contains(strings.Join(rec.plain(), "\n"), "Stopped 1 process(es).")
	})
	if out := strings.Join(rec.plain(), "\n"); !strings.Contains(out, "Interrupting agent") {
		t.Fatalf("output %q", out)
	}
}

func TestDockStopKillsOneProcessThroughTheGateway(t *testing.T) {
	recordPrints(t)
	_, gw := gatewayToolsFixture(t)
	c := bareCLI()
	processRows(c.ctx, c.processRegistry(), nowSeconds())
	if got := killDockProcess(c.ctx, c.processRegistry(), "proc_1"); got["status"] != "killed" {
		t.Fatalf("kill answered %v", got)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if !slices.ContainsFunc(gw.calls, func(call [2]any) bool {
		params, _ := call[1].(map[string]any)
		return call[0] == "process.kill" && len(params) == 2 && params["session_id"] == gatewayToolsSID && params["process_id"] == "proc_1"
	}) {
		t.Fatalf("calls %v", gw.calls)
	}
}

func TestToolsListsTheGatewaySessionCatalogWithMainRendering(t *testing.T) {
	rec := recordPrints(t)
	_, gw := gatewayToolsFixture(t)
	bareCLI().showTools()
	out := strings.Join(rec.plain(), "\n") + "\n"
	for _, want := range []string{"  [mcp-demo]\n    * mcp_demo_ping        - Ping.\n", "  [terminal]\n    * terminal             - Run.\n", "Total: 2 tools"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if !gw.askedForSession("tools.show") {
		t.Fatalf("calls %v", gw.calls)
	}
}

func TestReloadSkillsRescansInTheGatewaySessionAndPrintsMainLines(t *testing.T) {
	rec := recordPrints(t)
	_, gw := gatewayToolsFixture(t)
	c := bareCLI()
	c.reloadSkills()
	out := strings.Join(rec.plain(), "\n")
	if !strings.Contains(out, "zeta-skill") || !strings.Contains(out, "3 skill(s) available") {
		t.Fatalf("output %q", out)
	}
	if !gw.askedForSession("skills.reload") {
		t.Fatalf("calls %v", gw.calls)
	}
	if !strings.Contains(c.pendingSkillsReloadNote, "zeta-skill") {
		t.Fatalf("note %q", c.pendingSkillsReloadNote)
	}
}
