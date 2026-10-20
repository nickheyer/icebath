package mode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

func call(name string) protocol.ToolCall {
	return protocol.ToolCall{Name: name, Args: json.RawMessage(`{}`)}
}

func TestModes(t *testing.T) {
	var r tool.Registry
	r.Add(
		tool.Func{Name: "Read", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(context.Context, json.RawMessage) (tool.Result, error) { return tool.Text(""), nil }},
		tool.Func{Name: "Edit", Parameters: json.RawMessage(`{}`), Fn: func(context.Context, json.RawMessage) (tool.Result, error) { return tool.Text(""), nil }},
	)
	base := policy.Rules{Rules: []policy.Rule{{Tool: "Edit", When: func(args json.RawMessage) bool { return string(args) == `{"file_path":"/etc/passwd"}` }, Decision: policy.Deny}, {Tool: "Read", Decision: policy.Allow}}}
	ro := ReadOnlyOf(&r)
	dangerous := protocol.ToolCall{Name: "Edit", Args: json.RawMessage(`{"file_path":"/etc/passwd"}`)}
	cases := []struct {
		mode Mode
		call protocol.ToolCall
		want policy.Decision
	}{
		{Default, call("Read"), policy.Allow},
		{Default, call("Edit"), policy.Ask},
		{Default, call("Bash"), policy.Ask},
		{Plan, call("Read"), policy.Allow},
		{Plan, call("Edit"), policy.Deny},
		{Plan, call("Bash"), policy.Deny},
		{Plan, call("Unknown"), policy.Deny},
		{AcceptEdits, call("Edit"), policy.Allow},
		{AcceptEdits, call("Write"), policy.Allow},
		{AcceptEdits, dangerous, policy.Deny},
		{AcceptEdits, call("Bash"), policy.Ask},
		{Bypass, dangerous, policy.Allow},
		{Bypass, call("Bash"), policy.Allow},
	}
	for _, c := range cases {
		p := Policy{Mode: c.mode, Base: base, ReadOnly: ro}
		if got := p.Decide(c.call); got != c.want {
			t.Errorf("%s %s %s: got %s want %s", c.mode, c.call.Name, c.call.Args, got, c.want)
		}
	}
	if (Policy{Mode: Plan, Base: policy.AllowAll}).Decide(call("Read")) != policy.Deny {
		t.Fatal("without a read only oracle plan denies everything")
	}
	if (Policy{Mode: AcceptEdits, Edits: []string{"Patch"}}).Decide(call("Edit")) != policy.Ask || (Policy{Mode: AcceptEdits, Edits: []string{"Patch"}}).Decide(call("Patch")) != policy.Allow {
		t.Fatal("custom edits")
	}
	if (Policy{}).Decide(call("Read")) != policy.Ask {
		t.Fatal("zero policy asks")
	}
	if m, err := Parse("plan"); err != nil || m != Plan {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := Parse("yolo"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestGuardAsksForDestructiveAndOpenWorldTools(t *testing.T) {
	var r tool.Registry
	fn := func(context.Context, json.RawMessage) (tool.Result, error) { return tool.Text(""), nil }
	r.Add(
		tool.Func{Name: "Read", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true}, Fn: fn},
		tool.Func{Name: "Append", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{Idempotent: true}, Fn: fn},
		tool.Func{Name: "Write", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{Destructive: true}, Fn: fn},
		tool.Func{Name: "Fetch", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true, OpenWorld: true}, Fn: fn},
	)
	g := Guard{Base: policy.AllowAll, Tools: &r}
	for name, want := range map[string]policy.Decision{"Read": policy.Allow, "Append": policy.Allow, "Write": policy.Ask, "Fetch": policy.Ask, "Unknown": policy.Ask} {
		if got := g.Decide(call(name)); got != want {
			t.Errorf("%s: got %s want %s", name, got, want)
		}
	}
	if (Guard{Base: policy.DenyAll, Tools: &r}).Decide(call("Read")) != policy.Deny || (Guard{Base: policy.AskAll, Tools: &r}).Decide(call("Read")) != policy.Ask {
		t.Fatal("a base deny or ask stands")
	}
	if (Guard{Tools: &r}).Decide(call("Read")) != policy.Ask || (Guard{Base: policy.AllowAll}).Decide(call("Read")) != policy.Ask {
		t.Fatal("no base or no registry asks")
	}
}
