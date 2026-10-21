package harness

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agent/subagent"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Model that answers scripted turns, each a list of parts, calling tools when a turn holds a tool call
type script struct {
	mu    sync.Mutex
	turns [][]protocol.Part
	// Blocks the first turn until released so a run can be cancelled mid stream
	hold  chan struct{}
	calls int
}

func (s *script) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		s.mu.Lock()
		i := s.calls
		s.calls++
		hold := s.hold
		s.mu.Unlock()
		if hold != nil && i == 0 {
			select {
			case <-hold:
			case <-ctx.Done():
				yield(protocol.Event{}, ctx.Err())
				return
			}
		}
		if i >= len(s.turns) {
			yield(protocol.Event{}, errors.New("script exhausted"))
			return
		}
		parts := s.turns[i]
		resp := protocol.Response{Content: parts, StopReason: protocol.StopEnd, Usage: protocol.Usage{Input: 10, Output: 5}}
		for _, p := range parts {
			if _, ok := p.(protocol.ToolCall); ok {
				resp.StopReason = protocol.StopToolUse
			}
		}
		if !yield(protocol.Event{Type: protocol.EventStart}, nil) {
			return
		}
		for _, p := range parts {
			if t, ok := p.(protocol.Text); ok {
				for _, r := range t.Text {
					if !yield(protocol.Event{Type: protocol.EventTextDelta, Delta: string(r), Response: resp}, nil) {
						return
					}
				}
			}
		}
		yield(protocol.Event{Type: protocol.EventDone, Response: resp}, nil)
	}
}

func text(s string) protocol.Part { return protocol.Text{Text: s} }

func call(id, name, args string) protocol.Part {
	return protocol.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}
}

func echoTool() tool.Tool {
	return tool.Func{Name: "Echo", Description: "echoes", Parameters: json.RawMessage(`{"type":"object"}`), Hints: tool.Annotations{ReadOnly: true}, Fn: func(ctx context.Context, args json.RawMessage) (tool.Result, error) {
		tool.Report(ctx, tool.Progress{Stream: "stdout", Text: "working"})
		return tool.Text("echo:" + string(args)), nil
	}}
}

func newController(t *testing.T) (*Controller, *sink, *session.Memory) {
	t.Helper()
	s := newSink()
	store := &session.Memory{}
	pump := NewPump(s.send)
	pump.Interval = time.Millisecond
	c := NewController(s.send, pump)
	c.Saver = NewSaver(store, s.send)
	c.Saver.Delay = time.Millisecond
	c.Meter = NewMeter(s.send)
	c.Workspace = NewWorkspace("/w")
	c.Missions = mission.New()
	return c, s, store
}

func TestControllerRunsAgentsAndPersists(t *testing.T) {
	c, s, store := newController(t)
	reg := &tool.Registry{}
	reg.Add(echoTool())
	model := &script{turns: [][]protocol.Part{{text("hi"), call("c1", "Echo", `{"x":1}`)}, {text("done")}}}
	sess := session.New()
	a := &agent.Agent{Name: "root", Model: model, ModelID: "m", Tools: reg, Gate: policy.Gate{Policy: policy.AllowAll}, Session: sess}
	m, _ := c.Missions.Create("objective")
	if _, err := c.Start(context.Background(), nil, "", nil); err == nil {
		t.Fatal("nil agent accepted")
	}
	r, err := c.Start(context.Background(), a, m.ID, []protocol.Part{text("go")})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "run-1" || r.Name != "root" || r.Mission != m.ID || !r.Running() {
		t.Fatalf("%+v", r)
	}
	if _, err := c.Start(context.Background(), a, "", nil); err == nil {
		t.Fatal("running agent started twice")
	}
	done := s.wait(t, func(a any) bool { _, ok := a.(RunDoneMsg); return ok }).(RunDoneMsg)
	if done.Err != nil || done.Response.Text() != "done" || done.Run != "run-1" || done.Duration < 0 {
		t.Fatalf("%+v", done)
	}
	dc, _ := c.Done("run-1")
	<-dc
	last, n := r.LastTool()
	if r.Running() || r.Finished().IsZero() || r.Elapsed(time.Now()) < 0 || last != "Echo" || n != 1 || r.Err() != nil || r.Response().Text() != "done" {
		t.Fatalf("%+v", r)
	}
	var progress, events, saved int
	for _, msg := range s.all() {
		switch v := msg.(type) {
		case ProgressMsg:
			progress++
			if v.Run != "run-1" || v.Progress.CallID != "c1" || v.Progress.Text != "working" {
				t.Fatalf("%+v", v)
			}
		case EventMsg:
			events++
		case SavedMsg:
			saved++
			if v.Err != nil || v.Session != sess.ID() {
				t.Fatalf("%+v", v)
			}
		}
	}
	if progress != 1 || events < 6 || saved < 1 {
		t.Fatalf("progress %d events %d saved %d", progress, events, saved)
	}
	if ok, _ := store.Exists(context.Background(), sess.ID()); !ok {
		t.Fatal("session must be saved")
	}
	if got, _ := c.Missions.Get(m.ID); len(got.Agents) != 1 || got.Agents[0] != "root" {
		t.Fatalf("%+v", got)
	}
	if c.Meter.Snapshot().Usage.Output != 10 || c.Busy() || len(c.Active()) != 0 || len(c.Runs()) != 1 {
		t.Fatal("bookkeeping")
	}
	steer := func(id string) error { return c.Steer(id, text("x")) }
	for _, fn := range []func(string) error{steer, c.Interrupt, c.Cancel} {
		if err := fn("run-1"); err == nil || !strings.Contains(err.Error(), "finished") {
			t.Fatalf("%v", err)
		}
		if err := fn("nope"); err == nil {
			t.Fatal("unknown run accepted")
		}
	}
	if _, ok := c.Done("nope"); ok {
		t.Fatal("unknown run has a done channel")
	}
	c.Shutdown()
}

func TestControllerCancelsSteersAndInterrupts(t *testing.T) {
	c, s, _ := newController(t)
	hold := make(chan struct{})
	model := &script{turns: [][]protocol.Part{{text("one")}, {text("two")}}, hold: hold}
	a := &agent.Agent{Name: "slow", Model: model, ModelID: "m", Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}
	r, err := c.Start(context.Background(), a, "", []protocol.Part{text("go")})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Busy() || len(c.Active()) != 1 {
		t.Fatal("run must be active")
	}
	if err := c.Steer(r.ID, text("more")); err != nil {
		t.Fatal(err)
	}
	if err := c.Interrupt(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(r.ID); err != nil {
		t.Fatal(err)
	}
	done := s.wait(t, func(a any) bool { _, ok := a.(RunDoneMsg); return ok }).(RunDoneMsg)
	if done.Err == nil {
		t.Fatal("cancelled run must fail")
	}
	close(hold)
	c.Shutdown()
}

func TestControllerTracksChildren(t *testing.T) {
	c, s, store := newController(t)
	childModel := &script{turns: [][]protocol.Part{{text("child answer")}}}
	sub := &subagent.Tool{Factory: func(ctx context.Context) (*agent.Agent, error) {
		return &agent.Agent{Model: childModel, ModelID: "m", Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}, nil
	}}
	c.Subagents = sub
	reg := &tool.Registry{}
	reg.Add(sub)
	parentModel := &script{turns: [][]protocol.Part{{call("c1", "Agent", `{"prompt":"do it","description":"delegate"}`)}, {text("parent done")}}}
	a := &agent.Agent{Name: "parent", Model: parentModel, ModelID: "m", Tools: reg, Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}
	r, err := c.Start(context.Background(), a, "", []protocol.Part{text("go")})
	if err != nil {
		t.Fatal(err)
	}
	s.wait(t, func(a any) bool { _, ok := a.(RunDoneMsg); return ok })
	kids := c.Children(r.ID)
	if len(kids) != 1 || kids[0].Parent != r.ID || kids[0].Depth != 1 || kids[0].CallID != "c1" || kids[0].Description != "delegate" || kids[0].Running() {
		t.Fatalf("%+v", kids)
	}
	if kids[0].Response().Text() != "child answer" || kids[0].Err() != nil || kids[0].Finished().IsZero() {
		t.Fatalf("%+v", kids[0])
	}
	if ok, _ := store.Exists(context.Background(), kids[0].Session.ID()); !ok {
		t.Fatal("child session must be saved")
	}
	if len(c.Runs()) != 2 {
		t.Fatalf("%d runs", len(c.Runs()))
	}
	if err := c.Cancel(kids[0].ID); err == nil || !strings.Contains(err.Error(), "finished") {
		t.Fatalf("%v", err)
	}
	if err := c.Steer(kids[0].ID, text("x")); err == nil {
		t.Fatal("steering a finished child accepted")
	}
	c.Subagents = nil
	kids[0].mu.Lock()
	kids[0].running = true
	kids[0].mu.Unlock()
	if err := c.Cancel(kids[0].ID); err == nil || !strings.Contains(err.Error(), "subagent tool") {
		t.Fatalf("%v", err)
	}
	kids[0].mu.Lock()
	kids[0].running = false
	kids[0].mu.Unlock()
	c.Shutdown()
}

func TestSaverCoalescesAndFlushes(t *testing.T) {
	s := newSink()
	store := &session.Memory{}
	sv := NewSaver(store, s.send)
	sv.Delay = 20 * time.Millisecond
	sess := session.New()
	sv.Touch(sess)
	sv.Touch(sess)
	sv.Touch(nil)
	(*Saver)(nil).Touch(sess)
	(*Saver)(nil).Flush(sess)
	(*Saver)(nil).Wait()
	sv.Wait()
	if n := len(s.all()); n != 1 {
		t.Fatalf("two touches must save once, got %d", n)
	}
	sv.Touch(sess)
	sv.Flush(sess)
	sv.Wait()
	if n := len(s.all()); n != 2 {
		t.Fatalf("flush must cancel the pending save and write once, got %d", n)
	}
	bad := &session.Memory{}
	broken := NewSaver(bad, s.send)
	broken.Delay = time.Millisecond
	invalid := session.FromHeader(session.Header{ID: "bad id!"}, nil, nil)
	broken.Touch(invalid)
	broken.Wait()
	last := s.all()[len(s.all())-1].(SavedMsg)
	if last.Err == nil {
		t.Fatal("save failure must be reported")
	}
	none := NewSaver(nil, s.send)
	none.Touch(sess)
	none.Flush(sess)
	none.Wait()
	zero := NewSaver(store, nil)
	zero.Delay = 0
	zero.Touch(sess)
	zero.Wait()
}
