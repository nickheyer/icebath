package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeChild struct {
	mu                        sync.Mutex
	id                        string
	interrupted               bool
	kind, message, toolReason string
	steering                  []string
}

func (c *fakeChild) SubagentID() string           { return c.id }
func (c *fakeChild) Provider() *string            { return strp("test") }
func (c *fakeChild) Model() *string               { return strp("test-model") }
func (c *fakeChild) DelegateRole() (string, bool) { return "leaf", true }
func (c *fakeChild) DelegateDepth() int           { return 1 }
func (c *fakeChild) isInterrupted() bool          { c.mu.Lock(); defer c.mu.Unlock(); return c.interrupted }

type fakeParent struct {
	session string
	enabled []string
}

type fakeDelegation struct {
	mu       sync.Mutex
	counter  int
	built    []SubagentChildSpec
	children map[string]*fakeChild
	config   map[string]any
	resolved []map[string]any
	resolve  func(cfg map[string]any) (map[string]any, error)
	run      func(child *fakeChild) any
	known    map[string]bool
}

func (d *fakeDelegation) SessionID(parent any) string { return parent.(*fakeParent).session }
func (d *fakeDelegation) EnabledToolsets(parent any) ([]string, bool) {
	p := parent.(*fakeParent)
	return p.enabled, p.enabled != nil
}
func (d *fakeDelegation) ValidateToolset(_ context.Context, name string) bool { return d.known[name] }
func (d *fakeDelegation) LoadConfig(context.Context) map[string]any           { return d.config }
func (d *fakeDelegation) ResolveCredentials(_ context.Context, cfg map[string]any, _ any) (map[string]any, error) {
	d.mu.Lock()
	d.resolved = append(d.resolved, cfg)
	d.mu.Unlock()
	if d.resolve != nil {
		return d.resolve(cfg)
	}
	return map[string]any{"model": cfg["model"]}, nil
}
func (d *fakeDelegation) BuildChild(_ context.Context, spec SubagentChildSpec) (SubagentChild, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	child := &fakeChild{id: fmt.Sprintf("sa-%d", d.counter)}
	d.counter++
	d.built = append(d.built, spec)
	d.children[child.id] = child
	return child, nil
}
func (d *fakeDelegation) RunChild(_ context.Context, _ string, child SubagentChild, _ any) (any, error) {
	return d.run(child.(*fakeChild)), nil
}
func (d *fakeDelegation) HardInterrupt(child SubagentChild, reason, toolReason string) (bool, error) {
	c := child.(*fakeChild)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interrupted, c.kind, c.message, c.toolReason = true, "hard", reason, toolReason
	return true, nil
}
func (d *fakeDelegation) Steer(id, text string) bool {
	d.mu.Lock()
	c := d.children[id]
	d.mu.Unlock()
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steering = append(c.steering, text)
	return true
}

// Lifecycle fixture whose children finish after a short run unless interrupted
func lifecycleFixture(t *testing.T) (*SubagentLifecycleService, *fakeDelegation) {
	t.Helper()
	d := &fakeDelegation{children: map[string]*fakeChild{}, config: map[string]any{}, known: map[string]bool{"file": true},
		run: func(c *fakeChild) any {
			for i := 0; i < 20; i++ {
				if c.isInterrupted() {
					return map[string]any{"status": "interrupted", "summary": nil, "api_calls": 0, "duration_seconds": 0}
				}
				time.Sleep(2 * time.Millisecond)
			}
			return map[string]any{"status": "completed", "summary": "safe summary", "api_calls": 1, "duration_seconds": 0.01}
		}}
	previous := installedServices()
	InstallServices(&Services{SubagentLifecycle: d})
	t.Cleanup(func() { InstallServices(previous) })
	parent := &fakeParent{session: "parent-1", enabled: []string{"file"}}
	return NewSubagentLifecycleService(func(context.Context) any { return parent }), d
}

func one(f float64) *float64 { return &f }

func asLifecycleErr(err error, sub string) bool {
	var le *SubagentLifecycleError
	return errors.As(err, &le) && strings.Contains(le.Message, sub)
}

// test_subagent_lifecycle.py test_cancel_is_cooperative_and_forged_handle_is_unknown and test_cancel_uses_explicit_hard_interrupt
func TestSubagentLifecycleCancelAndForgedHandles(t *testing.T) {
	ctx := context.Background()
	s, d := lifecycleFixture(t)
	handle, err := s.Launch(ctx, &SubagentLaunchRequest{Goal: "x", Role: "leaf"})
	if err != nil {
		t.Fatal(err)
	}
	record := s.record(ctx, handle)
	if record == nil || record.agent == nil {
		t.Fatal("no live record")
	}
	if !s.Cancel(ctx, handle, "explicit user cancel").Accepted {
		t.Fatal("cancel refused")
	}
	child := d.children[handle.SubagentID]
	if child.kind != "hard" || !strings.Contains(child.message, "explicit user cancel") || child.toolReason != "subagent cancellation requested" {
		t.Fatalf("interrupt %+v", child)
	}
	if terminal := s.Wait(ctx, handle, one(1)); terminal.State != SubagentCancelled || !terminal.Completed {
		t.Fatalf("terminal %+v", terminal)
	}
	if again := s.Cancel(ctx, handle, "again"); again.Accepted || !again.AlreadyTerminal || again.State != SubagentCancelled {
		t.Fatalf("already terminal %+v", again)
	}
	forged := handle.ToDict()
	forged["capability"] = "forged"
	if s.Status(ctx, forged).State != SubagentUnknown || *s.Result(ctx, forged).ErrorClassification != "UNKNOWN_HANDLE" || !s.Cancel(ctx, forged, "x").UnknownHandle {
		t.Fatal("forged handle accepted")
	}
	other := NewSubagentLifecycleService(func(context.Context) any { return &fakeParent{session: "different-parent"} })
	if other.Status(ctx, handle).State != SubagentUnknown || other.Reconnect(ctx, handle).Diagnostic == nil {
		t.Fatal("foreign parent reached the record")
	}
	result := s.Result(ctx, handle)
	if !result.Ready || result.TerminalState != SubagentCancelled || *result.ErrorClassification != "INTERRUPTED" || result.ResultHash == nil || len(*result.ResultHash) != 64 {
		t.Fatalf("result %+v", result)
	}
}

// test_subagent_lifecycle.py test_steer_queues_to_running_child_and_rejects_invalid_calls for both the direct and wire forms
func TestSubagentLifecycleSteer(t *testing.T) {
	for _, wire := range []bool{false, true} {
		ctx := context.Background()
		s, d := lifecycleFixture(t)
		started, finish := make(chan struct{}), make(chan struct{})
		d.run = func(*fakeChild) any {
			close(started)
			<-finish
			return map[string]any{"status": "completed", "summary": "done"}
		}
		var request any = &SubagentLaunchRequest{Goal: "review", Role: "leaf", AllowedToolsets: []string{"file"}}
		if wire {
			request = Record{"goal": "review", "context": nil, "role": "leaf", "model": nil, "provider": nil, "allowed_toolsets": []any{"file"},
				"blocked_tools": []any{}, "working_directory": nil, "parent_session_id": nil, "correlation_id": nil, "metadata": map[string]any{}, "timeout_seconds": nil}
		}
		h, err := s.Launch(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		<-started
		var handle any = h
		if wire {
			handle = Record(h.ToDict())
		}
		if s.Status(ctx, handle).State != SubagentRunning || !s.Reconnect(ctx, handle).Connected {
			t.Fatal("not running")
		}
		if !s.Steer(ctx, handle, "focus on correctness") || s.Steer(ctx, handle, "") || s.Steer(ctx, handle, "   ") || s.Steer(ctx, handle, 5) {
			t.Fatal("steer validation")
		}
		other := NewSubagentLifecycleService(func(context.Context) any { return &fakeParent{session: "foreign"} })
		forged := h.ToDict()
		forged["capability"] = "forged"
		if other.Steer(ctx, handle, "foreign") || s.Steer(ctx, forged, "forged") {
			t.Fatal("foreign steer")
		}
		close(finish)
		if s.Wait(ctx, handle, one(2)).State != SubagentSucceeded || *s.Result(ctx, handle).Summary != "done" || s.Steer(ctx, handle, "too late") {
			t.Fatal("after finish")
		}
		if got := d.children[h.SubagentID].steering; len(got) != 1 || got[0] != "focus on correctness" {
			t.Fatalf("steering %v", got)
		}
	}
}

// test_subagent_lifecycle.py test_mapping_request_errors_are_explicit and the request rejection table
func TestSubagentLifecycleRequestErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := lifecycleFixture(t)
	for _, tc := range []struct {
		request any
		message string
	}{
		{map[string]any{"goal": "review", "future_field": true}, "future_field"},
		{42, "request must be a SubagentLaunchRequest"},
		{map[string]any{"role": "leaf"}, "missing 1 required positional argument: 'goal'"},
		{map[string]any{"goal": "  "}, "goal must be a non-empty string"},
		{map[string]any{"goal": 5}, "goal must be a non-empty string"},
		{map[string]any{"goal": "x", "role": "boss"}, "role must be 'leaf' or 'orchestrator'."},
		{map[string]any{"goal": "x", "provider": " "}, "provider must be a non-empty string"},
		{map[string]any{"goal": "x", "timeout_seconds": 3}, "Per-launch timeout is not supported"},
		{map[string]any{"goal": "x", "working_directory": "/tmp"}, "working_directory is not supported"},
		{map[string]any{"goal": "x", "blocked_tools": []any{"terminal"}}, "Per-tool blocking is not supported"},
		{map[string]any{"goal": "x", "metadata": map[string]any{"blob": strings.Repeat("a", 9000)}}, "metadata exceeds 8192 bytes."},
		{map[string]any{"goal": "x", "allowed_toolsets": []any{"nope", "zap"}}, "Unknown toolsets: nope, zap."},
		{map[string]any{"goal": "x", "parent_session_id": "other"}, "parent_session_id does not match the active session."},
	} {
		if _, err := s.Launch(ctx, tc.request); !asLifecycleErr(err, tc.message) {
			t.Fatalf("%v: %v", tc.request, err)
		}
	}
	if _, err := NewSubagentLifecycleService(func(context.Context) any { return nil }).Launch(ctx, map[string]any{"goal": "x"}); !asLifecycleErr(err, "No active Icebath parent session is available.") {
		t.Fatalf("no parent %v", err)
	}
	h, err := s.Launch(ctx, map[string]any{"goal": "x", "correlation_id": "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Launch(ctx, map[string]any{"goal": "x", "correlation_id": "c1"}); !asLifecycleErr(err, "Duplicate correlation_id") {
		t.Fatalf("duplicate %v", err)
	}
	s.Wait(ctx, h, nil)
	if _, err := SubagentHandleFromDict(map[string]any{"subagent_id": "x"}); !asLifecycleErr(err, "Malformed subagent handle.") {
		t.Fatalf("from_dict %v", err)
	}
}

// Plugin halves of the routing and plugin toolset cases of test_subagent_lifecycle.py
func TestSubagentLifecycleRoutingAndToolsets(t *testing.T) {
	ctx := context.Background()
	s, d := lifecycleFixture(t)
	d.config = map[string]any{"provider": "openrouter", "model": "delegated-model"}
	d.resolve = func(cfg map[string]any) (map[string]any, error) {
		return map[string]any{"model": cfg["model"], "provider": "openrouter"}, nil
	}
	h, _ := s.Launch(ctx, &SubagentLaunchRequest{Goal: "route", Role: "leaf"})
	if s.Wait(ctx, h, one(2)).State != SubagentSucceeded || d.built[0].Model != "delegated-model" || d.resolved[0]["provider"] != "openrouter" {
		t.Fatalf("config route %v %v", d.built, d.resolved)
	}
	h, _ = s.Launch(ctx, &SubagentLaunchRequest{Goal: "route", Role: "leaf", Model: strp("request-model")})
	s.Wait(ctx, h, one(2))
	if d.built[1].Model != "request-model" {
		t.Fatalf("request model %v", d.built[1])
	}
	h, _ = s.Launch(ctx, Record{"goal": "route", "provider": "openrouter", "model": "m"})
	s.Wait(ctx, h, one(2))
	if d.built[2].Model != "m" || d.built[2].RoutingCfg["provider"] != "openrouter" || d.built[2].RoutingCfg["model"] != "m" || len(d.built[2].RoutingCfg) != 2 {
		t.Fatalf("request provider %v", d.built[2])
	}
	for _, perLaunch := range []bool{false, true} {
		d.resolve = func(map[string]any) (map[string]any, error) {
			return nil, errors.New("Unknown provider fixture-missing")
		}
		d.config = map[string]any{"provider": "fixture-missing"}
		request := map[string]any{"goal": "route", "correlation_id": "refused-route"}
		if perLaunch {
			d.config = map[string]any{}
			request["provider"] = "fixture-missing"
		}
		lifecycleRegistry.mu.Lock()
		records, built := len(lifecycleRegistry.records), len(d.built)
		lifecycleRegistry.mu.Unlock()
		if _, err := s.Launch(ctx, request); !asLifecycleErr(err, "Unknown provider fixture-missing") {
			t.Fatalf("unknown provider %v", err)
		}
		lifecycleRegistry.mu.Lock()
		after := len(lifecycleRegistry.records)
		lifecycleRegistry.mu.Unlock()
		if after != records || len(d.built) != built {
			t.Fatal("refused route registered")
		}
	}
	d.resolve = nil
	d.known["plugin-tools"] = true
	plugin := NewSubagentLifecycleService(func(context.Context) any {
		return &fakeParent{session: "parent-plugin", enabled: []string{"file", "plugin-tools"}}
	})
	h, err := plugin.Launch(ctx, &SubagentLaunchRequest{Goal: "use the plugin", Role: "leaf", AllowedToolsets: []string{"plugin-tools"}})
	if err != nil || plugin.Wait(ctx, h, one(2)).State != SubagentSucceeded {
		t.Fatalf("plugin toolset %v", err)
	}
	if _, err := s.Launch(ctx, &SubagentLaunchRequest{Goal: "x", Role: "leaf", AllowedToolsets: []string{"plugin-tools"}}); !asLifecycleErr(err, "would broaden parent permissions") {
		t.Fatalf("broaden %v", err)
	}
}

// bind_subagent_parent scoping and the per context service the plugin context exposes
func TestSubagentParentBindingAndContextService(t *testing.T) {
	parent := &fakeParent{session: "p"}
	ctx := BindSubagentParent(context.Background(), parent)
	if GetActiveSubagentParent(ctx) != parent || GetActiveSubagentParent(context.Background()) != nil {
		t.Fatal("binding")
	}
	c := newContext(&Manifest{Name: "x"}, NewManager(t.TempDir()))
	if svc := c.SubagentLifecycle(); svc != c.SubagentLifecycle() || svc.parentResolver(ctx) != parent {
		t.Fatal("context service")
	}
}

const hostLifecyclePlugin = `
import json, os
from agent.subagent_lifecycle import SubagentLaunchRequest
def register(ctx):
    def run(args, **kw):
        service = ctx.subagent_lifecycle
        handle = service.launch(SubagentLaunchRequest(goal="review from host"))
        return json.dumps({"pid": os.getpid(), "state": service.status(handle).state,
                           "accepted": service.steer(handle, "check correctness"), "handle": handle})
    ctx.register_tool(name="host_lifecycle", toolset="hostlife",
        schema={"name": "host_lifecycle", "description": "d",
                "parameters": {"type": "object", "properties": {}}}, handler=run)
`

// test_plugin_host.py test_host_plugin_can_launch_status_and_steer_subagent
func TestLiveHostPluginCanLaunchStatusAndSteerSubagent(t *testing.T) {
	installLiveServices(t)
	finish := make(chan struct{})
	d := &fakeDelegation{children: map[string]*fakeChild{}, config: map[string]any{}, known: map[string]bool{},
		run: func(*fakeChild) any {
			<-finish
			return map[string]any{"status": "completed", "summary": "host child done"}
		}}
	withDelegation := *installedServices()
	withDelegation.SubagentLifecycle = d
	InstallServices(&withDelegation)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"hostlife": hostLifecyclePlugin})
	if e := loadedError(m, "hostlife"); e != "" {
		t.Fatalf("load error %q", e)
	}
	parent := &fakeParent{session: "parent-host-lifecycle"}
	def := tools.Default.Lookup(homeCtx(h), h, "host_lifecycle")
	if def == nil {
		t.Fatal("tool not registered")
	}
	out := def.Handler(BindSubagentParent(homeCtx(h), parent), &tools.Call{Name: "host_lifecycle", Args: map[string]any{}})
	close(finish)
	decoded, err := pyjson.Loads(out)
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	result, _ := pyjson.Plain(decoded).(map[string]any)
	if result["state"] != "PENDING" && result["state"] != "RUNNING" || result["accepted"] != true || d.built[0].Goal != "review from host" || d.built[0].Parent != parent {
		t.Fatalf("hosted %v", result)
	}
	if got := d.children["sa-0"].steering; len(got) != 1 || got[0] != "check correctness" {
		t.Fatalf("steering %v", got)
	}
	service := NewSubagentLifecycleService(func(context.Context) any { return parent })
	service.Wait(context.Background(), result["handle"], one(5))
	if r := service.Result(context.Background(), result["handle"]); r.Summary == nil || *r.Summary != "host child done" {
		t.Fatalf("result %+v", r)
	}
}
