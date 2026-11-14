package app

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Gateway double running every automation call against the in process managers of the case's home
type automationGateway struct {
	mu    sync.Mutex
	calls [][2]string
}

func (g *automationGateway) handle(method string, params map[string]any) (any, error) {
	text, err := pyjson.Dumps(params)
	if err != nil {
		return nil, err
	}
	wire, err := pyjson.LoadsDict(text)
	if err != nil {
		return nil, err
	}
	kind, call := wire.GetOr("kind", "").(string), wire.GetOr("call", "").(string)
	g.mu.Lock()
	g.calls = append(g.calls, [2]string{kind, call})
	g.mu.Unlock()
	args, _ := wire.GetOr("args", nil).([]any)
	kwargs, _ := wire.GetOr("kwargs", nil).(*pyjson.Object)
	reply, err := agent.InvokeViewerAutomation(context.Background(), nil, kind, wire.GetOr("session_id", "").(string), call, args, kwargs)
	if err != nil {
		return nil, err
	}
	answer, err := pyjson.Dumps(reply)
	if err != nil {
		return nil, err
	}
	return pyjson.LoadsDict(answer)
}

func (g *automationGateway) called() [][2]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][2]string(nil), g.calls...)
}

func (g *automationGateway) asked(kind, call string) bool {
	for _, c := range g.called() {
		if c == [2]string{kind, call} {
			return true
		}
	}
	return false
}

// CLI on a gateway session store whose gateway is the double, its session open as sid-1
func automationCLI(t *testing.T) (*CLI, *automationGateway, *gatewaycli.GatewayConnection, string) {
	t.Helper()
	h := goalHooksHome(t)
	gw := &automationGateway{}
	connection, _ := fakeGatewayConnection(t, gw.handle)
	store := gatewaycli.NewGatewaySessionStore(connection)
	t.Cleanup(func() { _ = store.CloseReader() })
	c := bareCLI()
	c.pendingInput = &inputQueue{}
	c.sessionDB, c.gatewaySessionOpen, c.sessionID = store, true, "sid-1"
	return c, gw, connection, h
}

func TestGoalManagerIsTheGateways(t *testing.T) {
	c, gw, _, _ := automationCLI(t)
	mgr := c.getGoalManager()
	if _, isRemote := mgr.(*GatewayGoalManager); !isRemote {
		t.Fatalf("manager %T", mgr)
	}
	if got := gw.called(); !reflect.DeepEqual(got, [][2]string{{"goal", "state"}}) {
		t.Fatalf("calls %v", got)
	}
	state, err := mgr.Set("ship it", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.Goal != "ship it" || !mgr.IsActive() {
		t.Fatalf("state %+v active %v", state, mgr.IsActive())
	}
	if got := mgr.StatusLine(); got != "⊙ Goal (active, 0/20 turns): ship it" {
		t.Fatalf("status line %q", got)
	}
	if stored := goals.LoadGoal(context.Background(), "sid-1"); stored == nil || stored.Goal != "ship it" {
		t.Fatalf("stored %+v", stored)
	}
	_, err = mgr.RemoveSubgoal(4)
	refusal, isPy := err.(*pyint.Error)
	if !isPy || refusal.Kind != "IndexError" || !strings.Contains(refusal.Msg, "index out of range") {
		t.Fatalf("refusal %v", err)
	}
	if again := c.getGoalManager(); again != mgr {
		t.Fatal("manager rebuilt for the same session id")
	}
}

func TestNoManagerBeforeTheGatewaySessionIsOpen(t *testing.T) {
	c, gw, _, _ := automationCLI(t)
	c.gatewaySessionOpen = false
	if mgr := c.getGoalManager(); mgr != nil {
		t.Fatalf("manager %T", mgr)
	}
	if got := gw.called(); len(got) != 0 {
		t.Fatalf("calls %v", got)
	}
}

func TestJudgeDecisionComesFromTheGateway(t *testing.T) {
	c, gw, _, h := automationCLI(t)
	fakeGoalJudge(t, h, `{"done": true, "reason": "the report is written"}`)
	mgr := c.getGoalManager()
	if _, err := mgr.Set("write the report", 0, nil); err != nil {
		t.Fatal(err)
	}
	decision, err := mgr.EvaluateAfterTurn("the report is written", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Message != "✓ Goal achieved: the report is written" {
		t.Fatalf("message %q", decision.Message)
	}
	if !gw.asked("goal", "evaluate_after_turn") {
		t.Fatalf("calls %v", gw.called())
	}
	if mgr.State().Status != "done" {
		t.Fatalf("status %v", mgr.State().Status)
	}
}

func TestIdlePollsCostNoRoundTrip(t *testing.T) {
	c, gw, _, _ := automationCLI(t)
	hb, loop, goal := c.getHeartbeatManager(), c.getLoopManager(), c.getGoalManager()
	before := len(gw.called())
	if prompt, due := hb.DuePrompt(nil); due || prompt != "" || loop.IsDue() || goal.IsWaiting() {
		t.Fatal("an idle manager reported work")
	}
	if got := len(gw.called()); got != before {
		t.Fatalf("idle polls made %d calls", got-before)
	}
	if _, err := hb.Set("pulse", 60); err != nil {
		t.Fatal(err)
	}
	if line, err := hb.StatusLine(); err != nil || !hb.IsActive() || !strings.HasPrefix(line, "♥ Heartbeat (every 1m") {
		t.Fatalf("heartbeat %q %v", line, err)
	}
	interval := int64(60)
	if _, err := loop.Set(c.ctx, "check", &interval, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	if !loop.IsDue() {
		t.Fatal("loop not due")
	}
	if wakeup, fired := loop.FireTick(c.ctx); !fired || !strings.HasPrefix(wakeup, "[/loop wakeup #1") {
		t.Fatalf("wakeup %q", wakeup)
	}
	if got := loop.CompleteTick(c.ctx, "done").Message; got != "✓ Loop finished — ran 1/1 times." {
		t.Fatalf("message %q", got)
	}
}

func TestTheWatchdogFiresAHeartbeatAnotherViewerResumed(t *testing.T) {
	c, _, connection, _ := automationCLI(t)
	hb := c.getHeartbeatManager()
	if _, err := hb.Set("pulse", 60); err != nil {
		t.Fatal(err)
	}
	hb.Pause()
	other, err := NewGatewayHeartbeatManager(c.ctx, "sid-1", connection)
	if err != nil {
		t.Fatal(err)
	}
	other.Resume()
	state := agent.LoadHeartbeat(c.ctx, "sid-1")
	state.LastFiredAt -= 120
	agent.SaveHeartbeat(c.ctx, "sid-1", state)

	saved := heartbeatWatchdogSleep
	t.Cleanup(func() { heartbeatWatchdogSleep = saved })
	heartbeatWatchdogSleep = func(time.Duration) { c.shouldExit = true }
	c.startHeartbeatWatchdog()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.uiMu.Lock()
		started := c.heartbeatWatchdogStarted
		c.uiMu.Unlock()
		if !started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watchdog did not exit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	found := false
	for _, item := range c.pendingInput.snapshot() {
		if text, isText := item.(string); isText && strings.Contains(text, "pulse") {
			found = true
		}
	}
	if !found {
		t.Fatalf("queued %v", c.pendingInput.snapshot())
	}
}

func TestGoalDraftDraftsOnTheGateway(t *testing.T) {
	c, gw, _, h := automationCLI(t)
	var mu sync.Mutex
	var drafted []string
	srv, err := fakellm.New(nil, fakellm.Options{Aux: func(r *fakellm.Request) any {
		messages, _ := r.Body["messages"].([]any)
		for _, raw := range messages {
			if m, _ := raw.(map[string]any); m["role"] == "user" {
				mu.Lock()
				drafted = append(drafted, strings.TrimPrefix(m["content"].(string), "Objective:\n"))
				mu.Unlock()
			}
		}
		return fakellm.Text{Text: "no contract"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	if err := fakellm.WriteHome(h, srv.BaseURL(), "", "", 0); err != nil {
		t.Fatal(err)
	}
	mgr := c.getGoalManager()
	var lines []string
	result, err := dispatchGoalCommand(mgr, "draft ship the API", goals.GoalCommandOptions{AuthorizeGate: func() string { return "" },
		Progress: func(text string) { lines = append(lines, text) }})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), drafted...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"ship the API"}) || !gw.asked("goal", "command") {
		t.Fatalf("drafted %v calls %v", got, gw.called())
	}
	if !reflect.DeepEqual(lines, []string{"Drafting completion contract…"}) {
		t.Fatalf("progress %v", lines)
	}
	if !strings.HasPrefix(result.Output, "⊙ Goal set (20-turn budget): ship the API") || !result.Kickoff {
		t.Fatalf("result %+v", result)
	}
	if mgr.State().Goal != "ship the API" {
		t.Fatalf("goal %v", mgr.State().Goal)
	}
}

func TestHeartbeatRefusalFromTheGatewayIsTheManagersValueError(t *testing.T) {
	c, _, _, _ := automationCLI(t)
	hb := c.getHeartbeatManager()
	_, err := hb.Set("pulse", 1)
	if !agent.IsHeartbeatValueError(err) || !strings.HasPrefix(err.Error(), "interval must be at least") {
		t.Fatalf("refusal %v", err)
	}
	if hb.HasHeartbeat() {
		t.Fatal("a refused set left a heartbeat")
	}
}
