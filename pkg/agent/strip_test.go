package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

func thinks(m protocol.Message) bool {
	for _, p := range m.Content {
		if _, ok := p.(protocol.Thinking); ok {
			return true
		}
	}
	return false
}

func TestStripThinkingReplaysOnlyThisRun(t *testing.T) {
	ctx := context.Background()
	think := protocol.Thinking{Text: "t", Signature: "s"}
	call := protocol.ToolCall{ID: "c1", Name: "Read", Args: []byte(`{}`)}
	history := func() []protocol.Message {
		return []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Part{think, protocol.Text{Text: "done"}}},
			{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "again"}}},
		}
	}
	s := &StripThinking{}
	if _, err := s.BeforeRun(ctx, nil); err != nil {
		t.Fatal(err)
	}
	req := protocol.Request{Messages: history()}
	if err := s.BeforeTurn(ctx, &req); err != nil {
		t.Fatal(err)
	}
	if thinks(req.Messages[1]) {
		t.Fatal("thinking of an earlier run replayed on the first turn")
	}
	if err := s.AfterTurn(ctx, protocol.Response{}); err != nil {
		t.Fatal(err)
	}
	req = protocol.Request{Messages: append(history(),
		protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{think, call}},
		protocol.Message{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c1", Content: []protocol.Part{protocol.Text{Text: "ok"}}}}},
	)}
	if err := s.BeforeTurn(ctx, &req); err != nil {
		t.Fatal(err)
	}
	if thinks(req.Messages[1]) || !thinks(req.Messages[3]) {
		t.Fatal("only the turn this run produced should keep its thinking")
	}
	if _, err := s.BeforeRun(ctx, nil); err != nil {
		t.Fatal(err)
	}
	req = protocol.Request{Messages: append(history(), protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{think, call}})}
	if err := s.BeforeTurn(ctx, &req); err != nil {
		t.Fatal(err)
	}
	if thinks(req.Messages[3]) {
		t.Fatal("a new run replayed the thinking of the last one")
	}
}

func TestStripThinkingDropsBlockOnChangedPrefix(t *testing.T) {
	ctx := context.Background()
	think := protocol.Thinking{Text: "t", Signature: "s"}
	call := protocol.ToolCall{ID: "c1", Name: "Read", Args: []byte(`{}`)}
	asked := []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}
	answered := append(append([]protocol.Message{}, asked...),
		protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{think, call}},
		protocol.Message{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c1", Content: []protocol.Part{protocol.Text{Text: "ok"}}}}},
	)
	s := &StripThinking{}
	if _, err := s.BeforeRun(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BeforeTurn(ctx, &protocol.Request{System: "a", Messages: asked}); err != nil {
		t.Fatal(err)
	}
	if err := s.AfterTurn(ctx, protocol.Response{}); err != nil {
		t.Fatal(err)
	}
	failed := protocol.Request{System: "a", Messages: append(answered, protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "more"}}})}
	if err := s.BeforeTurn(ctx, &failed); err != nil {
		t.Fatal(err)
	}
	same := protocol.Request{System: "a", Messages: answered}
	if err := s.BeforeTurn(ctx, &same); err != nil {
		t.Fatal(err)
	}
	if !thinks(same.Messages[1]) {
		t.Fatal("a request that got no answer replaced the one the thinking is bound to")
	}
	moved := protocol.Request{System: "b", Messages: answered}
	if err := s.BeforeTurn(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	if thinks(moved.Messages[1]) {
		t.Fatal("thinking resent under a different system prompt")
	}
}

// Model that answers each turn with the next scripted response and keeps every request it was sent
type scripted struct {
	replies []protocol.Response
	sent    []protocol.Request
}

func (m *scripted) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	m.sent = append(m.sent, req)
	resp := m.replies[len(m.sent)-1]
	return func(yield func(protocol.Event, error) bool) {
		yield(protocol.Event{Type: protocol.EventDone, Response: resp}, nil)
	}
}

func TestPruneMidRunDropsBoundThinking(t *testing.T) {
	sess := session.New()
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "start"}}})
	sess.Append(protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.ToolCall{ID: "c0", Name: "Read", Args: []byte(`{}`)}}})
	sess.Append(protocol.Message{Role: protocol.RoleTool, Content: []protocol.Part{protocol.ToolResult{CallID: "c0", Name: "Read", Content: []protocol.Part{protocol.Text{Text: strings.Repeat("line of file\n", pruneBatch)}}}}})
	turn := func(sig, id string) protocol.Response {
		return protocol.Response{StopReason: protocol.StopToolUse, Content: []protocol.Part{
			protocol.Thinking{Text: sig, Signature: sig},
			protocol.ToolCall{ID: id, Name: "Read", Args: []byte(`{}`)},
		}}
	}
	model := &scripted{replies: []protocol.Response{turn("t1", "c1"), turn("t2", "c2"), {StopReason: protocol.StopEnd, Content: []protocol.Part{protocol.Text{Text: "done"}}}}}
	ag := &Agent{Model: model, ModelID: "m", Session: sess, Hooks: []Hook{&Pruner{Session: sess, After: 1}, &StripThinking{}}}
	if _, err := ag.Run(context.Background(), []protocol.Part{protocol.Text{Text: "go"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(model.sent) != 3 {
		t.Fatalf("%d requests sent", len(model.sent))
	}
	if _, ok := sess.Pruned("c0"); !ok {
		t.Fatal("the old read was never pruned")
	}
	second, third := model.sent[1].Messages, model.sent[2].Messages
	if thinks(second[len(second)-2]) {
		t.Fatal("thinking resent after a prune changed the request before it")
	}
	if !thinks(third[len(third)-2]) {
		t.Fatal("thinking dropped though nothing before it changed")
	}
}
