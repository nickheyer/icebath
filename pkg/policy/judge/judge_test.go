package judge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

type scripted struct {
	text string
	err  error
	req  protocol.Request
	wait time.Duration
}

func (s *scripted) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	s.req = req
	if s.wait > 0 {
		select {
		case <-time.After(s.wait):
		case <-ctx.Done():
			return protocol.Response{}, ctx.Err()
		}
	}
	return protocol.Response{Content: []protocol.Part{protocol.Text{Text: s.text}}}, s.err
}

var call = protocol.ToolCall{ID: "c", Name: "Bash", Args: json.RawMessage(`{"command":"rm -rf /"}`)}

var req = policy.Request{Call: call, Agent: "root", Reason: "default", Tool: &protocol.Tool{Name: "Bash", Description: "Runs a command"}}

func TestVerdicts(t *testing.T) {
	ctx := context.Background()
	m := &scripted{text: `Sure. {"decision":"allow","reason":"harmless"}`}
	a := Approver{Model: m, ModelID: "judge-1", Instructions: "Production host.", Context: func(context.Context, policy.Request) string { return "cwd /srv" }}
	ans, err := a.Approve(ctx, req)
	if !ans.Allowed || err != nil || ans.By != "judge" || ans.Reason != "harmless" {
		t.Fatalf("%+v %v", ans, err)
	}
	text := m.req.Messages[0].Content[0].(protocol.Text).Text
	if m.req.Model != "judge-1" || m.req.MaxTokens != 256 || !strings.Contains(m.req.System, "Production host.") || !strings.Contains(text, "\"command\": \"rm -rf /\"") || !strings.Contains(text, "Agent: root") || !strings.Contains(text, "Purpose: Runs a command") || !strings.Contains(text, "Gate: default") || !strings.Contains(text, "Context:\ncwd /srv") {
		t.Fatalf("%+v", m.req)
	}
	m.text = `{"decision":"deny","reason":"wipes the disk"}`
	var denied *policy.Denied
	if ans, err := a.Approve(ctx, req); ans.Allowed || !errors.As(err, &denied) || !strings.Contains(err.Error(), "wipes the disk") || ans.Reason != "wipes the disk" {
		t.Fatalf("%+v %v", ans, err)
	}
	m.text = `{"decision":"ask","reason":"unsure"}`
	if ans, err := a.Approve(ctx, req); ans.Allowed || !errors.As(err, &denied) || !strings.Contains(err.Error(), "none is configured") {
		t.Fatalf("%+v %v", ans, err)
	}
	var escalated []policy.Request
	a.Escalate = policy.ApproverFunc(func(_ context.Context, r policy.Request) (policy.Answer, error) {
		escalated = append(escalated, r)
		return policy.Answer{Allowed: true, By: "person"}, nil
	})
	if ans, err := a.Approve(ctx, req); !ans.Allowed || err != nil || len(escalated) != 1 || ans.By != "person" {
		t.Fatalf("%+v %v %d", ans, err, len(escalated))
	}
	if r := escalated[0].Review; r == nil || r.By != "judge" || r.Decision != policy.Ask || r.Reason != "unsure" {
		t.Fatalf("the person must see the review: %+v", escalated[0])
	}
	for _, bad := range []string{"", "no json here", `{"decision":"maybe"}`, `{"decision":`, `{"reason":"x"}`} {
		m.text = bad
		if ans, err := a.Approve(ctx, req); ans.Allowed || err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	m.text, m.err = `{"decision":"allow"}`, errors.New("down")
	if ans, err := a.Approve(ctx, req); ans.Allowed || err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("%+v %v", ans, err)
	}
	m.err, m.wait = nil, time.Second
	a.Timeout = 20 * time.Millisecond
	if ans, err := a.Approve(ctx, req); ans.Allowed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%+v %v", ans, err)
	}
	if _, err := (Approver{}).Approve(ctx, req); err == nil {
		t.Fatal("unconfigured judge approved")
	}
	v, err := Approver{Model: &scripted{text: `{"decision":"deny","reason":"r"}`}, ModelID: "m", MaxTokens: 9}.Judge(ctx, policy.Request{Call: protocol.ToolCall{Name: "x", Args: json.RawMessage(`not json`)}})
	if err != nil || v.Decision != policy.Deny || v.Reason != "r" {
		t.Fatalf("%+v %v", v, err)
	}
	if !strings.Contains(describe(policy.Request{Call: protocol.ToolCall{Name: "x", Args: json.RawMessage(`not json`)}}), "Arguments:\nnot json") {
		t.Fatal("raw arguments must survive an indent failure")
	}
}
