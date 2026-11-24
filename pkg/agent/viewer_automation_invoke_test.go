package agent

import (
	"context"
	"strings"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

type automationTextForm struct{}

func (automationTextForm) String() string { return "text form" }

// One automation call against the case's home, its reply as the wire JSON text
func automationReply(t *testing.T, kind, call string, args []any, kwargs *jsonutil.Object) *jsonutil.Object {
	t.Helper()
	reply, err := InvokeViewerAutomation(context.Background(), nil, kind, "viewer-auto-sid", call, args, kwargs)
	if err != nil {
		t.Fatalf("%s.%s failed %v", kind, call, err)
	}
	text, err := jsonutil.Encode(reply)
	if err != nil {
		t.Fatalf("%s.%s reply does not serialise %v", kind, call, err)
	}
	wire, err := jsonutil.DecodeObject(text)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestViewerAutomationInvokeWiresEveryReturnShape(t *testing.T) {
	goalsTestHome(t)
	contract := jsonutil.ObjectOf("verification", "the suite is green")
	set := automationReply(t, "goal", "set", []any{"ship the port"}, jsonutil.ObjectOf("contract", contract))
	wrapped, isState := set.ValueOr("value", nil).(*jsonutil.Object)
	if !isState {
		t.Fatalf("set value %#v", set.ValueOr("value", nil))
	}
	stateText, _ := wrapped.ValueOr("__state__", nil).(string)
	if !strings.Contains(stateText, "ship the port") || !strings.Contains(stateText, "the suite is green") {
		t.Fatalf("set state %q", stateText)
	}
	if after, _ := set.ValueOr("state", nil).(string); after != stateText {
		t.Fatalf("state after %q differs from value %q", after, stateText)
	}

	if read := automationReply(t, "goal", "state", nil, nil); read.ValueOr("value", "unset") != nil || read.ValueOr("state", nil) != stateText {
		t.Fatalf("state read %#v", read)
	}

	command := automationReply(t, "goal", "command", []any{"status"}, nil)
	result, isDict := command.ValueOr("value", nil).(*jsonutil.Object)
	if !isDict {
		t.Fatalf("command value %#v", command.ValueOr("value", nil))
	}
	for _, key := range []string{"output", "prompt", "kickoff", "clear_pending", "error"} {
		if _, present := result.Get(key); !present {
			t.Fatalf("command result lacks %q in %v", key, result.Keys())
		}
	}
	if output, _ := result.ValueOr("output", "").(string); !strings.Contains(output, "ship the port") || result.ValueOr("error", true) != false {
		t.Fatalf("command result %#v", result)
	}

	gate := automationReply(t, "goal", "add_gate", []any{"true"}, nil)
	gateValue, isGate := gate.ValueOr("value", nil).(*jsonutil.Object)
	if !isGate {
		t.Fatalf("gate value %#v", gate.ValueOr("value", nil))
	}
	gateDict, _ := gateValue.ValueOr("__gate__", nil).(*jsonutil.Object)
	if gateDict == nil || gateDict.ValueOr("command", "") != "true" {
		t.Fatalf("gate dict %#v", gateValue)
	}

	if waiting := automationReply(t, "goal", "is_waiting", nil, nil); waiting.ValueOr("value", nil) != false {
		t.Fatalf("is_waiting %#v", waiting)
	}
	if blocks := automationReply(t, "loop", "goal_blocks_tick", nil, nil); blocks.ValueOr("value", nil) != true {
		t.Fatalf("goal_blocks_tick %#v", blocks)
	}

	empty := automationReply(t, "goal", "set", []any{"  "}, nil)
	failure, isError := empty.ValueOr("error", nil).(*jsonutil.Object)
	if !isError || failure.ValueOr("type", "") != "ValueError" || failure.ValueOr("message", "") != "goal text is empty" || empty.ValueOr("value", "absent") != "absent" {
		t.Fatalf("empty goal reply %#v", empty)
	}
	if kept, _ := empty.ValueOr("state", nil).(string); !strings.Contains(kept, "ship the port") {
		t.Fatalf("state after a refused set %q", kept)
	}

	tick := automationReply(t, "loop", "complete_tick", []any{"done"}, nil)
	decision, isDecision := tick.ValueOr("value", nil).(*jsonutil.Object)
	if !isDecision || decision.ValueOr("reason", nil) != "no tick in flight" || decision.ValueOr("stopped", nil) != false {
		t.Fatalf("complete_tick value %#v", tick.ValueOr("value", nil))
	}
}

func TestViewerAutomationWireRecursesIntoContainers(t *testing.T) {
	gate := NewHeartbeatState("check the deploy", 600)
	nested := jsonutil.ObjectOf("rows", []any{int64(1), "two", jsonutil.ObjectOf("state", gate)}, "plain", 1.5)
	wired, err := wireAutomationValue(nested)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := wired.(*jsonutil.Object).ValueOr("rows", nil).([]any)
	if len(rows) != 3 || rows[0] != int64(1) || rows[1] != "two" {
		t.Fatalf("rows %#v", rows)
	}
	inner, _ := rows[2].(*jsonutil.Object).ValueOr("state", nil).(*jsonutil.Object)
	if text, _ := inner.ValueOr("__state__", nil).(string); !strings.Contains(text, "check the deploy") {
		t.Fatalf("nested state %#v", inner)
	}
	if other, _ := wireAutomationValue(automationTextForm{}); other != "text form" {
		t.Fatalf("fallback text %#v", other)
	}
}
