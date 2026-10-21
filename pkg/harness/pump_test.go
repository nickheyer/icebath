package harness

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Message sink safe to use from any goroutine
type sink struct {
	mu   sync.Mutex
	msgs []any
	ch   chan any
}

func newSink() *sink { return &sink{ch: make(chan any, 256)} }

func (s *sink) send(msg any) {
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.mu.Unlock()
	select {
	case s.ch <- msg:
	default:
	}
}

func (s *sink) all() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.msgs...)
}

func (s *sink) wait(t *testing.T, match func(any) bool) any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-s.ch:
			if match(m) {
				return m
			}
		case <-deadline:
			var kinds []string
			for _, m := range s.all() {
				kinds = append(kinds, fmt.Sprintf("%T%+v", m, m))
			}
			t.Fatalf("message never arrived, saw %d messages:\n%s", len(kinds), strings.Join(kinds, "\n"))
		}
	}
}

func textDelta(agentName string, round, index int, text string) agent.Event {
	return agent.Event{Type: agent.EventModel, Agent: agentName, Round: round, Model: protocol.Event{Type: protocol.EventTextDelta, Index: index, Delta: text, Response: protocol.Response{Content: []protocol.Part{protocol.Text{Text: text}}}}}
}

func TestPumpMergesDeltasPerFrame(t *testing.T) {
	s := newSink()
	p := NewPump(s.send)
	p.Interval = time.Hour
	p.Clock = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	obs := p.Observer("run-1")
	obs(textDelta("a", 1, 0, "hel"))
	obs(textDelta("a", 1, 0, "lo"))
	obs(agent.Event{Type: agent.EventModel, Agent: "a", Round: 1, Model: protocol.Event{Type: protocol.EventThinkingDelta, Index: 1, Delta: "hmm"}})
	if held(p) != 2 || len(s.all()) != 0 {
		t.Fatalf("held %d sent %d", held(p), len(s.all()))
	}
	obs(agent.Event{Type: agent.EventTurnEnd, Agent: "a", Round: 1})
	msgs := s.all()
	if len(msgs) != 3 || held(p) != 0 {
		t.Fatalf("%d messages held %d", len(msgs), held(p))
	}
	first := msgs[0].(EventMsg)
	if first.Run != "run-1" || first.Event.Model.Delta != "hello" || first.Event.Model.Response.Content[0].(protocol.Text).Text != "lo" || first.Event.Time.IsZero() {
		t.Fatalf("%+v", first)
	}
	if msgs[1].(EventMsg).Event.Model.Delta != "hmm" || msgs[2].(EventMsg).Event.Type != agent.EventTurnEnd {
		t.Fatalf("%+v", msgs[1:])
	}
	obs(textDelta("a", 2, 0, "x"))
	obs(textDelta("b", 2, 0, "y"))
	obs(agent.Event{Type: agent.EventModel, Agent: "a", Round: 2, Model: protocol.Event{Type: protocol.EventToolCallDelta, Index: 0, Delta: "{", Partial: "{"}})
	if held(p) != 3 {
		t.Fatalf("different agents and kinds must not merge, held %d", held(p))
	}
	p.Flush()
	if held(p) != 0 || len(s.all()) != 6 {
		t.Fatalf("flush held %d sent %d", held(p), len(s.all()))
	}
	p.Flush()
	if len(s.all()) != 6 {
		t.Fatal("empty flush sent something")
	}
}

func TestPumpFlushesOnTheFrameTimer(t *testing.T) {
	s := newSink()
	p := NewPump(s.send)
	p.Interval = 5 * time.Millisecond
	p.Push("r", textDelta("a", 1, 0, "a"))
	p.Push("r", textDelta("a", 1, 0, "b"))
	m := s.wait(t, func(m any) bool { _, ok := m.(EventMsg); return ok })
	if m.(EventMsg).Event.Model.Delta != "ab" {
		t.Fatalf("%+v", m)
	}
	p.Interval = 0
	p.Push("r", textDelta("a", 1, 0, "c"))
	if got := s.wait(t, func(m any) bool { return m.(EventMsg).Event.Model.Delta == "c" }); got == nil {
		t.Fatal("default interval must still flush")
	}
}

// Deltas waiting for the frame to end
func held(p *Pump) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}
