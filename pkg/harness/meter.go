package harness

import (
	"maps"
	"slices"
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
	// Tokens the next request will carry, summed over every session in the totals
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
	// Share of the last request of the session served from cache, zero in the totals
	CacheRatio float64
	// Output tokens per second of the current or last turn of the session, zero in the totals
	TokensPerSecond float64
	// Whether the session is in a turn, false in the totals
	Streaming     bool
	Requests      int
	Retries       int
	Failures      int
	LastRequestID string
	LastLatency   time.Duration
	AvgLatency    time.Duration
	// Time from the request start to the first token of the current or last turn, zero in the totals
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

// Provider observer and agent event consumer that keeps the telemetry of every session and their totals current
type Meter struct {
	send      Sender
	mu        sync.Mutex
	t         Telemetry
	attempts  []provider.Attempt
	latencies time.Duration
	pricing   *profiles.Pricing
	// Session each run id works on
	runs map[string]string
	// Telemetry of each session by id, with the ids in first seen order
	sessions map[string]*gauge
	order    []string
}

// Telemetry of one session with the timing state its throughput is measured from
type gauge struct {
	t         Telemetry
	pricing   *profiles.Pricing
	latencies time.Duration
	turnStart time.Time
	// Start of the request in flight and of its first token
	reqStart time.Time
	firstAt  time.Time
	deltas   int
	outChars int
}

func NewMeter(send Sender) *Meter {
	return &Meter{send: send, t: blank(), runs: map[string]string{}, sessions: map[string]*gauge{}}
}

// Telemetry with nothing recorded and its maps ready
func blank() Telemetry {
	return Telemetry{Costs: map[string]float64{}, Tools: map[string]ToolStat{}}
}

// Uses an explicit catalog entry for the totals and for runs bound to no session
func (m *Meter) SetCatalog(model profiles.Model, providerName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.Model, m.t.Provider = model.ID, providerName
	m.pricing = model.Pricing
	m.t.Priced = model.Pricing != nil
	m.t.ContextLimit = model.Context
}

// Records the compaction threshold of the totals
func (m *Meter) SetThreshold(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t.Threshold = n
}

// Records the model, endpoint, pricing, window and compaction threshold one session runs on
func (m *Meter) Track(session string, model profiles.Model, providerName string, threshold int) {
	if session == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.gauge(session)
	g.t.Model, g.t.Provider = model.ID, providerName
	g.pricing = model.Pricing
	g.t.Priced = model.Pricing != nil
	g.t.ContextLimit = model.Context
	g.t.Threshold = threshold
}

// Sets the context a session carries as its last stored response measured it, for one opened or forked
func (m *Meter) seed(session string, context int) {
	if session == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauge(session).t.Context = context
}

// Charges the attempts and events labeled with the run id to the session
func (m *Meter) Bind(run, session string) {
	if run == "" || session == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run] = session
	m.gauge(session)
}

// Telemetry of the session, created empty on first sight
func (m *Meter) gauge(session string) *gauge {
	if g, ok := m.sessions[session]; ok {
		return g
	}
	g := &gauge{t: blank()}
	m.sessions[session] = g
	m.order = append(m.order, session)
	return g
}

// Session id the run label is bound to with its telemetry, nil when unbound
func (m *Meter) bound(run string) (string, *gauge) {
	id, ok := m.runs[run]
	if !ok {
		return "", nil
	}
	return id, m.sessions[id]
}

// Run an event of the run belongs to, the child run for a sub-agent event and the run itself otherwise
func eventRun(run string, ev agent.Event) string {
	if ev.Depth > 0 && ev.Agent != "" {
		return childRun(run, ev.Agent)
	}
	return run
}

// Session id an event of the run is charged to, empty when no session is bound
func (m *Meter) SessionOf(run string, ev agent.Event) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, _ := m.bound(eventRun(run, ev))
	return id
}

// Records when a request leaves, the origin the first token of its session is timed from
func (m *Meter) Begin(a provider.Attempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, g := m.bound(a.Label); g != nil {
		g.reqStart = a.Started
	}
}

// Records one provider attempt in the totals and in the session of its run
func (m *Meter) Observe(a provider.Attempt) {
	m.mu.Lock()
	count(&m.t, &m.latencies, a)
	m.attempts = append(m.attempts, a)
	if len(m.attempts) > maxAttempts {
		m.attempts = m.attempts[len(m.attempts)-maxAttempts:]
	}
	session, g := m.bound(a.Label)
	snap := m.snapshot()
	if g != nil {
		count(&g.t, &g.latencies, a)
		snap = clone(g.t)
	}
	m.mu.Unlock()
	m.send(AttemptMsg{Attempt: a, Session: session, Telemetry: snap})
}

// Adds the attempt to the request counters, latencies and rate limit of the telemetry
func count(t *Telemetry, latencies *time.Duration, a provider.Attempt) {
	t.Requests++
	if a.Wait > 0 {
		t.Retries++
	}
	if a.Err != nil {
		t.Failures++
	}
	if a.RequestID != "" {
		t.LastRequestID = a.RequestID
	}
	t.LastLatency = a.Duration
	*latencies += a.Duration
	t.AvgLatency = *latencies / time.Duration(t.Requests)
	if a.RateLimit.Known() {
		t.RateLimit = a.RateLimit
	}
}

// Updates the totals and the session of the run with usage, cost, tools, context and throughput of an event
func (m *Meter) Apply(run string, ev agent.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := ev.Time
	if now.IsZero() {
		now = time.Now()
	}
	key := eventRun(run, ev)
	_, g := m.bound(key)
	switch ev.Type {
	case agent.EventTurnStart:
		if g != nil {
			g.start(now)
		}
	case agent.EventModel:
		if g != nil {
			g.delta(now, ev.Model)
		}
	case agent.EventTurnEnd:
		m.t.Usage = m.t.Usage.Add(ev.Usage)
		pricing := m.pricingFor(g, ev)
		cost := 0.0
		if pricing != nil {
			cost = pricing.Cost(ev.Usage)
			m.t.Cost += cost
			m.t.Costs[run] += cost
		}
		if g != nil {
			g.end(now, ev.Usage)
			if pricing != nil {
				g.t.Cost += cost
				g.t.Costs[key] += cost
			}
		}
	case agent.EventToolEnd:
		tokens := protocol.EstimateTokens(ev.Result.Content)
		used(&m.t, ev.Call.Name, tokens)
		if g != nil {
			used(&g.t, ev.Call.Name, tokens)
		}
	case agent.EventCompactEnd:
		if g != nil {
			g.t.Context = 0
		}
	case agent.EventRunEnd, agent.EventInterrupt:
		if g != nil {
			g.t.Streaming = false
		}
	}
}

// Counts one call of the tool and the tokens its result put into context
func used(t *Telemetry, name string, tokens int) {
	st := t.Tools[name]
	st.Calls++
	st.Tokens += tokens
	t.Tools[name] = st
	t.ToolTokens += tokens
}

// Opens a turn, clearing the throughput and first token time of the last one
func (g *gauge) start(now time.Time) {
	g.turnStart, g.reqStart, g.firstAt, g.deltas, g.outChars = now, time.Time{}, time.Time{}, 0, 0
	g.t.Streaming, g.t.TokensPerSecond, g.t.FirstToken = true, 0, 0
}

// Counts a streamed delta toward the first token time and the running throughput of the turn
func (g *gauge) delta(now time.Time, ev protocol.Event) {
	switch ev.Type {
	case protocol.EventTextDelta, protocol.EventThinkingDelta, protocol.EventToolCallDelta:
		g.deltas++
		if g.firstAt.IsZero() {
			g.firstAt = now
			g.t.FirstToken = now.Sub(g.origin())
		}
		g.outChars += len(ev.Delta)
		if el := now.Sub(g.firstAt).Seconds(); el > 0.25 {
			g.t.TokensPerSecond = float64(g.outChars) / 4 / el
		}
	}
}

// Closes the turn with its usage, the context it left, its cache share and its measured throughput
func (g *gauge) end(now time.Time, u protocol.Usage) {
	g.t.Streaming = false
	g.t.Usage = g.t.Usage.Add(u)
	g.t.Context = u.Context()
	if ctx := u.Context(); ctx > 0 {
		g.t.CacheRatio = float64(u.CacheRead) / float64(ctx)
	}
	if el := now.Sub(g.generating()).Seconds(); el > 0 && u.Output > 0 {
		g.t.TokensPerSecond = float64(u.Output) / el
	}
}

// Moment the turn is timed from, the request start when the provider reported one and the turn start before that
func (g *gauge) origin() time.Time {
	if g.reqStart.IsZero() || g.reqStart.Before(g.turnStart) {
		return g.turnStart
	}
	return g.reqStart
}

// Moment generation is timed from, the first token of a streamed turn and the request start of one that came whole
func (g *gauge) generating() time.Time {
	if g.deltas < 2 || g.firstAt.IsZero() {
		return g.origin()
	}
	return g.firstAt
}

// Pricing of the model that answered the turn when the catalog knows it, else the session or meter pricing
func (m *Meter) pricingFor(g *gauge, ev agent.Event) *profiles.Pricing {
	model, pricing := m.t.Model, m.pricing
	if g != nil {
		model, pricing = g.t.Model, g.pricing
	}
	if ev.Response != nil && ev.Response.Model != "" && ev.Response.Model != model {
		if known, ok := profiles.Describe(ev.Response.Model); ok && known.Pricing != nil {
			return known.Pricing
		}
	}
	return pricing
}

// Totals over every session, the context summed and the per session gauges left at zero
func (m *Meter) Snapshot() Telemetry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot()
}

func (m *Meter) snapshot() Telemetry {
	t := clone(m.t)
	for _, id := range m.order {
		g := m.sessions[id]
		t.Context += g.t.Context
		t.Priced = t.Priced || g.t.Priced
	}
	return t
}

// Telemetry of one session, false when the meter has never seen it
func (m *Meter) SnapshotOf(session string) (Telemetry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.sessions[session]
	if !ok {
		return Telemetry{}, false
	}
	return clone(g.t), true
}

// Ids of the sessions with telemetry in the order the meter first saw them
func (m *Meter) Sessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.order)
}

// Copy of the telemetry with maps of its own
func clone(t Telemetry) Telemetry {
	t.Costs = maps.Clone(t.Costs)
	t.Tools = maps.Clone(t.Tools)
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
