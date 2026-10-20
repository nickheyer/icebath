package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Scripted model that replays one response per turn and records every request it saw
type script struct {
	mu        sync.Mutex
	responses []protocol.Response
	requests  []protocol.Request
	err       error
	onTurn    func(turn int)
}

func (s *script) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		s.mu.Lock()
		s.requests = append(s.requests, req)
		turn := len(s.requests)
		s.mu.Unlock()
		if s.onTurn != nil {
			s.onTurn(turn)
		}
		if s.err != nil {
			yield(protocol.Event{}, s.err)
			return
		}
		if len(s.responses) == 0 {
			return
		}
		resp := s.responses[0]
		s.responses = s.responses[1:]
		if !yield(protocol.Event{Type: protocol.EventStart}, nil) {
			return
		}
		yield(protocol.Event{Type: protocol.EventDone, Response: resp}, nil)
	}
}

func callTurn(id, name, args string) protocol.Response {
	return protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}}, StopReason: protocol.StopToolUse, Usage: protocol.Usage{Input: 1, Output: 1}}
}

func textTurn(s string) protocol.Response {
	return protocol.Response{Content: []protocol.Part{protocol.Text{Text: s}}, StopReason: protocol.StopEnd, Usage: protocol.Usage{Input: 1, Output: 1}}
}

func registry(t *testing.T) *tool.Registry {
	t.Helper()
	var r tool.Registry
	err := r.Add(tool.Func{Name: "add", Description: "Adds", Parameters: json.RawMessage(`{"type":"object"}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
		var in struct{ A, B int }
		if err := json.Unmarshal(args, &in); err != nil {
			return tool.Result{}, err
		}
		Observe(ctx, Event{Type: EventNote, Text: "adding"})
		return tool.Text(json.Number(string(rune('0' + in.A + in.B))).String()), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func types(events []Event) string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = string(e.Type)
	}
	return strings.Join(out, ",")
}

func TestRunLoopsThroughToolRounds(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":2}`), textTurn("three")}}
	sess := session.New()
	a := &Agent{Name: "root", Model: model, ModelID: "m", System: "sys", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess, MaxTokens: 9}
	var events []Event
	resp, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "1+2?"}}, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if Text(resp) != "three" {
		t.Fatalf("%+v", resp)
	}
	if types(events) != "model,model,tool_start,note,tool_end,model,model" {
		t.Fatalf("%v", types(events))
	}
	for _, e := range events {
		if e.Agent != "root" {
			t.Fatalf("events must name the agent: %+v", e)
		}
	}
	msgs := sess.Snapshot()
	if len(msgs) != 4 || msgs[0].Role != protocol.RoleUser || msgs[1].Role != protocol.RoleAssistant || msgs[2].Role != protocol.RoleTool || msgs[3].Role != protocol.RoleAssistant {
		t.Fatalf("%+v", msgs)
	}
	res := msgs[2].Content[0].(protocol.ToolResult)
	if res.CallID != "c1" || res.IsError || res.Content[0].(protocol.Text).Text != "3" {
		t.Fatalf("%+v", res)
	}
	if sess.Usage != (protocol.Usage{Input: 2, Output: 2}) {
		t.Fatalf("%+v", sess.Usage)
	}
	second := model.requests[1]
	if second.Model != "m" || second.System != "sys" || second.MaxTokens != 9 || !second.Stream || len(second.Tools) != 1 || len(second.Messages) != 3 {
		t.Fatalf("%+v", second)
	}
	if a.Running() {
		t.Fatal("running after return")
	}
}

func TestDeniedCallsBecomeErrorResults(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":1}`), textTurn("ok")}}
	sess := session.New()
	a := &Agent{Model: model, ModelID: "m", Tools: registry(t), Session: sess}
	var ends []Event
	if _, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "go"}}, func(e Event) {
		if e.Type == EventToolEnd {
			ends = append(ends, e)
		}
	}); err != nil {
		t.Fatal(err)
	}
	res := sess.Snapshot()[2].Content[0].(protocol.ToolResult)
	if !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "no approver") {
		t.Fatalf("%+v", res)
	}
	var denied *policy.Denied
	if len(ends) != 1 || !errors.As(ends[0].Err, &denied) {
		t.Fatalf("%+v", ends)
	}
}

func TestRunStopsOnErrorsAndLimits(t *testing.T) {
	boom := errors.New("down")
	a := &Agent{Model: &script{err: boom}, ModelID: "m", Session: session.New()}
	if _, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "x"}}, nil); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	a = &Agent{Model: &script{}, ModelID: "m", Session: session.New()}
	if _, err := a.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "without a done event") {
		t.Fatalf("%v", err)
	}
	a = &Agent{Model: &script{responses: []protocol.Response{{StopReason: protocol.StopToolUse}}}, ModelID: "m", Session: session.New()}
	if _, err := a.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "no tool calls") {
		t.Fatalf("%v", err)
	}
	endless := &script{responses: []protocol.Response{callTurn("1", "add", `{}`), callTurn("2", "add", `{}`), callTurn("3", "add", `{}`)}}
	a = &Agent{Model: endless, ModelID: "m", Session: session.New(), Gate: policy.Gate{Policy: policy.AllowAll}, MaxRounds: 2}
	if _, err := a.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "2 tool rounds") {
		t.Fatalf("%v", err)
	}
	if _, err := (&Agent{}).Run(context.Background(), nil, nil); err == nil {
		t.Fatal("unconfigured agent ran")
	}
}

func TestMissingRegistryYieldsUnknownToolResults(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "nope", `{}`), textTurn("ok")}}
	sess := session.New()
	a := &Agent{Model: model, ModelID: "m", Session: sess, Gate: policy.Gate{Policy: policy.AllowAll}}
	if _, err := a.Run(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	res := sess.Snapshot()[1].Content[0].(protocol.ToolResult)
	if !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "unknown tool") {
		t.Fatalf("%+v", res)
	}
}

func TestSteeringReachesTheModel(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":1}`), textTurn("two"), textTurn("bye")}}
	sess := session.New()
	a := &Agent{Model: model, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess}
	model.onTurn = func(turn int) {
		switch turn {
		case 1:
			a.Steer(protocol.Text{Text: "also say hi"})
		case 2:
			a.Steer(protocol.Text{Text: "and bye"})
		}
	}
	var steered []Event
	resp, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "1+1"}}, func(e Event) {
		if e.Type == EventSteer {
			steered = append(steered, e)
		}
	})
	if err != nil || Text(resp) != "bye" {
		t.Fatalf("%v %+v", err, resp)
	}
	var roles []string
	for _, m := range sess.Snapshot() {
		roles = append(roles, string(m.Role))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user,assistant,user,assistant" {
		t.Fatalf("%v", roles)
	}
	if len(steered) != 2 || steered[0].Parts[0].(protocol.Text).Text != "also say hi" || steered[1].Parts[0].(protocol.Text).Text != "and bye" {
		t.Fatalf("%+v", steered)
	}
	if len(model.requests[1].Messages) != 4 || model.requests[1].Messages[3].Content[0].(protocol.Text).Text != "also say hi" {
		t.Fatalf("%+v", model.requests[1].Messages)
	}
}

type recorder struct {
	Hooks
	log []string
}

func TestHooksWrapEveryStep(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":1}`), textTurn("two"), textTurn("done")}}
	sess := session.New()
	rec := &recorder{}
	rec.BeforeRunFn = func(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
		rec.log = append(rec.log, "run")
		return append(input, protocol.Text{Text: "(hooked)"}), nil
	}
	rec.BeforeTurnFn = func(ctx context.Context, req *protocol.Request) error {
		rec.log = append(rec.log, "turn")
		req.System += " +hook"
		return nil
	}
	rec.BeforeToolFn = func(ctx context.Context, call protocol.ToolCall) (protocol.ToolCall, policy.Decision, error) {
		rec.log = append(rec.log, "before "+call.Name)
		call.Args = json.RawMessage(`{"a":2,"b":2}`)
		return call, "", nil
	}
	rec.AfterToolFn = func(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
		rec.log = append(rec.log, "after "+call.Name)
		res.Content = append(res.Content, protocol.Text{Text: " (checked)"})
		return res, nil
	}
	stops := 0
	rec.OnStopFn = func(ctx context.Context, resp protocol.Response) ([]protocol.Part, error) {
		rec.log = append(rec.log, "stop")
		stops++
		if stops == 1 {
			return []protocol.Part{protocol.Text{Text: "one more"}}, nil
		}
		return nil, nil
	}
	a := &Agent{Model: model, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess, Hooks: []Hook{rec}}
	resp, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "1+1"}}, nil)
	if err != nil || Text(resp) != "done" {
		t.Fatalf("%v %+v", err, resp)
	}
	if strings.Join(rec.log, ",") != "run,turn,before add,after add,turn,stop,turn,stop" {
		t.Fatalf("%v", rec.log)
	}
	msgs := sess.Snapshot()
	if msgs[0].Content[1].(protocol.Text).Text != "(hooked)" || model.requests[0].System != " +hook" {
		t.Fatal("before run and before turn rewrites")
	}
	res := msgs[2].Content[0].(protocol.ToolResult)
	if res.Content[0].(protocol.Text).Text != "4" || res.Content[1].(protocol.Text).Text != " (checked)" {
		t.Fatalf("%+v", res)
	}
	if msgs[4].Content[0].(protocol.Text).Text != "one more" {
		t.Fatalf("%+v", msgs[4])
	}
}

func TestHookFailures(t *testing.T) {
	boom := errors.New("hook says no")
	blockRun := Hooks{BeforeRunFn: func(context.Context, []protocol.Part) ([]protocol.Part, error) { return nil, boom }}
	a := &Agent{Model: &script{responses: []protocol.Response{textTurn("x")}}, ModelID: "m", Session: session.New(), Hooks: []Hook{blockRun}}
	if _, err := a.Run(context.Background(), nil, nil); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	blockTurn := Hooks{BeforeTurnFn: func(context.Context, *protocol.Request) error { return boom }}
	a = &Agent{Model: &script{responses: []protocol.Response{textTurn("x")}}, ModelID: "m", Session: session.New(), Hooks: []Hook{blockTurn}}
	if _, err := a.Run(context.Background(), nil, nil); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	blockTool := Hooks{BeforeToolFn: func(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, policy.Decision, error) {
		return c, "", boom
	}}
	sess := session.New()
	var ends []Event
	a = &Agent{Model: &script{responses: []protocol.Response{callTurn("c1", "add", `{}`), textTurn("x")}}, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess, Hooks: []Hook{blockTool}}
	if _, err := a.Run(context.Background(), nil, func(e Event) {
		if e.Type == EventToolEnd {
			ends = append(ends, e)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if res := sess.Snapshot()[1].Content[0].(protocol.ToolResult); !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "hook says no") || !errors.Is(ends[0].Err, boom) {
		t.Fatalf("%+v %+v", res, ends)
	}
	failAfter := Hooks{AfterToolFn: func(_ context.Context, _ protocol.ToolCall, r protocol.ToolResult) (protocol.ToolResult, error) {
		return r, boom
	}}
	sess = session.New()
	a = &Agent{Model: &script{responses: []protocol.Response{callTurn("c1", "add", `{}`), textTurn("x")}}, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess, Hooks: []Hook{failAfter}}
	if _, err := a.Run(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if res := sess.Snapshot()[1].Content[0].(protocol.ToolResult); !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "hook says no") {
		t.Fatalf("%+v", res)
	}
	failStop := Hooks{OnStopFn: func(context.Context, protocol.Response) ([]protocol.Part, error) { return nil, boom }}
	a = &Agent{Model: &script{responses: []protocol.Response{textTurn("x")}}, ModelID: "m", Session: session.New(), Hooks: []Hook{failStop}}
	if resp, err := a.Run(context.Background(), nil, nil); !errors.Is(err, boom) || Text(resp) != "x" {
		t.Fatalf("%v %+v", err, resp)
	}
	forever := Hooks{OnStopFn: func(context.Context, protocol.Response) ([]protocol.Part, error) {
		return []protocol.Part{protocol.Text{Text: "again"}}, nil
	}}
	a = &Agent{Model: &script{responses: []protocol.Response{textTurn("1"), textTurn("2"), textTurn("3")}}, ModelID: "m", Session: session.New(), Hooks: []Hook{forever}, MaxRounds: 2}
	if _, err := a.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "2 rounds") {
		t.Fatalf("%v", err)
	}
}

func TestRunRefusesToOverlap(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	model := &script{responses: []protocol.Response{textTurn("x")}}
	model.onTurn = func(int) {
		close(started)
		<-release
	}
	a := &Agent{Model: model, ModelID: "m", Session: session.New()}
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), nil, nil)
		done <- err
	}()
	<-started
	if !a.Running() {
		t.Fatal("not running")
	}
	if _, err := a.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("%v", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run never finished")
	}
	Observe(context.Background(), Event{Type: EventNote})
}

func TestHooksSettlePermissions(t *testing.T) {
	settle := func(d policy.Decision) Hook {
		return Hooks{BeforeToolFn: func(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, policy.Decision, error) {
			return c, d, nil
		}}
	}
	asked := 0
	approver := policy.ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { asked++; return true, nil })
	run := func(gate policy.Gate, hooks ...Hook) protocol.ToolResult {
		sess := session.New()
		a := &Agent{Model: &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":2}`), textTurn("x")}}, ModelID: "m", Tools: registry(t), Gate: gate, Session: sess, Hooks: hooks}
		if _, err := a.Run(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
		return sess.Snapshot()[1].Content[0].(protocol.ToolResult)
	}
	if res := run(policy.Gate{Policy: policy.AskAll}, settle(policy.Allow)); res.IsError {
		t.Fatalf("a hook allow must run the tool without an approver: %+v", res)
	}
	if res := run(policy.Gate{Policy: policy.AllowAll, Approver: approver}, settle(policy.Ask)); res.IsError || asked != 1 {
		t.Fatalf("a hook ask must consult the approver: %+v %d", res, asked)
	}
	if res := run(policy.Gate{Policy: policy.DenyAll}, settle(policy.Allow)); !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "denied by policy") {
		t.Fatalf("a hook allow must never beat a policy deny: %+v", res)
	}
	if res := run(policy.Gate{Policy: policy.AllowAll, Approver: approver}, settle(policy.Allow), settle(policy.Ask)); res.IsError || asked != 2 {
		t.Fatalf("the strictest hook decision wins: %+v %d", res, asked)
	}
}
