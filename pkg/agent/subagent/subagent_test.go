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
	if strings.Join(seen, ",") != "root:model,root:tool_start,root:note,Agent-1:model,root:tool_end,root:model" {
		t.Fatalf("%v", seen)
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
	deep := context.WithValue(ctx, depthKey{}, 1)
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
