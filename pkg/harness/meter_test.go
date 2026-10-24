package harness

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMeterCountsAttempts(t *testing.T) {
	s := newSink()
	m := NewMeter(s.send)
	m.Max = 2
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Clock = func() time.Time { return at }
	h := http.Header{}
	h.Set("x-ratelimit-remaining-requests", "9")
	m.Observe(provider.Attempt{Status: 429, Err: errors.New("slow"), Duration: 100 * time.Millisecond, Wait: time.Second, RequestID: "r1"})
	m.Observe(provider.Attempt{Status: 200, Duration: 300 * time.Millisecond, RequestID: "r2", RateLimit: provider.ParseRateLimit(h)})
	m.Observe(provider.Attempt{Status: 500, Err: errors.New("down"), Duration: 200 * time.Millisecond})
	snap := m.Snapshot()
	if snap.Requests != 3 || snap.Retries != 1 || snap.Failures != 2 || snap.LastStatus != 500 || snap.LastRequestID != "r2" || snap.AvgLatency != 200*time.Millisecond || snap.LastLatency != 200*time.Millisecond {
		t.Fatalf("%+v", snap)
	}
	if snap.RateLimit.RequestsRemaining != 9 || !snap.RateLimitAt.Equal(at) {
		t.Fatalf("%+v", snap)
	}
	if got := m.Attempts(); len(got) != 2 || got[0].RequestID != "r2" {
		t.Fatalf("ring must keep the newest: %+v", got)
	}
	msgs := s.all()
	if len(msgs) != 3 || msgs[2].(AttemptMsg).Telemetry.Requests != 3 {
		t.Fatalf("%+v", msgs)
	}
	m.Failover(0, errors.New("x"))
	m.Served(1)
	if snap = m.Snapshot(); snap.Failovers != 1 || snap.Serving != 1 {
		t.Fatalf("%+v", snap)
	}
	msgs = s.all()
	if _, ok := msgs[3].(FailoverMsg); !ok {
		t.Fatalf("%T", msgs[3])
	}
	if served, ok := msgs[4].(ServedMsg); !ok || served.Index != 1 {
		t.Fatalf("%+v", msgs[4])
	}
}

func TestMeterTracksUsageCostAndThroughput(t *testing.T) {
	s := newSink()
	m := NewMeter(s.send)
	opus, _ := profiles.Describe("claude-opus-5")
	m.SetCatalog(opus, "anthropic")
	m.SetThreshold(150000)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Apply("run-1", agent.Event{Type: agent.EventTurnStart, Time: base})
	m.Apply("run-1", agent.Event{Type: agent.EventModel, Time: base.Add(200 * time.Millisecond), Model: protocol.Event{Type: protocol.EventTextDelta, Delta: "12345678"}})
	snap := m.Snapshot()
	if !snap.Streaming || snap.FirstToken != 200*time.Millisecond || snap.TokensPerSecond != 0 {
		t.Fatalf("%+v", snap)
	}
	m.Apply("run-1", agent.Event{Type: agent.EventModel, Time: base.Add(time.Second), Model: protocol.Event{Type: protocol.EventThinkingDelta, Delta: "12345678"}})
	if snap = m.Snapshot(); snap.TokensPerSecond < 4.9 || snap.TokensPerSecond > 5.1 {
		t.Fatalf("estimated rate %v", snap.TokensPerSecond)
	}
	usage := protocol.Usage{Input: 1000, Output: 200, CacheRead: 3000, CacheWrite: 0}
	m.Apply("run-1", agent.Event{Type: agent.EventTurnEnd, Time: base.Add(2 * time.Second), Usage: usage})
	snap = m.Snapshot()
	if snap.Streaming || snap.Usage != usage || snap.Context != 4000 || snap.TokensPerSecond < 111.1 || snap.TokensPerSecond > 111.2 || snap.ContextLimit != 1000000 || snap.Threshold != 150000 {
		t.Fatalf("%+v", snap)
	}
	if snap.CacheRatio != 0.75 {
		t.Fatalf("cache ratio %v", snap.CacheRatio)
	}
	want := (1000*5 + 200*25 + 3000*0.5) / 1e6
	if snap.Cost < want-1e-9 || snap.Cost > want+1e-9 || snap.Costs["run-1"] != snap.Cost {
		t.Fatalf("cost %v want %v", snap.Cost, want)
	}
	m.Apply("run-1", agent.Event{Type: agent.EventCompactEnd})
	if m.Snapshot().Context != 0 {
		t.Fatal("compaction must reset the context measure")
	}
	m.Apply("run-1", agent.Event{Type: agent.EventTurnStart})
	m.Apply("run-1", agent.Event{Type: agent.EventRunEnd})
	if m.Snapshot().Streaming {
		t.Fatal("run end must stop streaming")
	}
	m.SetCatalog(profiles.Model{ID: "unknown-model"}, "x")
	m.Apply("run-2", agent.Event{Type: agent.EventTurnEnd, Usage: protocol.Usage{Output: 10}})
	if snap = m.Snapshot(); snap.Cost != want || snap.ContextLimit != 0 || snap.Model != "unknown-model" {
		t.Fatalf("unknown models must not add cost: %+v", snap)
	}
	m.SetCatalog(profiles.Model{ID: "mine", Context: 42, Pricing: &profiles.Pricing{Output: 1e6}}, "p")
	m.Apply("run-2", agent.Event{Type: agent.EventTurnEnd, Usage: protocol.Usage{Output: 1}})
	if snap = m.Snapshot(); snap.ContextLimit != 42 || snap.Cost < want+1-1e-9 {
		t.Fatalf("%+v", snap)
	}
	snap.Costs["run-1"] = 0
	if m.Snapshot().Costs["run-1"] == 0 {
		t.Fatal("snapshot must copy costs")
	}
}
