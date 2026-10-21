package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestApproverRoundTrip(t *testing.T) {
	s := newSink()
	a := NewApprover(s.send)
	a.Clock = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	ctx := WithRun(context.Background(), "run-7")
	type outcome struct {
		v   policy.Verdict
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		v, err := a.Ask(ctx, policy.Request{Call: protocol.ToolCall{ID: "c1", Name: "Bash"}, Agent: "root", Reason: "rule"})
		done <- outcome{v, err}
	}()
	msg := s.wait(t, func(m any) bool { _, ok := m.(ApprovalMsg); return ok }).(ApprovalMsg)
	p := msg.Pending
	if p.ID != 1 || p.Run != "run-7" || p.Agent != "root" || p.Request.Call.ID != "c1" || p.Asked.IsZero() {
		t.Fatalf("%+v", p)
	}
	if got := a.Pending(); len(got) != 1 || got[0] != p || a.Len() != 1 {
		t.Fatalf("%+v", got)
	}
	if a.Answer(99, policy.Yes) {
		t.Fatal("unknown id answered")
	}
	if !a.Answer(p.ID, policy.Always) {
		t.Fatal("answer refused")
	}
	out := <-done
	if out.err != nil || out.v != policy.Always {
		t.Fatalf("%+v", out)
	}
	fin := s.wait(t, func(m any) bool { _, ok := m.(ApprovalDoneMsg); return ok }).(ApprovalDoneMsg)
	if fin.ID != p.ID || fin.Verdict != policy.Always || fin.Err != nil || fin.Run != "run-7" {
		t.Fatalf("%+v", fin)
	}
	if a.Len() != 0 || a.Answer(p.ID, policy.Yes) {
		t.Fatal("answered approval must be gone")
	}
}

func TestApproverCancelTimeoutAndClose(t *testing.T) {
	s := newSink()
	a := NewApprover(s.send)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := a.Ask(ctx, policy.Request{Call: protocol.ToolCall{ID: "c2"}})
		errs <- err
	}()
	s.wait(t, func(m any) bool { _, ok := m.(ApprovalMsg); return ok })
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	fin := s.wait(t, func(m any) bool { _, ok := m.(ApprovalDoneMsg); return ok }).(ApprovalDoneMsg)
	if fin.Err == nil {
		t.Fatal("cancelled approval must carry its error")
	}
	a.Timeout = 10 * time.Millisecond
	if _, err := a.Ask(context.Background(), policy.Request{Call: protocol.ToolCall{ID: "c3"}}); err == nil {
		t.Fatal("timeout must fail the ask")
	}
	a.Timeout = 0
	go func() {
		_, err := a.Ask(context.Background(), policy.Request{Call: protocol.ToolCall{ID: "c4"}})
		errs <- err
	}()
	s.wait(t, func(m any) bool { m2, ok := m.(ApprovalMsg); return ok && m2.Pending.Request.Call.ID == "c4" })
	a.Close()
	if err := <-errs; err != nil {
		t.Fatalf("close must deny the pending ask rather than fail it: %v", err)
	}
	if _, err := a.Ask(context.Background(), policy.Request{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("%v", err)
	}
	if a.DenyAll() != 0 {
		t.Fatal("nothing left to deny")
	}
}
