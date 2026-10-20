package compact

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

type fake struct {
	text string
	err  error
	reqs []protocol.Request
}

func (f *fake) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		f.reqs = append(f.reqs, req)
		if f.err != nil {
			yield(protocol.Event{}, f.err)
			return
		}
		yield(protocol.Event{Type: protocol.EventDone, Response: protocol.Response{Content: []protocol.Part{protocol.Text{Text: f.text}}}}, nil)
	}
}

func transcript(turns int, usage int) *session.Session {
	s := session.New()
	for i := 0; i < turns; i++ {
		s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "ask"}}})
		s.AddResponse("", protocol.Response{Content: []protocol.Part{protocol.Thinking{Text: "t"}, protocol.Text{Text: "answer"}}, Usage: protocol.Usage{Input: usage}})
	}
	return s
}

func TestSummarizerCompactsPastThreshold(t *testing.T) {
	ctx := context.Background()
	model := &fake{text: "Goal\n- do things"}
	s := transcript(5, 900)
	h := &Summarizer{Model: model, ModelID: "sum", Session: s, Threshold: 1000, Keep: 2, Instructions: "Mention hosts."}
	req := protocol.Request{Messages: s.Snapshot()}
	if err := h.BeforeTurn(ctx, &req); err != nil || len(model.reqs) != 0 || len(req.Messages) != 10 {
		t.Fatalf("below threshold must do nothing: %v %d", err, len(model.reqs))
	}
	s = transcript(5, 1200)
	h.Session = s
	var notes []string
	var kinds []agent.EventType
	ctx = agent.WithObserver(ctx, func(e agent.Event) {
		kinds = append(kinds, e.Type)
		if e.Type == agent.EventNote {
			notes = append(notes, e.Text)
		}
	})
	befores := 0
	h.Before = func(context.Context) error { befores++; return nil }
	req = protocol.Request{Messages: s.Snapshot()}
	if err := h.BeforeTurn(ctx, &req); err != nil {
		t.Fatal(err)
	}
	if befores != 1 || len(kinds) != 3 || kinds[0] != agent.EventCompactStart || kinds[1] != agent.EventCompactEnd || kinds[2] != agent.EventNote {
		t.Fatalf("%d %v", befores, kinds)
	}
	if recs := s.Records(); len(recs) != 1 || recs[0].Kind != "compact" || !strings.Contains(string(recs[0].Data), "Goal") {
		t.Fatalf("%+v", recs)
	}
	if len(model.reqs) != 1 || model.reqs[0].Model != "sum" || model.reqs[0].MaxTokens != 2048 || !strings.Contains(model.reqs[0].System, "Mention hosts.") || !strings.Contains(model.reqs[0].Messages[0].Content[0].(protocol.Text).Text, "## Assistant") {
		t.Fatalf("%+v", model.reqs)
	}
	if len(req.Messages) != 4 || s.Len() != 4 || !strings.HasPrefix(req.Messages[0].Content[0].(protocol.Text).Text, "Summary of the earlier conversation:\n\nGoal") || req.Messages[1].Content[0].(protocol.Text).Text != session.Ack {
		t.Fatalf("%+v", req.Messages)
	}
	if s.Context() != 0 {
		t.Fatal("context measure must reset after compaction")
	}
	if len(notes) != 1 || notes[0] != "compacted 10 messages into a summary, 4 remain" {
		t.Fatalf("%v", notes)
	}
	if err := h.BeforeTurn(ctx, &req); err != nil || len(model.reqs) != 1 {
		t.Fatal("no second summary until the context grows again")
	}
	small := transcript(1, 5000)
	h.Session, h.Keep = small, 6
	if err := h.BeforeTurn(ctx, &req); err != nil || len(model.reqs) != 1 {
		t.Fatal("nothing to compact when everything is kept")
	}
	grown := transcript(5, 900)
	grown.AddResults("", []protocol.ToolResult{{CallID: "c", Content: []protocol.Part{protocol.Text{Text: strings.Repeat("x", 2000)}}}})
	h.Session, h.Keep = grown, 2
	req = protocol.Request{Messages: grown.Snapshot()}
	if err := h.BeforeTurn(ctx, &req); err != nil || len(model.reqs) != 2 {
		t.Fatalf("a large tool result added since the last response must count toward the threshold: %v %d", err, len(model.reqs))
	}
	if pending([]protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Image{Data: []byte{1}}, protocol.ToolCall{Args: json.RawMessage(`{"a":1}`)}}}}) != 1001 {
		t.Fatal("pending estimate")
	}
	failing := &Summarizer{Model: model, ModelID: "sum", Session: transcript(5, 1200), Threshold: 1000, Keep: 2, Before: func(context.Context) error { return errors.New("pre compact hook refused") }}
	if err := failing.BeforeTurn(ctx, &protocol.Request{}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("%v", err)
	}
}

func TestLongTranscriptsAreSummarizedInParts(t *testing.T) {
	ctx := context.Background()
	model := &fake{text: "part summary"}
	s := transcript(6, 0)
	h := &Summarizer{Model: model, ModelID: "sum", Session: s, Threshold: 1, ChunkChars: 120}
	summary, err := h.Summarize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.reqs) < 3 {
		t.Fatalf("expected several part calls and one combine call, got %d", len(model.reqs))
	}
	last := model.reqs[len(model.reqs)-1]
	if !strings.Contains(last.System, "merge partial") || !strings.Contains(last.Messages[0].Content[0].(protocol.Text).Text, "Part 1 of") {
		t.Fatalf("%+v", last)
	}
	for _, r := range model.reqs[:len(model.reqs)-1] {
		if !strings.Contains(r.Messages[0].Content[0].(protocol.Text).Text, "<transcript part=") {
			t.Fatalf("%+v", r)
		}
	}
	if !strings.HasPrefix(summary, "Summary of the earlier conversation:\n\npart summary") {
		t.Fatalf("%q", summary)
	}
	if h.chunkChars() != 120 || (&Summarizer{}).chunkChars() != DefaultChunkChars {
		t.Fatal("chunk default")
	}
}

func TestSummarizerFailures(t *testing.T) {
	ctx := context.Background()
	s := transcript(5, 5000)
	if err := (&Summarizer{}).BeforeTurn(ctx, &protocol.Request{}); err == nil {
		t.Fatal("unconfigured summarizer ran")
	}
	if _, err := (&Summarizer{}).Summarize(ctx); err == nil {
		t.Fatal("unconfigured summarizer summarized")
	}
	down := &Summarizer{Model: &fake{err: errors.New("down")}, ModelID: "m", Session: s, Threshold: 10}
	if err := down.BeforeTurn(ctx, &protocol.Request{}); err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("%v", err)
	}
	empty := &Summarizer{Model: &fake{}, ModelID: "m", Session: s, Threshold: 10}
	if err := empty.BeforeTurn(ctx, &protocol.Request{}); err == nil || !strings.Contains(err.Error(), "no summary") {
		t.Fatalf("%v", err)
	}
	if s.Len() != 10 {
		t.Fatal("failed summaries must leave the session alone")
	}
	keep := &Summarizer{Model: &fake{text: "s"}, ModelID: "m", Session: s, Threshold: 10, MaxTokens: 99}
	if keep.maxTokens() != 99 || keep.keep() != 6 {
		t.Fatal("defaults")
	}
	plain, err := keep.Summarize(ctx)
	if err != nil || !strings.HasSuffix(plain, "\n\ns") {
		t.Fatalf("%v %q", err, plain)
	}
	if _, err := keep.BeforeRun(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStripThinking(t *testing.T) {
	req := protocol.Request{Messages: []protocol.Message{
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "a"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "old"}, protocol.Text{Text: "b"}}},
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "c"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "only"}}},
		{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "d"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.Part{protocol.Thinking{Text: "fresh", Signature: "sig"}, protocol.ToolCall{ID: "1", Name: "t"}}},
	}}
	if err := (StripThinking{}).BeforeTurn(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[1].Content) != 1 || req.Messages[1].Content[0].(protocol.Text).Text != "b" {
		t.Fatalf("%+v", req.Messages[1])
	}
	if len(req.Messages[3].Content) != 1 || req.Messages[3].Content[0].(protocol.Text).Text != Omitted {
		t.Fatalf("%+v", req.Messages[3])
	}
	if len(req.Messages[5].Content) != 2 {
		t.Fatal("the last assistant turn must keep its thinking")
	}
	if err := (StripThinking{}).BeforeTurn(context.Background(), &protocol.Request{}); err != nil {
		t.Fatal(err)
	}
}
