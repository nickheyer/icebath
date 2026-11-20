package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type automationTextForm struct{}

func (automationTextForm) String() string { return "text form" }

// One automation call against the case's home, its reply as the wire JSON text
func automationReply(t *testing.T, kind, call string, args []any, kwargs *pyjson.Object) *pyjson.Object {
	t.Helper()
	reply, err := InvokeViewerAutomation(context.Background(), nil, kind, "viewer-auto-sid", call, args, kwargs)
	if err != nil {
		t.Fatalf("%s.%s failed %v", kind, call, err)
	}
	text, err := pyjson.Dumps(reply)
	if err != nil {
		t.Fatalf("%s.%s reply does not serialise %v", kind, call, err)
	}
	wire, err := pyjson.LoadsDict(text)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestViewerAutomationInvokeWiresEveryReturnShape(t *testing.T) {
	goalsTestHome(t)
	contract := pyjson.ObjectOf("verification", "the suite is green")
	set := automationReply(t, "goal", "set", []any{"ship the port"}, pyjson.ObjectOf("contract", contract))
	wrapped, isState := set.GetOr("value", nil).(*pyjson.Object)
	if !isState {
		t.Fatalf("set value %#v", set.GetOr("value", nil))
	}
	stateText, _ := wrapped.GetOr("__state__", nil).(string)
	if !strings.Contains(stateText, "ship the port") || !strings.Contains(stateText, "the suite is green") {
		t.Fatalf("set state %q", stateText)
	}
	if after, _ := set.GetOr("state", nil).(string); after != stateText {
		t.Fatalf("state after %q differs from value %q", after, stateText)
	}

	if read := automationReply(t, "goal", "state", nil, nil); read.GetOr("value", "unset") != nil || read.GetOr("state", nil) != stateText {
		t.Fatalf("state read %#v", read)
	}

	command := automationReply(t, "goal", "command", []any{"status"}, nil)
	result, isDict := command.GetOr("value", nil).(*pyjson.Object)
	if !isDict {
		t.Fatalf("command value %#v", command.GetOr("value", nil))
	}
	for _, key := range []string{"output", "prompt", "kickoff", "clear_pending", "error"} {
		if _, present := result.Get(key); !present {
			t.Fatalf("command result lacks %q in %v", key, result.Keys)
		}
	}
	if output, _ := result.GetOr("output", "").(string); !strings.Contains(output, "ship the port") || result.GetOr("error", true) != false {
		t.Fatalf("command result %#v", result)
	}

	gate := automationReply(t, "goal", "add_gate", []any{"true"}, nil)
	gateValue, isGate := gate.GetOr("value", nil).(*pyjson.Object)
	if !isGate {
		t.Fatalf("gate value %#v", gate.GetOr("value", nil))
	}
	gateDict, _ := gateValue.GetOr("__gate__", nil).(*pyjson.Object)
	if gateDict == nil || gateDict.GetOr("command", "") != "true" {
		t.Fatalf("gate dict %#v", gateValue)
	}

	if waiting := automationReply(t, "goal", "is_waiting", nil, nil); waiting.GetOr("value", nil) != false {
		t.Fatalf("is_waiting %#v", waiting)
	}
	if blocks := automationReply(t, "loop", "goal_blocks_tick", nil, nil); blocks.GetOr("value", nil) != true {
		t.Fatalf("goal_blocks_tick %#v", blocks)
	}

	empty := automationReply(t, "goal", "set", []any{"  "}, nil)
	failure, isError := empty.GetOr("error", nil).(*pyjson.Object)
	if !isError || failure.GetOr("type", "") != "ValueError" || failure.GetOr("message", "") != "goal text is empty" || empty.GetOr("value", "absent") != "absent" {
		t.Fatalf("empty goal reply %#v", empty)
	}
	if kept, _ := empty.GetOr("state", nil).(string); !strings.Contains(kept, "ship the port") {
		t.Fatalf("state after a refused set %q", kept)
	}

	tick := automationReply(t, "loop", "complete_tick", []any{"done"}, nil)
	decision, isDecision := tick.GetOr("value", nil).(*pyjson.Object)
	if !isDecision || decision.GetOr("reason", nil) != "no tick in flight" || decision.GetOr("stopped", nil) != false {
		t.Fatalf("complete_tick value %#v", tick.GetOr("value", nil))
	}
}

func TestViewerAutomationWireRecursesIntoContainers(t *testing.T) {
	gate := NewHeartbeatState("check the deploy", 600)
	nested := pyjson.ObjectOf("rows", []any{int64(1), "two", pyjson.ObjectOf("state", gate)}, "plain", 1.5)
	wired, err := wireAutomationValue(nested)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := wired.(*pyjson.Object).GetOr("rows", nil).([]any)
	if len(rows) != 3 || rows[0] != int64(1) || rows[1] != "two" {
		t.Fatalf("rows %#v", rows)
	}
	inner, _ := rows[2].(*pyjson.Object).GetOr("state", nil).(*pyjson.Object)
	if text, _ := inner.GetOr("__state__", nil).(string); !strings.Contains(text, "check the deploy") {
		t.Fatalf("nested state %#v", inner)
	}
	if other, _ := wireAutomationValue(automationTextForm{}); other != "text form" {
		t.Fatalf("fallback text %#v", other)
	}
}
