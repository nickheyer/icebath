package harness

import (
	"maps"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Attempts remembered for the telemetry log
const maxAttempts = 64

// What the model layer has done so far, as a card can show it
type Telemetry struct {
	Model    string
	Provider string
	// Tokens the next request will carry
	Context int
	// Context window of the model, zero when unknown
	ContextLimit int
	// Context at which compaction runs, zero when none is configured
	Threshold int
	Usage     protocol.Usage
	// Dollars spent over the session, zero when no pricing is known
	Cost float64
	// Whether the model has pricing, so the cost means something
	Priced bool
	// Share of the last request served from cache
	CacheRatio float64
	// Output tokens per second of the current or last turn
	TokensPerSecond float64
	Streaming       bool
	Requests        int
	Retries         int
	Failures        int
	LastRequestID   string
	LastLatency     time.Duration
	AvgLatency      time.Duration
	// Time from the request start to the first token of the current or last turn
	FirstToken time.Duration
	RateLimit  provider.RateLimit
	// Dollars per run id
	Costs map[string]float64
	// Calls and estimated result tokens per tool name
	Tools map[string]ToolStat
	// Estimated result tokens over every tool call
	ToolTokens int
}

// Calls a tool took and the estimated tokens its results put into context
type ToolStat struct {
	Calls  int
	Tokens int
}

// Provider observer and agent event consumer that keeps the telemetry current
type Meter struct {
	send      Sender
	mu        sync.Mutex
	t         Telemetry
	attempts  []provider.Attempt
	latencies time.Duration
	turnStart time.Time
	// Start of the request in flight and of its first token
	reqStart time.Time
	firstAt  time.Time
	deltas   int
	outChars int
	pricing  *profiles.Pricing
}

func NewMeter(send Sender) *Meter {
	return &Meter{send: send, t: Telemetry{Costs: map[string]float64{}, Tools: map[string]ToolStat{}}}
}

// Uses an explicit catalog entry, for endpoints that report their own pricing
func (m *Meter) SetCatalog(model profiles.Model, providerName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.Model, m.t.Provider = model.ID, providerName
	m.pricing = model.Pricing
	m.t.Priced = model.Pricing != nil
	m.t.ContextLimit = model.Context
}

// Forgets usage, cost, attempts and counters for a new session, keeping the model, pricing and threshold
func (m *Meter) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = Telemetry{Model: m.t.Model, Provider: m.t.Provider, Priced: m.t.Priced, ContextLimit: m.t.ContextLimit, Threshold: m.t.Threshold, Costs: map[string]float64{}, Tools: map[string]ToolStat{}}
	m.attempts = nil
	m.latencies = 0
	m.turnStart, m.reqStart, m.firstAt, m.deltas, m.outChars = time.Time{}, time.Time{}, time.Time{}, 0, 0
}

// Records when a request leaves, the origin the first token is timed from
func (m *Meter) Begin(a provider.Attempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqStart = a.Started
}

// Records the compaction threshold shown against the context fill
func (m *Meter) SetThreshold(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.Threshold = n
}

// Records one provider attempt
func (m *Meter) Observe(a provider.Attempt) {
	m.mu.Lock()
	m.t.Requests++
	if a.Wait > 0 {
		m.t.Retries++
	}
	if a.Err != nil {
		m.t.Failures++
	}
	if a.RequestID != "" {
		m.t.LastRequestID = a.RequestID
	}
	m.t.LastLatency = a.Duration
	m.latencies += a.Duration
	m.t.AvgLatency = m.latencies / time.Duration(m.t.Requests)
	if a.RateLimit.Known() {
		m.t.RateLimit = a.RateLimit
	}
	m.attempts = append(m.attempts, a)
	if len(m.attempts) > maxAttempts {
		m.attempts = m.attempts[len(m.attempts)-maxAttempts:]
	}
	snap := m.snapshot()
	m.mu.Unlock()
	m.send(AttemptMsg{Attempt: a, Telemetry: snap})
}

// Updates usage, cost, context fill and throughput from an agent event
func (m *Meter) Apply(run string, ev agent.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := ev.Time
	if now.IsZero() {
		now = time.Now()
	}
	switch ev.Type {
	case agent.EventTurnStart:
		m.turnStart, m.reqStart, m.firstAt, m.deltas, m.outChars = now, time.Time{}, time.Time{}, 0, 0
		m.t.Streaming, m.t.TokensPerSecond, m.t.FirstToken = true, 0, 0
	case agent.EventModel:
		switch ev.Model.Type {
		case protocol.EventTextDelta, protocol.EventThinkingDelta, protocol.EventToolCallDelta:
			m.deltas++
			if m.firstAt.IsZero() {
				m.firstAt = now
				m.t.FirstToken = now.Sub(m.origin())
			}
			m.outChars += len(ev.Model.Delta)
			if el := now.Sub(m.firstAt).Seconds(); el > 0.25 {
				m.t.TokensPerSecond = float64(m.outChars) / 4 / el
			}
		}
	case agent.EventTurnEnd:
		m.t.Streaming = false
		m.t.Usage = m.t.Usage.Add(ev.Usage)
		m.t.Context = ev.Usage.Context()
		if ctx := ev.Usage.Context(); ctx > 0 {
			m.t.CacheRatio = float64(ev.Usage.CacheRead) / float64(ctx)
		}
		if el := now.Sub(m.generating()).Seconds(); el > 0 && ev.Usage.Output > 0 {
			m.t.TokensPerSecond = float64(ev.Usage.Output) / el
		}
		if pricing := m.pricingFor(ev); pricing != nil {
			c := pricing.Cost(ev.Usage)
			m.t.Cost += c
			m.t.Costs[run] += c
		}
	case agent.EventToolEnd:
		tokens := protocol.EstimateTokens(ev.Result.Content)
		st := m.t.Tools[ev.Call.Name]
		st.Calls++
		st.Tokens += tokens
		m.t.Tools[ev.Call.Name] = st
		m.t.ToolTokens += tokens
	case agent.EventCompactEnd:
		m.t.Context = 0
	case agent.EventRunEnd, agent.EventInterrupt:
		m.t.Streaming = false
	}
}

// Moment the turn is timed from, the request start when the provider reported one and the turn start before that
func (m *Meter) origin() time.Time {
	if m.reqStart.IsZero() || m.reqStart.Before(m.turnStart) {
		return m.turnStart
	}
	return m.reqStart
}

// Moment generation is timed from, the first token of a streamed turn and the request start of one that came whole
func (m *Meter) generating() time.Time {
	if m.deltas < 2 || m.firstAt.IsZero() {
		return m.origin()
	}
	return m.firstAt
}

// Pricing of the model that answered the turn when the catalog knows it, else the pricing set for the meter
func (m *Meter) pricingFor(ev agent.Event) *profiles.Pricing {
	if ev.Response != nil && ev.Response.Model != "" && ev.Response.Model != m.t.Model {
		if known, ok := profiles.Describe(ev.Response.Model); ok && known.Pricing != nil {
			return known.Pricing
		}
	}
	return m.pricing
}

// Current telemetry
func (m *Meter) Snapshot() Telemetry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot()
}

func (m *Meter) snapshot() Telemetry {
	t := m.t
	t.Costs = make(map[string]float64, len(m.t.Costs))
	maps.Copy(t.Costs, m.t.Costs)
	t.Tools = make(map[string]ToolStat, len(m.t.Tools))
	maps.Copy(t.Tools, m.t.Tools)
	return t
}

// Recent attempts, oldest first
func (m *Meter) Attempts() []provider.Attempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]provider.Attempt(nil), m.attempts...)
}

var _ provider.Observer = (*Meter)(nil)
var _ provider.Starter = (*Meter)(nil)
