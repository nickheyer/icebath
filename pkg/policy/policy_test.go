package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

var (
	readCall  = protocol.ToolCall{ID: "1", Name: "read", Args: json.RawMessage(`{"path":"a"}`)}
	bashCall  = protocol.ToolCall{ID: "2", Name: "bash", Args: json.RawMessage(`{"cmd":"rm -rf /"}`)}
	otherCall = protocol.ToolCall{ID: "3", Name: "other", Args: json.RawMessage(`{}`)}
)

func yesFor(name string, asked *int) Approver {
	return ApproverFunc(func(_ context.Context, req Request) (Answer, error) {
		*asked++
		return Answer{Allowed: req.Call.Name == name}, nil
	})
}

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
	if d, why := rules.Explain(bashCall); d != Deny || why != "rule 2 for bash" {
		t.Fatalf("%s %s", d, why)
	}
	if d, why := (Rules{}).Explain(readCall); d != Ask || why != "default" {
		t.Fatalf("%s %s", d, why)
	}
	if (Rules{Default: Deny}).Decide(readCall) != Deny {
		t.Fatal("default")
	}
	if AllowAll.Decide(bashCall) != Allow || DenyAll.Decide(readCall) != Deny || AskAll.Decide(readCall) != Ask {
		t.Fatal("static")
	}
	if PolicyFunc(func(protocol.ToolCall) Decision { return Deny }).Decide(readCall) != Deny {
		t.Fatal("func")
	}
	if d, why := Explain(AllowAll, readCall); d != Allow || why != "policy" {
		t.Fatalf("plain policies explain as policy: %s %s", d, why)
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
	if d, why := Strictest(AllowAll, Rules{Rules: []Rule{{Tool: "read", Decision: Deny}}}).(Explainer).Explain(readCall); d != Deny || why != "rule 1 for read" {
		t.Fatalf("%s %s", d, why)
	}
}

func TestSwitchSwapsPolicies(t *testing.T) {
	var s Switch
	if s.Decide(readCall) != Ask {
		t.Fatal("empty switch asks")
	}
	s.Set(AllowAll)
	if s.Decide(readCall) != Allow || s.Get() != AllowAll {
		t.Fatal("set")
	}
	s.Set(nil)
	if d, why := s.Explain(readCall); d != Ask || why != "policy" {
		t.Fatalf("%s %s", d, why)
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
	asked := 0
	if err := (Gate{Policy: DenyAll, Approver: yesFor("read", &asked)}).Check(ctx, readCall); !errors.As(err, &denied) || asked != 0 {
		t.Fatal("deny must not consult the approver")
	}
	g := Gate{Policy: AskAll, Approver: yesFor("read", &asked)}
	if err := g.Check(ctx, readCall); err != nil || asked != 1 {
		t.Fatalf("%v %d", err, asked)
	}
	if err := g.Check(ctx, bashCall); !errors.As(err, &denied) || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("%v", err)
	}
	boom := errors.New("ui gone")
	g.Approver = ApproverFunc(func(context.Context, Request) (Answer, error) { return Answer{}, boom })
	if err := g.Check(ctx, readCall); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if err := (Gate{Policy: Static("maybe")}).Check(ctx, readCall); !errors.As(err, &denied) {
		t.Fatal("unknown decision must deny")
	}
}

type keyed struct{ Static }

func (keyed) Key(call protocol.ToolCall) string { return call.Name + "(" + string(call.Args) + ")" }
func (keyed) SubjectsOf(call protocol.ToolCall) []Subject {
	return []Subject{{Kind: "plain", Text: string(call.Args)}}
}

func TestGateBuildsRichRequests(t *testing.T) {
	var reg tool.Registry
	reg.Add(tool.Func{Name: "read", Description: "Reads a file", Parameters: json.RawMessage(`{"type":"object"}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(context.Context, json.RawMessage) (tool.Result, error) { return tool.Text(""), nil }})
	var asked []Request
	var answered []Answer
	g := Gate{
		Policy:   keyed{Static(Ask)},
		Tools:    &reg,
		OnAsk:    func(r Request) { asked = append(asked, r) },
		OnAnswer: func(_ Request, a Answer, _ error) { answered = append(answered, a) },
		Approver: ApproverFunc(func(_ context.Context, r Request) (Answer, error) {
			return Answer{Allowed: true, By: "person", Verdict: Always}, nil
		}),
	}
	var entries []Entry
	g.Recorder = RecorderFunc(func(e Entry) { entries = append(entries, e) })
	ctx := WithAgent(context.Background(), "root")
	if err := g.Check(ctx, readCall); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].Agent != "root" || asked[0].Reason != "policy" || asked[0].Key != `read({"path":"a"})` || len(asked[0].Subjects) != 1 || asked[0].Tool == nil || asked[0].Tool.Description != "Reads a file" || !asked[0].Hints.ReadOnly {
		t.Fatalf("%+v", asked)
	}
	if len(answered) != 1 || !answered[0].Allowed || answered[0].By != "person" {
		t.Fatalf("%+v", answered)
	}
	if e := entries[0]; e.Agent != "root" || e.By != "person" || e.Verdict != "always" || e.Key != `read({"path":"a"})` || e.Reason != "approved by person" {
		t.Fatalf("%+v", e)
	}
	if AgentFrom(context.Background()) != "" {
		t.Fatal("agent from a bare context")
	}
}

func TestGateRecordsEveryDecision(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	g := Gate{Policy: Rules{Rules: []Rule{{Tool: "read", Decision: Allow}, {Tool: "bash", Decision: Deny}}}, Approver: ApproverFunc(func(context.Context, Request) (Answer, error) { return Answer{Allowed: true}, nil }), Recorder: Log(&buf)}
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
	if entries[0].Tool != "read" || !entries[0].Allowed || entries[0].Decision != Allow || entries[0].Reason != "allowed by rule 1 for read" || entries[0].Time.IsZero() {
		t.Fatalf("%+v", entries[0])
	}
	if entries[1].Tool != "bash" || entries[1].Allowed || entries[1].Decision != Deny || entries[1].Reason != "denied by rule 2 for bash" || string(entries[1].Args) != `{"cmd":"rm -rf /"}` {
		t.Fatalf("%+v", entries[1])
	}
	if entries[2].Decision != Ask || !entries[2].Allowed || entries[2].Reason != "approved" {
		t.Fatalf("%+v", entries[2])
	}
	var got []Entry
	g.Recorder = RecorderFunc(func(e Entry) { got = append(got, e) })
	g.Approver = ApproverFunc(func(context.Context, Request) (Answer, error) { return Answer{}, errors.New("gone") })
	g.Check(ctx, otherCall)
	if len(got) != 1 || got[0].Allowed || got[0].Reason != "gone" {
		t.Fatalf("%+v", got)
	}
}

func TestMemoryRemembersAlwaysAndNever(t *testing.T) {
	ctx := context.Background()
	answers := []Verdict{Yes, Always, No, Never}
	asked := 0
	var changes []string
	m := &Memory{Ask: AskerFunc(func(context.Context, Request) (Verdict, error) {
		v := answers[asked]
		asked++
		return v, nil
	}), OnChange: func(key string, allow, remembered bool) {
		changes = append(changes, key+"/"+map[bool]string{true: "allow", false: "deny"}[allow]+"/"+map[bool]string{true: "kept", false: "gone"}[remembered])
	}}
	req := func(c protocol.ToolCall) Request { return Request{Call: c} }
	if ans, err := m.Approve(ctx, req(readCall)); !ans.Allowed || err != nil || asked != 1 || ans.By != "person" || ans.Verdict != Yes {
		t.Fatalf("%+v %v %d", ans, err, asked)
	}
	if ans, _ := m.Approve(ctx, req(readCall)); !ans.Allowed || asked != 2 {
		t.Fatal("yes must not be remembered")
	}
	if ans, _ := m.Approve(ctx, req(readCall)); !ans.Allowed || asked != 2 || ans.By != "memory" {
		t.Fatalf("always must be remembered: %+v", ans)
	}
	if ans, _ := m.Approve(ctx, req(bashCall)); ans.Allowed || asked != 3 {
		t.Fatal("no")
	}
	if ans, _ := m.Approve(ctx, req(bashCall)); ans.Allowed || asked != 4 {
		t.Fatal("no must not be remembered")
	}
	var denied *Denied
	if ans, err := m.Approve(ctx, req(bashCall)); ans.Allowed || asked != 4 || !errors.As(err, &denied) || !strings.Contains(err.Error(), "never") || ans.By != "memory" {
		t.Fatalf("%+v %v", ans, err)
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
	if strings.Join(changes, " ") != "read/allow/kept bash/deny/kept bash/deny/gone other/deny/kept read/deny/kept" {
		t.Fatalf("%v", changes)
	}
	exported := m.Export()
	fresh := &Memory{}
	fresh.Import(exported)
	if len(exported) != 2 || exported["read"] || strings.Join(fresh.Denied(), ",") != "other,read" {
		t.Fatalf("%v %v", exported, fresh.Denied())
	}
	byKey := &Memory{Key: func(c protocol.ToolCall) string { return c.Name + ":" + string(c.Args) }, Ask: AskerFunc(func(context.Context, Request) (Verdict, error) { return Always, nil })}
	byKey.Approve(ctx, req(readCall))
	if byKey.Allowed()[0] != `read:{"path":"a"}` {
		t.Fatalf("%v", byKey.Allowed())
	}
	byRequest := &Memory{Ask: AskerFunc(func(_ context.Context, r Request) (Verdict, error) {
		if r.Key != "read(a)" {
			t.Fatalf("request key must reach the asker: %q", r.Key)
		}
		return Always, nil
	})}
	byRequest.Approve(ctx, Request{Call: readCall, Key: "read(a)"})
	if byRequest.Allowed()[0] != "read(a)" {
		t.Fatalf("%v", byRequest.Allowed())
	}
	if _, err := (&Memory{}).Approve(ctx, req(readCall)); !errors.As(err, &denied) {
		t.Fatal("memory without asker must deny")
	}
	boom := errors.New("ui gone")
	if _, err := (&Memory{Ask: AskerFunc(func(context.Context, Request) (Verdict, error) { return No, boom })}).Approve(ctx, req(readCall)); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	for v, want := range map[Verdict]string{Yes: "yes", No: "no", Always: "always", Never: "never"} {
		if v.String() != want || v.Allowed() != (v == Yes || v == Always) {
			t.Fatalf("%v", v)
		}
	}
}

func TestAdapters(t *testing.T) {
	ctx := context.Background()
	asker := AskerFunc(func(_ context.Context, r Request) (Verdict, error) {
		if r.Call.Name == "read" {
			return Always, nil
		}
		return No, nil
	})
	ans, err := FromAsker(asker).Approve(ctx, Request{Call: readCall})
	if err != nil || !ans.Allowed || ans.By != "person" || ans.Verdict != Always {
		t.Fatalf("%+v %v", ans, err)
	}
	if ans, _ := FromAsker(asker).Approve(ctx, Request{Call: bashCall}); ans.Allowed {
		t.Fatal("no must refuse")
	}
	approver := ApproverFunc(func(_ context.Context, r Request) (Answer, error) {
		switch r.Call.Name {
		case "read":
			return Answer{Allowed: true, By: "judge"}, nil
		case "bash":
			return Answer{By: "judge"}, &Denied{Call: r.Call, Reason: "judge said no"}
		}
		return Answer{}, errors.New("judge down")
	})
	if v, err := ToAsker(approver).Ask(ctx, Request{Call: readCall}); err != nil || v != Yes {
		t.Fatalf("%v %v", v, err)
	}
	if v, err := ToAsker(approver).Ask(ctx, Request{Call: bashCall}); err != nil || v != No {
		t.Fatalf("a denial must become a no without an error: %v %v", v, err)
	}
	if _, err := ToAsker(approver).Ask(ctx, Request{Call: otherCall}); err == nil {
		t.Fatal("other errors must propagate")
	}
	boom := errors.New("ui gone")
	if _, err := FromAsker(AskerFunc(func(context.Context, Request) (Verdict, error) { return No, boom })).Approve(ctx, Request{}); !errors.Is(err, boom) {
		t.Fatal("asker errors must propagate")
	}
	m := &Memory{Ask: ToAsker(approver)}
	if ans, err := m.Approve(ctx, Request{Call: readCall}); err != nil || !ans.Allowed {
		t.Fatalf("memory over an approver: %+v %v", ans, err)
	}
}

func TestGateResolveAndStricter(t *testing.T) {
	ctx := context.Background()
	var denied *Denied
	var entries []Entry
	rec := RecorderFunc(func(e Entry) { entries = append(entries, e) })
	asked := 0
	approver := ApproverFunc(func(context.Context, Request) (Answer, error) {
		asked++
		return Answer{Allowed: true, By: "person"}, nil
	})
	if err := (Gate{Policy: AskAll, Recorder: rec}).Resolve(ctx, readCall, Allow); err != nil || entries[0].Decision != Allow || entries[0].Reason != "allowed by settled decision" {
		t.Fatalf("%v %+v", err, entries)
	}
	if err := (Gate{Policy: AllowAll, Approver: approver, Recorder: rec}).Resolve(ctx, readCall, Ask); err != nil || asked != 1 || entries[1].Decision != Ask || entries[1].Reason != "approved by person" || entries[1].By != "person" {
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
	rejecting := ApproverFunc(func(context.Context, Request) (Answer, error) { return Answer{By: "judge", Reason: "risky"}, nil })
	if err := (Gate{Policy: AskAll, Approver: rejecting}).Check(ctx, readCall); !errors.As(err, &denied) || denied.Reason != "rejected by judge" {
		t.Fatalf("%v", err)
	}
	for _, c := range []struct{ a, b, want Decision }{
		{"", Allow, Allow}, {Allow, "", Allow}, {Allow, Ask, Ask}, {Ask, Allow, Ask}, {Ask, Deny, Deny}, {Deny, Ask, Deny}, {"", "", ""}, {Allow, Allow, Allow},
	} {
		if got := Stricter(c.a, c.b); got != c.want {
			t.Errorf("Stricter(%q, %q) = %q", c.a, c.b, got)
		}
	}
}
