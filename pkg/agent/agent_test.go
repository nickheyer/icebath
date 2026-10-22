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
		return tool.Text(json.Number(string(rune('0' + in.A + in.B))).String()).With("sum"), nil
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
	if types(events) != "run_start,turn_start,model,model,turn_end,tool_start,note,tool_end,turn_start,model,model,turn_end,run_end" {
		t.Fatalf("%v", types(events))
	}
	last := int64(0)
	for i, e := range events {
		if e.Agent != "root" || e.Seq <= last || e.Time.IsZero() {
			t.Fatalf("event %d must be stamped: %+v", i, e)
		}
		last = e.Seq
	}
	if events[1].Round != 1 || events[8].Round != 2 || events[0].Parts[0].(protocol.Text).Text != "1+2?" {
		t.Fatalf("rounds %d %d", events[1].Round, events[8].Round)
	}
	toolEnd := events[7]
	if toolEnd.Data != "sum" || toolEnd.Result.CallID != "c1" || toolEnd.Duration < 0 {
		t.Fatalf("%+v", toolEnd)
	}
	turnEnd := events[4]
	if turnEnd.Response == nil || turnEnd.Usage.Input != 1 {
		t.Fatalf("%+v", turnEnd)
	}
	runEnd := events[12]
	if runEnd.Response == nil || Text(*runEnd.Response) != "three" || runEnd.Usage != (protocol.Usage{Input: 2, Output: 2}) || runEnd.Err != nil {
		t.Fatalf("%+v", runEnd)
	}
	msgs := sess.Snapshot()
	if len(msgs) != 4 || msgs[0].Role != protocol.RoleUser || msgs[1].Role != protocol.RoleAssistant || msgs[2].Role != protocol.RoleTool || msgs[3].Role != protocol.RoleAssistant {
		t.Fatalf("%+v", msgs)
	}
	res := msgs[2].Content[0].(protocol.ToolResult)
	if res.CallID != "c1" || res.IsError || res.Content[0].(protocol.Text).Text != "3" {
		t.Fatalf("%+v", res)
	}
	if sess.Usage() != (protocol.Usage{Input: 2, Output: 2}) || sess.Model() != "m" {
		t.Fatalf("%+v %s", sess.Usage(), sess.Model())
	}
	entries := sess.Entries()
	if entries[1].Agent != "root" || entries[2].Agent != "root" || entries[0].Agent != "" {
		t.Fatalf("entries must name the agent: %+v", entries)
	}
	records := sess.Records()
	if len(records) != 2 || records[0].Kind != "run_start" || records[1].Kind != "run_end" {
		t.Fatalf("%+v", records)
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

func TestApprovalsAreEventsAndRecords(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":1}`), textTurn("ok")}}
	sess := session.New()
	var asks []policy.Request
	asker := policy.AskerFunc(func(_ context.Context, req policy.Request) (policy.Verdict, error) {
		asks = append(asks, req)
		return policy.Always, nil
	})
	a := &Agent{Name: "root", Model: model, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AskAll, Approver: &policy.Memory{Ask: asker}}, Session: sess}
	var approvals []Event
	if _, err := a.Run(context.Background(), nil, func(e Event) {
		if e.Type == EventApproval {
			approvals = append(approvals, e)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 2 || !approvals[0].Approval.Pending || approvals[1].Approval.Pending || !approvals[1].Approval.Answer.Allowed || approvals[1].Approval.Answer.Verdict != policy.Always || approvals[1].Call.ID != "c1" {
		t.Fatalf("%+v", approvals)
	}
	if len(asks) != 1 || asks[0].Agent != "root" || asks[0].Tool == nil || asks[0].Tool.Description != "Adds" || !asks[0].Hints.ReadOnly {
		t.Fatalf("the gate must describe the tool from the registry: %+v", asks)
	}
	var kinds []string
	for _, r := range sess.Records() {
		kinds = append(kinds, r.Kind)
	}
	if strings.Join(kinds, ",") != "run_start,approval,run_end" {
		t.Fatalf("%v", kinds)
	}
}

func TestRunStopsOnErrorsAndLimits(t *testing.T) {
	boom := errors.New("down")
	sess := session.New()
	a := &Agent{Model: &script{err: boom}, ModelID: "m", Session: sess}
	if _, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "x"}}, nil); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if sess.Len() != 0 {
		t.Fatal("a failed first turn must roll the input back out of the session")
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
	sess = session.New()
	a = &Agent{Model: endless, ModelID: "m", Session: sess, Gate: policy.Gate{Policy: policy.AllowAll}, MaxRounds: 2}
	if _, err := a.Run(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "2 tool rounds") {
		t.Fatalf("%v", err)
	}
	msgs := sess.Snapshot()
	if msgs[len(msgs)-1].Role != protocol.RoleTool || !msgs[len(msgs)-1].Content[0].(protocol.ToolResult).IsError {
		t.Fatal("the round limit must leave an answered tool call behind")
	}
	if _, err := (&Agent{}).Run(context.Background(), nil, nil); err == nil {
		t.Fatal("unconfigured agent ran")
	}
	later := &script{responses: []protocol.Response{callTurn("1", "add", `{"a":1,"b":1}`)}, err: nil}
	sess = session.New()
	a = &Agent{Model: later, ModelID: "m", Tools: registry(t), Session: sess, Gate: policy.Gate{Policy: policy.AllowAll}}
	later.onTurn = func(turn int) {
		if turn == 2 {
			later.err = boom
		}
	}
	if _, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "x"}}, nil); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if sess.Len() != 3 {
		t.Fatalf("a later failure keeps the resumable transcript, got %d", sess.Len())
	}
}

func TestTruncationIsDetectedFromUsage(t *testing.T) {
	cut := protocol.Response{Content: []protocol.Part{protocol.Text{Text: "partial"}}, StopReason: protocol.StopEnd, Usage: protocol.Usage{Output: 5}}
	a := &Agent{Model: &script{responses: []protocol.Response{cut}}, ModelID: "m", Session: session.New(), MaxTokens: 5}
	resp, err := a.Run(context.Background(), nil, nil)
	if err != nil || resp.StopReason != protocol.StopLength {
		t.Fatalf("%v %+v", err, resp)
	}
	a = &Agent{Model: &script{responses: []protocol.Response{textTurn("ok")}}, ModelID: "m", Session: session.New()}
	resp, _ = a.Run(context.Background(), nil, nil)
	if resp.StopReason != protocol.StopEnd {
		t.Fatal("natural stops stay")
	}
}

func TestInterruptStopsAfterTheCurrentStep(t *testing.T) {
	model := &script{responses: []protocol.Response{callTurn("c1", "add", `{"a":1,"b":1}`), textTurn("never")}}
	sess := session.New()
	a := &Agent{Model: model, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess}
	model.onTurn = func(int) { a.Interrupt() }
	var events []Event
	_, err := a.Run(context.Background(), nil, func(e Event) { events = append(events, e) })
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("%v", err)
	}
	if sess.Len() != 2 || len(model.requests) != 1 {
		t.Fatalf("the tool round must finish before the interrupt: %d %d", sess.Len(), len(model.requests))
	}
	if !strings.Contains(types(events), "tool_end,interrupt,run_end") {
		t.Fatalf("%v", types(events))
	}
	if events[len(events)-1].Err != ErrInterrupted {
		t.Fatal("run end carries the error")
	}
	if resp, err := a.Run(context.Background(), nil, nil); err != nil || Text(resp) != "never" {
		t.Fatalf("a new run must start clean: %v %+v", err, resp)
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
	if model.requests[0].MaxTokens != DefaultMaxTokens {
		t.Fatalf("default max tokens %d", model.requests[0].MaxTokens)
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
	rec.AfterTurnFn = func(ctx context.Context, resp protocol.Response) error {
		rec.log = append(rec.log, "after turn")
		return nil
	}
	rec.BeforeToolFn = func(ctx context.Context, call protocol.ToolCall) (Advice, error) {
		rec.log = append(rec.log, "before "+call.Name)
		call.Args = json.RawMessage(`{"a":2,"b":2}`)
		return Advice{Call: call, Context: "(context)"}, nil
	}
	rec.AfterToolFn = func(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
		rec.log = append(rec.log, "after "+call.Name)
		res.Content = append(res.Content, protocol.Text{Text: " (checked)"})
		return res, nil
	}
	var agains []bool
	rec.OnStopFn = func(ctx context.Context, resp protocol.Response, again bool) ([]protocol.Part, error) {
		rec.log = append(rec.log, "stop")
		agains = append(agains, again)
		if len(agains) == 1 {
			return []protocol.Part{protocol.Text{Text: "one more"}}, nil
		}
		return nil, nil
	}
	a := &Agent{Model: model, ModelID: "m", Tools: registry(t), Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess, Hooks: []Hook{rec}}
	var events []Event
	resp, err := a.Run(context.Background(), []protocol.Part{protocol.Text{Text: "1+1"}}, func(e Event) { events = append(events, e) })
	if err != nil || Text(resp) != "done" {
		t.Fatalf("%v %+v", err, resp)
	}
	if strings.Join(rec.log, ",") != "run,turn,after turn,before add,after add,turn,after turn,stop,turn,after turn,stop" {
		t.Fatalf("%v", rec.log)
	}
	if len(agains) != 2 || agains[0] || !agains[1] {
		t.Fatalf("stop hooks must learn they already continued once: %v", agains)
	}
	msgs := sess.Snapshot()
	if msgs[0].Content[1].(protocol.Text).Text != "(hooked)" || model.requests[0].System != " +hook" {
		t.Fatal("before run and before turn rewrites")
	}
	res := msgs[2].Content[0].(protocol.ToolResult)
	if res.Content[0].(protocol.Text).Text != "4" || res.Content[1].(protocol.Text).Text != " (checked)" || res.Content[2].(protocol.Text).Text != "(context)" {
		t.Fatalf("%+v", res)
	}
	if msgs[4].Content[0].(protocol.Text).Text != "one more" {
		t.Fatalf("%+v", msgs[4])
	}
	continued := 0
	for _, e := range events {
		if e.Type == EventSteer && e.Text == "continued by hook" {
			continued++
		}
	}
	if continued != 1 {
		t.Fatalf("hook continuations must be announced, got %d", continued)
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
	failAfterTurn := Hooks{AfterTurnFn: func(context.Context, protocol.Response) error { return boom }}
	a = &Agent{Model: &script{responses: []protocol.Response{textTurn("x")}}, ModelID: "m", Session: session.New(), Hooks: []Hook{failAfterTurn}}
	if resp, err := a.Run(context.Background(), nil, nil); !errors.Is(err, boom) || Text(resp) != "x" {
		t.Fatalf("%v %+v", err, resp)
	}
	blockTool := Hooks{BeforeToolFn: func(_ context.Context, c protocol.ToolCall) (Advice, error) {
		return Advice{Call: c}, boom
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
	failStop := Hooks{OnStopFn: func(context.Context, protocol.Response, bool) ([]protocol.Part, error) { return nil, boom }}
	a = &Agent{Model: &script{responses: []protocol.Response{textTurn("x")}}, ModelID: "m", Session: session.New(), Hooks: []Hook{failStop}}
	if resp, err := a.Run(context.Background(), nil, nil); !errors.Is(err, boom) || Text(resp) != "x" {
		t.Fatalf("%v %+v", err, resp)
	}
	forever := Hooks{OnStopFn: func(context.Context, protocol.Response, bool) ([]protocol.Part, error) {
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
	if l := LineageFrom(WithLineage(context.Background(), "root", 2)); l.Parent != "root" || l.Depth != 2 {
		t.Fatalf("%+v", l)
	}
}

func TestHooksSettlePermissions(t *testing.T) {
	settle := func(d policy.Decision) Hook {
		return Hooks{BeforeToolFn: func(_ context.Context, c protocol.ToolCall) (Advice, error) {
			return Advice{Call: c, Decision: d}, nil
		}}
	}
	asked := 0
	approver := policy.ApproverFunc(func(context.Context, policy.Request) (policy.Answer, error) {
		asked++
		return policy.Answer{Allowed: true}, nil
	})
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

func TestParallelToolEndsArriveAsTheyFinish(t *testing.T) {
	var r tool.Registry
	r.Add(
		tool.Func{Name: "slow", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(context.Context, json.RawMessage) (tool.Result, error) {
			time.Sleep(40 * time.Millisecond)
			return tool.Text("slow"), nil
		}},
		tool.Func{Name: "fast", Parameters: json.RawMessage(`{}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(context.Context, json.RawMessage) (tool.Result, error) {
			return tool.Text("fast"), nil
		}},
	)
	two := protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "a", Name: "slow", Args: json.RawMessage(`{}`)}, protocol.ToolCall{ID: "b", Name: "fast", Args: json.RawMessage(`{}`)}}, StopReason: protocol.StopToolUse}
	a := &Agent{Model: &script{responses: []protocol.Response{two, textTurn("ok")}}, ModelID: "m", Tools: &r, Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}
	var ends []string
	if _, err := a.Run(context.Background(), nil, func(e Event) {
		if e.Type == EventToolEnd {
			ends = append(ends, e.Call.ID)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ends, ",") != "b,a" {
		t.Fatalf("%v", ends)
	}
}

func TestRunTakesEveryRoundUnlessALimitIsSet(t *testing.T) {
	var turns []protocol.Response
	for i := range 80 {
		turns = append(turns, callTurn(string(rune('a'+i%26))+string(rune('0'+i/26)), "add", `{"a":1,"b":1}`))
	}
	turns = append(turns, textTurn("done"))
	a := &Agent{Model: &script{responses: turns}, ModelID: "m", Tools: registry(t), Session: session.New(), Gate: policy.Gate{Policy: policy.AllowAll}}
	resp, err := a.Run(context.Background(), nil, nil)
	if err != nil || resp.Text() != "done" {
		t.Fatalf("eighty tool rounds run to the end without a limit %v %q", err, resp.Text())
	}
}
