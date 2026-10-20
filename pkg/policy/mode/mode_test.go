package mode

import (
	"context"
	"encoding/json"
	"strings"
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
		{Bypass, dangerous, policy.Deny},
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
	if d, why := (Policy{Mode: Plan, Base: base, ReadOnly: ro}).Explain(call("Edit")); d != policy.Deny || !strings.Contains(why, "plan mode") {
		t.Fatalf("%s %s", d, why)
	}
	if d, why := (Policy{Mode: Bypass, Base: base}).Explain(call("Bash")); d != policy.Allow || why != "bypass mode" {
		t.Fatalf("%s %s", d, why)
	}
	if d, why := (Policy{Mode: AcceptEdits, Base: base}).Explain(call("Edit")); d != policy.Allow || why != "accept edits mode" {
		t.Fatalf("%s %s", d, why)
	}
	if d, why := (Policy{Mode: Default, Base: base}).Explain(call("Read")); d != policy.Allow || why != "rule 2 for Read" {
		t.Fatalf("%s %s", d, why)
	}
	if Default.Next() != AcceptEdits || Bypass.Next() != Default || Mode("x").Next() != Default || len(All()) != 4 || Plan.Label() != "plan" || Mode("x").Label() != "default" || AcceptEdits.Label() != "accept edits" || Bypass.Label() != "bypass" {
		t.Fatal("cycle order or labels")
	}
}

func TestSwitchChangesModesLive(t *testing.T) {
	var r tool.Registry
	r.Add(tool.Func{Name: "Read", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(context.Context, json.RawMessage) (tool.Result, error) { return tool.Text(""), nil }})
	var seen []Mode
	s := &Switch{Base: policy.AskAll, ReadOnly: ReadOnlyOf(&r), OnChange: func(m Mode) { seen = append(seen, m) }}
	if s.Mode() != Default || s.Decide(call("Read")) != policy.Ask {
		t.Fatal("default")
	}
	if s.Cycle() != AcceptEdits || s.Decide(call("Edit")) != policy.Allow {
		t.Fatal("accept edits")
	}
	s.Set(Plan)
	if d, why := s.Explain(call("Edit")); d != policy.Deny || !strings.Contains(why, "plan") {
		t.Fatalf("%s %s", d, why)
	}
	if len(seen) != 2 || seen[1] != Plan {
		t.Fatalf("%v", seen)
	}
	var gate policy.Policy = s
	if gate.Decide(call("Read")) != policy.Ask {
		t.Fatal("read only tools follow the base in plan mode")
	}
}

func TestComposeKeepsExplicitAllowRulesUnderTheGuard(t *testing.T) {
	var r tool.Registry
	fn := func(context.Context, json.RawMessage) (tool.Result, error) { return tool.Text(""), nil }
	r.Add(
		tool.Func{Name: "Read", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true}, Fn: fn},
		tool.Func{Name: "Bash", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{Destructive: true, OpenWorld: true}, Fn: fn},
	)
	rules := policy.Rules{Rules: []policy.Rule{{Tool: "Bash", When: func(args json.RawMessage) bool { return strings.Contains(string(args), "git") }, Decision: policy.Allow}}, Default: policy.Allow}
	s := Compose(Default, rules, &r)
	git := protocol.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"git status"}`)}
	other := protocol.ToolCall{Name: "Bash", Args: json.RawMessage(`{"command":"rm x"}`)}
	if d, why := s.Explain(git); d != policy.Allow || why != "rule 1 for Bash" {
		t.Fatalf("an explicit allow rule must survive the guard: %s %s", d, why)
	}
	if d, why := s.Explain(other); d != policy.Ask || !strings.Contains(why, "guard asks before destructive tool Bash") {
		t.Fatalf("a default allow must be guarded: %s %s", d, why)
	}
	if d, _ := s.Explain(call("Read")); d != policy.Allow {
		t.Fatal("read only tools pass the guard")
	}
	s.Set(Plan)
	if s.Decide(other) != policy.Deny || s.Decide(call("Read")) != policy.Allow {
		t.Fatal("plan mode over the stack")
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
	if d, why := g.Explain(call("Fetch")); d != policy.Ask || why != "guard asks before open world tool Fetch" {
		t.Fatalf("%s %s", d, why)
	}
	if d, why := (Guard{Base: policy.AllowAll}).Explain(call("Read")); d != policy.Ask || why != "guard asks for tools it cannot describe" {
		t.Fatalf("%s %s", d, why)
	}
}
