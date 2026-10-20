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

func TestVerdicts(t *testing.T) {
	ctx := context.Background()
	m := &scripted{text: `Sure. {"decision":"allow","reason":"harmless"}`}
	a := Approver{Model: m, ModelID: "judge-1", Instructions: "Production host."}
	ok, err := a.Approve(ctx, call)
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if m.req.Model != "judge-1" || m.req.MaxTokens != 256 || !strings.Contains(m.req.System, "Production host.") || !strings.Contains(m.req.Messages[0].Content[0].(protocol.Text).Text, "\"command\": \"rm -rf /\"") {
		t.Fatalf("%+v", m.req)
	}
	m.text = `{"decision":"deny","reason":"wipes the disk"}`
	var denied *policy.Denied
	if ok, err := a.Approve(ctx, call); ok || !errors.As(err, &denied) || !strings.Contains(err.Error(), "wipes the disk") {
		t.Fatalf("%v %v", ok, err)
	}
	m.text = `{"decision":"ask","reason":"unsure"}`
	if ok, err := a.Approve(ctx, call); ok || !errors.As(err, &denied) || !strings.Contains(err.Error(), "none is configured") {
		t.Fatalf("%v %v", ok, err)
	}
	escalated := 0
	a.Escalate = policy.ApproverFunc(func(context.Context, protocol.ToolCall) (bool, error) { escalated++; return true, nil })
	if ok, err := a.Approve(ctx, call); !ok || err != nil || escalated != 1 {
		t.Fatalf("%v %v %d", ok, err, escalated)
	}
	for _, bad := range []string{"", "no json here", `{"decision":"maybe"}`, `{"decision":`, `{"reason":"x"}`} {
		m.text = bad
		if ok, err := a.Approve(ctx, call); ok || err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	m.text, m.err = `{"decision":"allow"}`, errors.New("down")
	if ok, err := a.Approve(ctx, call); ok || err == nil || !strings.Contains(err.Error(), "down") {
		t.Fatalf("%v %v", ok, err)
	}
	m.err, m.wait = nil, time.Second
	a.Timeout = 20 * time.Millisecond
	if ok, err := a.Approve(ctx, call); ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := (Approver{}).Approve(ctx, call); err == nil {
		t.Fatal("unconfigured judge approved")
	}
	v, err := Approver{Model: &scripted{text: `{"decision":"deny","reason":"r"}`}, ModelID: "m", MaxTokens: 9}.Judge(ctx, protocol.ToolCall{Name: "x", Args: json.RawMessage(`not json`)})
	if err != nil || v.Decision != policy.Deny || v.Reason != "r" {
		t.Fatalf("%+v %v", v, err)
	}
}
