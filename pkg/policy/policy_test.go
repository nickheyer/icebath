package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

var (
	readCall  = protocol.ToolCall{ID: "1", Name: "read", Args: json.RawMessage(`{"path":"a"}`)}
	bashCall  = protocol.ToolCall{ID: "2", Name: "bash", Args: json.RawMessage(`{"cmd":"rm -rf /"}`)}
	otherCall = protocol.ToolCall{ID: "3", Name: "other", Args: json.RawMessage(`{}`)}
)

func TestRulesFirstMatchWins(t *testing.T) {
	rules := Rules{Rules: []Rule{
		{Tool: "read", Decision: Allow},
		{Tool: "bash", When: func(args json.RawMessage) bool { return strings.Contains(string(args), "rm ") }, Decision: Deny},
		{Tool: "bash", Decision: Ask},
		{Tool: "*", Decision: Allow},
	}}
	if rules.Decide(readCall) != Allow || rules.Decide(bashCall) != Deny || rules.Decide(otherCall) != Allow {
		t.Fatal("rule order")
	}
	safe := protocol.ToolCall{Name: "bash", Args: json.RawMessage(`{"cmd":"ls"}`)}
	if rules.Decide(safe) != Ask {
		t.Fatal("fallthrough to the second bash rule")
	}
	if (Rules{}).Decide(readCall) != Ask || (Rules{Default: Deny}).Decide(readCall) != Deny {
		t.Fatal("default")
	}
	if AllowAll.Decide(bashCall) != Allow || DenyAll.Decide(readCall) != Deny || AskAll.Decide(readCall) != Ask {
		t.Fatal("static")
	}
	if PolicyFunc(func(protocol.ToolCall) Decision { return Deny }).Decide(readCall) != Deny {
		t.Fatal("func")
	}
}

func TestStrictest(t *testing.T) {
	if Strictest().Decide(readCall) != Allow {
		t.Fatal("empty")
	}
	if Strictest(AllowAll, AskAll).Decide(readCall) != Ask || Strictest(AskAll, DenyAll, AllowAll).Decide(readCall) != Deny || Strictest(AllowAll, AllowAll).Decide(readCall) != Allow {
		t.Fatal("precedence")
	}
	if d := Strictest(Static("weird"), DenyAll).Decide(readCall); d != "weird" {
		t.Fatalf("unknown decision must rank strictest, got %s", d)
	}
}

func TestGateCheck(t *testing.T) {
	ctx := context.Background()
	var denied *Denied
	if err := (Gate{}).Check(ctx, readCall); !errors.As(err, &denied) || denied.Call.Name != "read" {
		t.Fatalf("zero gate must deny, got %v", err)
	}
	if err := (Gate{Policy: AllowAll}).Check(ctx, bashCall); err != nil {
		t.Fatal(err)
	}
	if err := (Gate{Policy: DenyAll, Approver: ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { return true, nil })}).Check(ctx, readCall); !errors.As(err, &denied) {
		t.Fatal("deny must not consult the approver")
	}
	asked := 0
	yes := ApproverFunc(func(_ context.Context, call protocol.ToolCall) (bool, error) {
		asked++
		return call.Name == "read", nil
	})
	g := Gate{Policy: AskAll, Approver: yes}
	if err := g.Check(ctx, readCall); err != nil || asked != 1 {
		t.Fatalf("%v %d", err, asked)
	}
	if err := g.Check(ctx, bashCall); !errors.As(err, &denied) || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("%v", err)
	}
	boom := errors.New("ui gone")
	g.Approver = ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { return false, boom })
	if err := g.Check(ctx, readCall); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if err := (Gate{Policy: Static("maybe")}).Check(ctx, readCall); !errors.As(err, &denied) {
		t.Fatal("unknown decision must deny")
	}
}

func TestGateRecordsEveryDecision(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	g := Gate{Policy: Rules{Rules: []Rule{{Tool: "read", Decision: Allow}, {Tool: "bash", Decision: Deny}}}, Approver: ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { return true, nil }), Recorder: Log(&buf)}
	g.Check(ctx, readCall)
	g.Check(ctx, bashCall)
	g.Check(ctx, otherCall)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%q", buf.String())
	}
	var entries []Entry
	for _, l := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	if entries[0].Tool != "read" || !entries[0].Allowed || entries[0].Decision != Allow || entries[0].Reason != "allowed by policy" || entries[0].Time.IsZero() {
		t.Fatalf("%+v", entries[0])
	}
	if entries[1].Tool != "bash" || entries[1].Allowed || entries[1].Decision != Deny || string(entries[1].Args) != `{"cmd":"rm -rf /"}` {
		t.Fatalf("%+v", entries[1])
	}
	if entries[2].Decision != Ask || !entries[2].Allowed || entries[2].Reason != "approved" {
		t.Fatalf("%+v", entries[2])
	}
	var got []Entry
	g.Recorder = RecorderFunc(func(e Entry) { got = append(got, e) })
	g.Approver = ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { return false, errors.New("gone") })
	g.Check(ctx, otherCall)
	if len(got) != 1 || got[0].Allowed || got[0].Reason != "gone" {
		t.Fatalf("%+v", got)
	}
}

func TestMemoryRemembersAlwaysAndNever(t *testing.T) {
	ctx := context.Background()
	answers := []Verdict{Yes, Always, No, Never}
	asked := 0
	m := &Memory{Ask: AskerFunc(func(context.Context, protocol.ToolCall) (Verdict, error) {
		v := answers[asked]
		asked++
		return v, nil
	})}
	if ok, err := m.Approve(ctx, readCall); !ok || err != nil || asked != 1 {
		t.Fatalf("%v %v %d", ok, err, asked)
	}
	if ok, _ := m.Approve(ctx, readCall); !ok || asked != 2 {
		t.Fatal("yes must not be remembered")
	}
	if ok, _ := m.Approve(ctx, readCall); !ok || asked != 2 {
		t.Fatal("always must be remembered")
	}
	if ok, _ := m.Approve(ctx, bashCall); ok || asked != 3 {
		t.Fatal("no")
	}
	if ok, _ := m.Approve(ctx, bashCall); ok || asked != 4 {
		t.Fatal("no must not be remembered")
	}
	var denied *Denied
	if ok, err := m.Approve(ctx, bashCall); ok || asked != 4 || !errors.As(err, &denied) || !strings.Contains(err.Error(), "never") {
		t.Fatalf("%v %v", ok, err)
	}
	if strings.Join(m.Allowed(), ",") != "read" || strings.Join(m.Denied(), ",") != "bash" {
		t.Fatalf("%v %v", m.Allowed(), m.Denied())
	}
	m.Forget("bash")
	m.Remember("other", false)
	m.Remember("read", false)
	if strings.Join(m.Allowed(), ",") != "" || strings.Join(m.Denied(), ",") != "other,read" {
		t.Fatalf("%v %v", m.Allowed(), m.Denied())
	}
	keyed := &Memory{Key: func(c protocol.ToolCall) string { return c.Name + ":" + string(c.Args) }, Ask: AskerFunc(func(context.Context, protocol.ToolCall) (Verdict, error) { return Always, nil })}
	keyed.Approve(ctx, readCall)
	if keyed.Allowed()[0] != `read:{"path":"a"}` {
		t.Fatalf("%v", keyed.Allowed())
	}
	if _, err := (&Memory{}).Approve(ctx, readCall); !errors.As(err, &denied) {
		t.Fatal("memory without asker must deny")
	}
	boom := errors.New("ui gone")
	if _, err := (&Memory{Ask: AskerFunc(func(context.Context, protocol.ToolCall) (Verdict, error) { return No, boom })}).Approve(ctx, readCall); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	for v, want := range map[Verdict]string{Yes: "yes", No: "no", Always: "always", Never: "never"} {
		if v.String() != want || v.Allowed() != (v == Yes || v == Always) {
			t.Fatalf("%v", v)
		}
	}
}

func TestGateResolveAndStricter(t *testing.T) {
	ctx := context.Background()
	var denied *Denied
	var entries []Entry
	rec := RecorderFunc(func(e Entry) { entries = append(entries, e) })
	asked := 0
	approver := ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { asked++; return true, nil })
	if err := (Gate{Policy: AskAll, Recorder: rec}).Resolve(ctx, readCall, Allow); err != nil || entries[0].Decision != Allow || entries[0].Reason != "allowed by settled decision" {
		t.Fatalf("%v %+v", err, entries)
	}
	if err := (Gate{Policy: AllowAll, Approver: approver, Recorder: rec}).Resolve(ctx, readCall, Ask); err != nil || asked != 1 || entries[1].Decision != Ask || entries[1].Reason != "approved" {
		t.Fatalf("%v %d %+v", err, asked, entries)
	}
	if err := (Gate{Policy: DenyAll, Recorder: rec}).Resolve(ctx, readCall, Allow); !errors.As(err, &denied) || denied.Reason != "denied by policy" {
		t.Fatalf("settled allow must not beat deny: %v", err)
	}
	if err := (Gate{Policy: AllowAll, Recorder: rec}).Resolve(ctx, readCall, Deny); !errors.As(err, &denied) || denied.Reason != "denied by settled decision" {
		t.Fatalf("%v", err)
	}
	if err := (Gate{Policy: AllowAll}).Resolve(ctx, readCall, ""); err != nil {
		t.Fatal("empty settles nothing")
	}
	for _, c := range []struct{ a, b, want Decision }{
		{"", Allow, Allow}, {Allow, "", Allow}, {Allow, Ask, Ask}, {Ask, Allow, Ask}, {Ask, Deny, Deny}, {Deny, Ask, Deny}, {"", "", ""}, {Allow, Allow, Allow},
	} {
		if got := Stricter(c.a, c.b); got != c.want {
			t.Errorf("Stricter(%q, %q) = %q", c.a, c.b, got)
		}
	}
}
