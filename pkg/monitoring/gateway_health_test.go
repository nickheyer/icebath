package monitoring

import (
	"math"
	"math/big"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Decoded JSON object of a status payload literal
func statusObject(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

// Event dicts one transition emits through a fresh enabled process emitter
func captureTransition(t *testing.T, previous, current *pyjson.Object) []*pyjson.Object {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	em := NewMonitoringEmitter(true)
	ResetEmitterForTests(em)
	defer ResetEmitterForTests(nil)
	var mu sync.Mutex
	var seen []*pyjson.Object
	em.Subscribe(NewSubscriber(func(batch []*pyjson.Object) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, batch...)
	}))
	EmitRuntimeStatusTransition(previous, current)
	em.Flush(2.0)
	mu.Lock()
	defer mu.Unlock()
	return append([]*pyjson.Object(nil), seen...)
}

// Field of an event dict
func field(ev *pyjson.Object, key string) any {
	v, _ := ev.Get(key)
	return v
}

func TestClassifyGatewayError(t *testing.T) {
	cases := []struct {
		raw  any
		want string
	}{
		{"401 Unauthorized", "auth_failed"},
		{"Bad TOKEN", "auth_failed"},
		{"Rate limit hit", "rate_limited"},
		{"request timed out", "timeout"},
		{"Connection refused", "network_error"},
		{"Name resolution failed", "network_error"},
		{"missing bot id", "invalid_config"},
		{"startup crashed", "startup_failed"},
		{"FATAL", "platform_fatal"},
		{"weird", "unknown"},
		{nil, "unknown"},
		{"", "unknown"},
		{int64(0), "unknown"},
		{int64(401), "auth_failed"},
	}
	for _, c := range cases {
		if got := ClassifyGatewayError(c.raw); got != c.want {
			t.Fatalf("%v: got %q want %q", c.raw, got, c.want)
		}
	}
}

func TestClassifyExitReason(t *testing.T) {
	str := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	cases := []struct {
		raw     any
		state   any
		restart bool
		want    any
	}{
		{"anything", "stopped", true, "restart_requested"},
		{nil, "stopped", false, nil},
		{nil, "startup_failed", false, "startup_failed"},
		{"bad token", "startup_failed", false, "auth_failed"},
		{"weird", "STARTUP_FAILED", false, "startup_failed"},
		{"received SIGTERM", "stopped", false, "signal"},
		{"Gateway shutdown", "stopped", false, "planned_stop"},
		{"Gateway shutdown", "running", false, "unknown"},
		{"connection reset", "stopped", false, "network_error"},
		{"", "stopped", false, "unknown"},
	}
	for _, c := range cases {
		if got := str(ClassifyExitReason(c.raw, c.state, c.restart)); got != c.want {
			t.Fatalf("%v %v %v: got %v want %v", c.raw, c.state, c.restart, got, c.want)
		}
	}
}

func TestOptionalStateKeepsAbsentAndBoundsUnknown(t *testing.T) {
	if optionalState(nil, knownGatewayStates) != nil {
		t.Fatal("absent state was not nil")
	}
	for raw, want := range map[any]string{"RUNNING": "running", "": "unknown", "bogus": "unknown", int64(0): "unknown"} {
		if got := optionalState(raw, knownGatewayStates); got == nil || *got != want {
			t.Fatalf("%v: got %v want %q", raw, got, want)
		}
	}
}

func TestCoercePID(t *testing.T) {
	huge, _ := new(big.Int).SetString("99999999999999999999", 10)
	cases := []struct {
		raw  any
		want any
	}{
		{int64(42), 42},
		{"17", 17},
		{0, nil},
		{int64(-3), nil},
		{"abc", nil},
		{nil, nil},
		{2.9, 2},
		{huge, huge},
	}
	for _, c := range cases {
		got, err := coercePID(c.raw)
		if err != nil || !pyjson.Equal(got, c.want) {
			t.Fatalf("%v: got %v %v want %v", c.raw, got, err, c.want)
		}
	}
}

func TestParseActiveAgentsUsesGatewayContractThenFallback(t *testing.T) {
	prev := GatewayParseActiveAgents
	defer func() { GatewayParseActiveAgents = prev }()
	GatewayParseActiveAgents = func(raw any) (any, error) { return int64(7), nil }
	if got, err := parseActiveAgents("3"); err != nil || !pyjson.Equal(got, 7) {
		t.Fatalf("contract result ignored: %v %v", got, err)
	}
	GatewayParseActiveAgents = func(raw any) (any, error) { panic("boom") }
	if got, err := parseActiveAgents("3"); err != nil || !pyjson.Equal(got, 3) {
		t.Fatalf("panicking contract did not fall back: %v %v", got, err)
	}
	GatewayParseActiveAgents = nil
	for raw, want := range map[any]any{"-2": 0, "x": 0, int64(5): 5} {
		if got, err := parseActiveAgents(raw); err != nil || !pyjson.Equal(got, want) {
			t.Fatalf("%v: got %v %v want %v", raw, got, err, want)
		}
	}
}

func TestEmitRuntimeStatusTransitionLifecycleAndExit(t *testing.T) {
	previous := statusObject(t, `{"gateway_state": "running", "platforms": {}}`)
	current := statusObject(t, `{"gateway_state": "stopped", "exit_reason": "Gateway shutdown", "active_agents": 2, "pid": 4321, "platforms": {}}`)
	events := captureTransition(t, previous, current)
	if len(events) != 2 {
		t.Fatalf("events %v", events)
	}
	for i, name := range []string{"gateway.lifecycle", "gateway.exit"} {
		ev := events[i]
		if field(ev, "event") != "gateway_health" || field(ev, "name") != name {
			t.Fatalf("event %d %v", i, ev)
		}
		if field(ev, "old_state") != "running" || field(ev, "new_state") != "stopped" || field(ev, "gateway_state") != "stopped" {
			t.Fatalf("states %v", ev)
		}
		if field(ev, "exit_reason") != "planned_stop" || field(ev, "restart_requested") != false {
			t.Fatalf("exit %v", ev)
		}
		if !pyjson.Equal(field(ev, "active_agents"), 2) || !pyjson.Equal(field(ev, "pid"), 4321) {
			t.Fatalf("agents or pid %v", ev)
		}
		if p, _ := field(ev, "profile").(string); p == "" {
			t.Fatalf("profile %v", ev)
		}
		if v, _ := field(ev, "version").(string); v == "" {
			t.Fatalf("version %v", ev)
		}
	}
}

func TestEmitRuntimeStatusTransitionStartupFailedDiagnostic(t *testing.T) {
	current := statusObject(t, `{"gateway_state": "startup_failed", "exit_reason": "invalid token"}`)
	events := captureTransition(t, nil, current)
	if len(events) != 2 {
		t.Fatalf("events %v", events)
	}
	if field(events[0], "name") != "gateway.lifecycle" || field(events[0], "old_state") != nil || field(events[0], "exit_reason") != "auth_failed" {
		t.Fatalf("lifecycle %v", events[0])
	}
	if !pyjson.Equal(field(events[0], "active_agents"), 0) || field(events[0], "pid") != nil {
		t.Fatalf("defaults %v", events[0])
	}
	diag := events[1]
	if field(diag, "event") != "gateway_diagnostic" || field(diag, "name") != "gateway.startup_failed" || field(diag, "subsystem") != "gateway" {
		t.Fatalf("diagnostic %v", diag)
	}
	if field(diag, "error_class") != "auth_failed" || field(diag, "error_code") != "auth_failed" || field(diag, "severity") != "error" {
		t.Fatalf("diagnostic class %v", diag)
	}
}

func TestEmitRuntimeStatusTransitionPlatformStateChanges(t *testing.T) {
	previous := statusObject(t, `{"gateway_state": "running", "platforms": {"telegram": {"state": "connected"}, "slack": {"state": "connecting"}}}`)
	current := statusObject(t, `{"gateway_state": "running", "platforms": {
		"telegram": {"state": "fatal", "error_message": "401 unauthorized"},
		"slack": {"state": "connecting"},
		"discord": {"state": "retrying", "error_code": "socket closed"},
		"matrix": {"error_message": "no state"}}}`)
	events := captureTransition(t, previous, current)
	if len(events) != 3 {
		t.Fatalf("events %v", events)
	}
	change, fatal, discord := events[0], events[1], events[2]
	if field(change, "name") != "platform.state_change" || field(change, "subsystem") != "platform.telegram" || field(change, "platform") != "telegram" {
		t.Fatalf("change %v", change)
	}
	if field(change, "old_state") != "connected" || field(change, "new_state") != "fatal" || field(change, "severity") != "error" || field(change, "error_code") != "auth_failed" {
		t.Fatalf("change detail %v", change)
	}
	if field(fatal, "name") != "platform.fatal" || field(fatal, "old_state") != nil || field(fatal, "new_state") != nil || field(fatal, "error_class") != "auth_failed" {
		t.Fatalf("fatal %v", fatal)
	}
	if field(discord, "name") != "platform.state_change" || field(discord, "old_state") != nil || field(discord, "new_state") != "retrying" || field(discord, "severity") != "warning" || field(discord, "error_code") != "network_error" {
		t.Fatalf("discord %v", discord)
	}
}

func TestEmitRuntimeStatusTransitionUnchangedStateEmitsNothing(t *testing.T) {
	previous := statusObject(t, `{"gateway_state": "running", "platforms": {"telegram": {"state": "connected"}}}`)
	current := statusObject(t, `{"gateway_state": "RUNNING", "platforms": {"telegram": {"state": "Connected"}, "slack": "malformed"}}`)
	if events := captureTransition(t, previous, current); len(events) != 0 {
		t.Fatalf("events %v", events)
	}
}

func TestEmitRuntimeStatusTransitionOverflowFailsOpen(t *testing.T) {
	prev := GatewayParseActiveAgents
	defer func() { GatewayParseActiveAgents = prev }()
	GatewayParseActiveAgents = nil
	current := pyjson.NewObject()
	current.Set("gateway_state", "running")
	current.Set("active_agents", math.Inf(1))
	if events := captureTransition(t, nil, current); len(events) != 0 {
		t.Fatalf("events %v", events)
	}
}
