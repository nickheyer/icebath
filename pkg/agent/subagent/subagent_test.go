package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

type script struct {
	responses []protocol.Response
	delay     time.Duration
	err       error
}

func (s *script) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-ctx.Done():
				yield(protocol.Event{}, ctx.Err())
				return
			}
		}
		if s.err != nil {
			yield(protocol.Event{}, s.err)
			return
		}
		resp := s.responses[0]
		s.responses = s.responses[1:]
		yield(protocol.Event{Type: protocol.EventDone, Response: resp}, nil)
	}
}

func text(s string) protocol.Response {
	return protocol.Response{Content: []protocol.Part{protocol.Text{Text: s}}, StopReason: protocol.StopEnd}
}

func call(name, args string) protocol.Response {
	return protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: name, Args: json.RawMessage(args)}}, StopReason: protocol.StopToolUse}
}

func TestDelegation(t *testing.T) {
	childModel := &script{responses: []protocol.Response{text("child says hi")}}
	var registry tool.Registry
	st := &Tool{Factory: func(ctx context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: childModel, ModelID: "m", Session: session.New(), Gate: policy.Gate{Policy: policy.AllowAll}}, nil
	}}
	if err := registry.Add(st); err != nil {
		t.Fatal(err)
	}
	if st.Definition().Name != "Agent" || !strings.Contains(string(st.Definition().Parameters), `"prompt"`) || !tool.Describe(st).Destructive {
		t.Fatalf("%+v", st.Definition())
	}
	parent := &agent.Agent{Name: "root", Model: &script{responses: []protocol.Response{call("Agent", `{"prompt":"greet","description":"greeting"}`), text("parent done")}}, ModelID: "m", Tools: &registry, Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}
	var seen []string
	resp, err := parent.Run(context.Background(), []protocol.Part{protocol.Text{Text: "go"}}, func(e agent.Event) {
		seen = append(seen, e.Agent+":"+string(e.Type))
	})
	if err != nil || agent.Text(resp) != "parent done" {
		t.Fatalf("%v %+v", err, resp)
	}
	res := parent.Session.Snapshot()[2].Content[0].(protocol.ToolResult)
	if res.IsError || res.Content[0].(protocol.Text).Text != "child says hi" {
		t.Fatalf("%+v", res)
	}
	want := "root:run_start,root:turn_start,root:model,root:turn_end,root:tool_start,root:subagent_start,Agent-1:run_start,Agent-1:turn_start,Agent-1:model,Agent-1:turn_end,Agent-1:run_end,root:subagent_end,root:tool_end,root:turn_start,root:model,root:turn_end,root:run_end"
	if strings.Join(seen, ",") != want {
		t.Fatalf("%v", seen)
	}
	children := st.Children()
	if len(children) != 1 || children[0].Name != "Agent-1" || children[0].Parent != "root" || children[0].Depth != 1 || children[0].CallID != "c" || children[0].Running || children[0].Answer != "child says hi" || children[0].Description != "greeting" || children[0].Agent.Session.Len() != 2 {
		t.Fatalf("%+v", children)
	}
	if c, ok := st.Child("Agent-1"); !ok || c.Finished.IsZero() {
		t.Fatal("child by name")
	}
	if err := st.Steer("Agent-1", protocol.Text{Text: "x"}); err == nil || !strings.Contains(err.Error(), "finished") {
		t.Fatalf("%v", err)
	}
	if err := st.Cancel("nope"); err == nil {
		t.Fatal("unknown child cancelled")
	}
}

func TestChildEventsCarryLineage(t *testing.T) {
	var registry tool.Registry
	st := &Tool{Factory: func(ctx context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: &script{responses: []protocol.Response{text("ok")}}, ModelID: "m", Session: session.New()}, nil
	}}
	registry.Add(st)
	parent := &agent.Agent{Name: "root", Model: &script{responses: []protocol.Response{call("Agent", `{"prompt":"x"}`), text("done")}}, ModelID: "m", Tools: &registry, Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}
	var childEvents []agent.Event
	var starts, ends []agent.Event
	if _, err := parent.Run(context.Background(), nil, func(e agent.Event) {
		switch {
		case e.Agent == "Agent-1":
			childEvents = append(childEvents, e)
		case e.Type == agent.EventSubagentStart:
			starts = append(starts, e)
		case e.Type == agent.EventSubagentEnd:
			ends = append(ends, e)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(childEvents) == 0 || childEvents[0].Parent != "root" || childEvents[0].Depth != 1 {
		t.Fatalf("%+v", childEvents)
	}
	if len(starts) != 1 || starts[0].Call.Name != "Agent-1" || starts[0].Call.ID != "c" || starts[0].Data.(Child).Running != true {
		t.Fatalf("%+v", starts)
	}
	if len(ends) != 1 || ends[0].Err != nil || ends[0].Response == nil || ends[0].Data.(Child).Running {
		t.Fatalf("%+v", ends)
	}
}

func TestNamesStayUniqueAndChildrenArePruned(t *testing.T) {
	ctx := context.Background()
	st := &Tool{MaxChildren: 2, Factory: func(ctx context.Context) (*agent.Agent, error) {
		return &agent.Agent{Name: "kid", Model: &script{responses: []protocol.Response{text("fine")}}, ModelID: "m", Session: session.New()}, nil
	}}
	for range 4 {
		if res, _ := st.Run(ctx, json.RawMessage(`{"prompt":"x"}`)); res.IsError {
			t.Fatalf("%+v", res)
		}
	}
	children := st.Children()
	if len(children) != 2 || children[0].Name != "kid-3" || children[1].Name != "kid-4" {
		t.Fatalf("%+v", children)
	}
	if !tool.Describe(st).Destructive || tool.Describe(st).ReadOnly {
		t.Fatal("serial children are not read only")
	}
	if !tool.Describe(&Tool{Parallel: true}).ReadOnly {
		t.Fatal("parallel children run as read only tools")
	}
}

func TestCancelStopsARunningChild(t *testing.T) {
	ctx := context.Background()
	st := &Tool{Factory: func(ctx context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: &script{responses: []protocol.Response{text("late")}, delay: 5 * time.Second}, ModelID: "m", Session: session.New()}, nil
	}}
	done := make(chan tool.Result, 1)
	go func() {
		res, _ := st.Run(ctx, json.RawMessage(`{"prompt":"x"}`))
		done <- res
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if c, ok := st.Child("Agent-1"); ok && c.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := st.Steer("Agent-1", protocol.Text{Text: "hurry"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Cancel("Agent-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "failed") {
			t.Fatalf("%+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop the child")
	}
	if c, _ := st.Child("Agent-1"); c.Running || c.Err == nil {
		t.Fatalf("%+v", c)
	}
}

func TestLimitsAndFailures(t *testing.T) {
	ctx := context.Background()
	run := func(st *Tool, args string) (string, bool) {
		t.Helper()
		res, err := st.Run(ctx, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(protocol.Text).Text, res.IsError
	}
	if out, isErr := run(&Tool{}, `{"prompt":"x"}`); !isErr || !strings.Contains(out, "no factory") {
		t.Fatal(out)
	}
	ok := func(ctx context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: &script{responses: []protocol.Response{text("fine")}}, ModelID: "m", Session: session.New()}, nil
	}
	st := &Tool{Name: "Delegate", Factory: ok, MaxDepth: 1}
	for args, want := range map[string]string{`{"prompt":" "}`: "required", `{"prompt":1}`: "cannot unmarshal"} {
		if out, isErr := run(st, args); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %q", args, out)
		}
	}
	if out, isErr := run(st, `{"prompt":"x"}`); isErr || out != "fine" {
		t.Fatal(out)
	}
	deep := agent.WithLineage(ctx, "root", 1)
	res, _ := st.Run(deep, json.RawMessage(`{"prompt":"x"}`))
	if !res.IsError || !strings.Contains(res.Content[0].(protocol.Text).Text, "deeper than 1") || Depth(deep) != 1 || Depth(ctx) != 0 {
		t.Fatalf("%+v", res)
	}
	st = &Tool{Factory: func(context.Context) (*agent.Agent, error) { return nil, errors.New("no model") }}
	if out, isErr := run(st, `{"prompt":"x"}`); !isErr || !strings.Contains(out, "no model") {
		t.Fatal(out)
	}
	st = &Tool{Factory: func(context.Context) (*agent.Agent, error) { return nil, nil }}
	if out, isErr := run(st, `{"prompt":"x"}`); !isErr || !strings.Contains(out, "no agent") {
		t.Fatal(out)
	}
	st = &Tool{Factory: func(context.Context) (*agent.Agent, error) {
		return &agent.Agent{Name: "kid", Model: &script{err: errors.New("down")}, ModelID: "m", Session: session.New()}, nil
	}}
	if out, isErr := run(st, `{"prompt":"x"}`); !isErr || !strings.Contains(out, "kid failed: down") {
		t.Fatal(out)
	}
	st = &Tool{Factory: func(context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: &script{responses: []protocol.Response{{StopReason: protocol.StopEnd}}}, ModelID: "m", Session: session.New()}, nil
	}}
	if out, isErr := run(st, `{"prompt":"x"}`); !isErr || !strings.Contains(out, "without a text answer") {
		t.Fatal(out)
	}
	st = &Tool{Factory: func(context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: &script{responses: []protocol.Response{text("late")}, delay: time.Second}, ModelID: "m", Session: session.New()}, nil
	}, Timeout: 20 * time.Millisecond}
	if out, isErr := run(st, `{"prompt":"x"}`); !isErr || !strings.Contains(out, "timed out") {
		t.Fatal(out)
	}
}
