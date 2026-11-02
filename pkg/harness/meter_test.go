package harness

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Two sessions streaming at once keep their own usage, context, throughput and attempts while the totals add up
func TestMeterKeepsSessionsApart(t *testing.T) {
	var sent []AttemptMsg
	m := NewMeter(func(msg any) {
		if a, ok := msg.(AttemptMsg); ok {
			sent = append(sent, a)
		}
	})
	m.Bind("1", "a")
	m.Bind("2", "b")
	m.Track("a", profiles.Model{ID: "model-a", Context: 200000}, "home", 150000)
	m.Track("b", profiles.Model{ID: "model-b", Context: 100000}, "work", 0)
	base := time.Unix(1000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	delta := func(run string, ms int) {
		m.Apply(run, agent.Event{Type: agent.EventModel, Time: at(ms), Model: protocol.Event{Type: protocol.EventTextDelta, Delta: "abcdefgh"}})
	}

	m.Apply("1", agent.Event{Type: agent.EventTurnStart, Time: at(0)})
	m.Begin(provider.Attempt{Label: "1", Started: at(50)})
	m.Apply("2", agent.Event{Type: agent.EventTurnStart, Time: at(10)})
	delta("1", 100)
	delta("2", 120)
	delta("1", 600)
	delta("2", 700)

	a, _ := m.SnapshotOf("a")
	b, _ := m.SnapshotOf("b")
	if !a.Streaming || !b.Streaming {
		t.Fatalf("streaming a %v b %v, want both", a.Streaming, b.Streaming)
	}
	if a.FirstToken != 50*time.Millisecond || b.FirstToken != 110*time.Millisecond {
		t.Fatalf("first token a %s b %s, want 50ms and 110ms", a.FirstToken, b.FirstToken)
	}
	if a.TokensPerSecond != 8 {
		t.Fatalf("a streams %.2f tok/s, want 8 from its own deltas", a.TokensPerSecond)
	}
	if total := m.Snapshot(); total.Streaming || total.TokensPerSecond != 0 || total.FirstToken != 0 {
		t.Fatalf("totals blend per session gauges %+v", total)
	}

	m.Apply("1", agent.Event{Type: agent.EventTurnEnd, Time: at(1000), Usage: protocol.Usage{Input: 100, Output: 40, CacheRead: 900}})
	a, _ = m.SnapshotOf("a")
	b, _ = m.SnapshotOf("b")
	if a.Streaming || !b.Streaming {
		t.Fatalf("after a ends streaming a %v b %v, want only b", a.Streaming, b.Streaming)
	}

	m.Apply("2", agent.Event{Type: agent.EventTurnEnd, Time: at(1100), Usage: protocol.Usage{Input: 50, Output: 10, CacheWrite: 450}})
	m.Observe(provider.Attempt{Label: "2", Status: 200, Duration: 300 * time.Millisecond, RequestID: "req-b"})
	a, _ = m.SnapshotOf("a")
	b, _ = m.SnapshotOf("b")
	if want := (protocol.Usage{Input: 100, Output: 40, CacheRead: 900}); a.Usage != want {
		t.Fatalf("a usage %+v, want %+v", a.Usage, want)
	}
	if want := (protocol.Usage{Input: 50, Output: 10, CacheWrite: 450}); b.Usage != want {
		t.Fatalf("b usage %+v, want %+v", b.Usage, want)
	}
	if a.Context != 1000 || b.Context != 500 {
		t.Fatalf("context a %d b %d, want 1000 and 500", a.Context, b.Context)
	}
	if a.CacheRatio != 0.9 || b.CacheRatio != 0 {
		t.Fatalf("cache a %.2f b %.2f, want 0.9 and 0", a.CacheRatio, b.CacheRatio)
	}
	if a.Model != "model-a" || a.ContextLimit != 200000 || a.Threshold != 150000 || b.Model != "model-b" || b.Provider != "work" {
		t.Fatalf("tracked a %s %d %d b %s %s", a.Model, a.ContextLimit, a.Threshold, b.Model, b.Provider)
	}
	if a.Requests != 0 || b.Requests != 1 || b.LastRequestID != "req-b" {
		t.Fatalf("requests a %d b %d %q, want the attempt on b alone", a.Requests, b.Requests, b.LastRequestID)
	}
	if len(sent) != 1 || sent[0].Session != "b" || sent[0].Telemetry.Usage != b.Usage {
		t.Fatalf("attempt message %+v, want b and its telemetry", sent)
	}

	total := m.Snapshot()
	if want := a.Usage.Add(b.Usage); total.Usage != want {
		t.Fatalf("total usage %+v, want %+v", total.Usage, want)
	}
	if total.Context != 1500 {
		t.Fatalf("total context %d, want 1500", total.Context)
	}
	if total.CacheRatio != 0 {
		t.Fatalf("total cache %.2f, want 0", total.CacheRatio)
	}

	m.Apply("2", agent.Event{Type: agent.EventCompactEnd, Time: at(1200)})
	a, _ = m.SnapshotOf("a")
	b, _ = m.SnapshotOf("b")
	if a.Context != 1000 || b.Context != 0 {
		t.Fatalf("after b compacts context a %d b %d, want 1000 and 0", a.Context, b.Context)
	}
	if total := m.Snapshot(); total.Context != 1000 {
		t.Fatalf("total context after compaction %d, want 1000", total.Context)
	}

	if _, ok := m.SnapshotOf("missing"); ok {
		t.Fatal("unknown session reported telemetry")
	}
	if got := m.Sessions(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("sessions %v, want a then b", got)
	}
}

// Events and attempts of a sub-agent land on its own session while the cost stays under the root run
func TestMeterChargesChildSession(t *testing.T) {
	m := NewMeter(func(any) {})
	priced := &profiles.Pricing{Input: 1, Output: 1}
	m.Bind("1", "root")
	m.Bind(childRun("1", "subagent-1"), "child")
	m.Track("root", profiles.Model{ID: "model-root", Pricing: priced}, "home", 0)
	m.Track("child", profiles.Model{ID: "model-child", Pricing: priced}, "home", 0)

	m.Apply("1", agent.Event{Type: agent.EventTurnStart, Agent: "subagent-1", Depth: 1})
	if root, _ := m.SnapshotOf("root"); root.Streaming {
		t.Fatal("child turn marked the root streaming")
	}
	m.Apply("1", agent.Event{Type: agent.EventTurnEnd, Agent: "subagent-1", Depth: 1, Usage: protocol.Usage{Input: 1000000, Output: 1000000}})
	m.Observe(provider.Attempt{Label: childRun("1", "subagent-1"), Status: 200})

	root, _ := m.SnapshotOf("root")
	child, _ := m.SnapshotOf("child")
	if root.Context != 0 || root.Usage.Total() != 0 || root.Requests != 0 {
		t.Fatalf("root took the child turn %+v", root)
	}
	if child.Context != 1000000 || child.Requests != 1 || child.Cost != 2 || child.Costs[childRun("1", "subagent-1")] != 2 {
		t.Fatalf("child telemetry %+v", child)
	}
	if total := m.Snapshot(); total.Costs["1"] != 2 || total.Cost != 2 {
		t.Fatalf("total costs %v, want the child cost under the root run", total.Costs)
	}
	if got := m.SessionOf("1", agent.Event{Agent: "subagent-1", Depth: 1}); got != "child" {
		t.Fatalf("child event charged to %q", got)
	}
	if got := m.SessionOf("1", agent.Event{Agent: "root"}); got != "root" {
		t.Fatalf("root event charged to %q", got)
	}
}

// Requests a sub-agent sends carry its child run id while those of the root agent keep the run id
func TestLabelChild(t *testing.T) {
	root := policy.WithAgent(provider.WithLabel(WithRun(context.Background(), "1"), "1"), "model-root")
	if got := provider.LabelFrom(labelChild(root)); got != "1" {
		t.Fatalf("root request labeled %q, want 1", got)
	}
	child := policy.WithAgent(agent.WithLineage(root, "model-root", 1), "subagent-1")
	if got := provider.LabelFrom(labelChild(child)); got != childRun("1", "subagent-1") {
		t.Fatalf("child request labeled %q, want %q", got, childRun("1", "subagent-1"))
	}
	outside := provider.WithLabel(context.Background(), "session:x")
	if got := provider.LabelFrom(labelChild(outside)); got != "session:x" {
		t.Fatalf("request outside a run labeled %q, want session:x", got)
	}
}

// A session opened from the store carries its stored context into the meter and the totals until its next turn
func TestMeterSeedsStoredContext(t *testing.T) {
	m := NewMeter(func(any) {})
	m.seed("resumed", 4200)
	m.Bind("1", "fresh")
	m.Apply("1", agent.Event{Type: agent.EventTurnEnd, Usage: protocol.Usage{Input: 300}})
	if s, ok := m.SnapshotOf("resumed"); !ok || s.Context != 4200 {
		t.Fatalf("resumed session context %d %v, want 4200", s.Context, ok)
	}
	if total := m.Snapshot(); total.Context != 4500 {
		t.Fatalf("total context %d, want 4500", total.Context)
	}
}
