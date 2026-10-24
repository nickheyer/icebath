package cards

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Samples of throughput kept for the graph
const sampleLimit = 120

type telemetryTick struct{}

// Model layer gauges, context fill, throughput, cache, cost, latency, retries and rate limits
type Telemetry struct {
	env     *Env
	snap    harness.Telemetry
	samples []float64
	events  []string
	ticking bool
	w, h    int
}

func NewTelemetry(env *Env) *Telemetry {
	t := &Telemetry{env: env}
	if env.Meter != nil {
		t.snap = env.Meter.Snapshot()
	}
	return t
}

func (c *Telemetry) Title() string { return "METRICS" }
func (c *Telemetry) Glyph() string { return c.env.Theme.Glyphs.Metrics }
func (c *Telemetry) Key() string   { return "metrics" }
func (c *Telemetry) Init() tea.Cmd { return nil }

func (c *Telemetry) Contexts() []string { return []string{keys.Panel} }

// Telemetry as last seen
func (c *Telemetry) Snapshot() harness.Telemetry { return c.snap }

// Rereads the meter and drops the throughput history, for when the session behind it changed
func (c *Telemetry) Reload() {
	if c.env.Meter != nil {
		c.snap = c.env.Meter.Snapshot()
	}
	c.samples = c.samples[:0]
}

func (c *Telemetry) note(s string) {
	c.events = append(c.events, c.env.now().Format("15:04:05")+" "+s)
	if len(c.events) > 50 {
		c.events = c.events[len(c.events)-50:]
	}
}

func (c *Telemetry) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.AttemptMsg:
		c.snap = msg.Telemetry
		a := msg.Attempt
		switch {
		case a.Err != nil && a.Wait > 0:
			c.note(render.Warm(fmt.Sprintf("retry in %s after %d", render.Elapsed(a.Wait), a.Status), c.env.Theme))
		case a.Err != nil:
			c.note(render.Bad(render.Truncate(a.Err.Error(), 60, c.env.Theme), c.env.Theme))
		default:
			c.note(render.Dim(fmt.Sprintf("%d in %s", a.Status, render.Elapsed(a.Duration)), c.env.Theme))
		}
	case harness.FailoverMsg:
		c.note(render.Bad(fmt.Sprintf("failover past model %d: %s", msg.From, render.Truncate(msg.Err.Error(), 40, c.env.Theme)), c.env.Theme))
		return c, wm.Notify("failover", wm.Warn)
	case harness.ServedMsg:
		if msg.Index > 0 {
			c.note(render.Warm(fmt.Sprintf("served by fallback model %d", msg.Index), c.env.Theme))
		}
	case harness.EventMsg:
		if c.env.Meter != nil {
			c.snap = c.env.Meter.Snapshot()
		}
		switch msg.Event.Type {
		case agent.EventTurnStart:
			if !c.ticking {
				c.ticking = true
				return c, c.tick()
			}
		case agent.EventCompactEnd:
			c.note(render.Warm("compacted", c.env.Theme))
		}
	case telemetryTick:
		if c.env.Meter != nil {
			c.snap = c.env.Meter.Snapshot()
		}
		c.samples = append(c.samples, c.snap.TokensPerSecond)
		if len(c.samples) > sampleLimit {
			c.samples = c.samples[len(c.samples)-sampleLimit:]
		}
		if c.snap.Streaming {
			return c, c.tick()
		}
		c.ticking = false
	}
	return c, nil
}

func (c *Telemetry) tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return telemetryTick{} })
}

// Bars of the throughput history scaled to the tallest sample
func (c *Telemetry) graph(w int) string {
	if w <= 0 {
		return ""
	}
	t := c.env.Theme
	peak := 1.0
	for _, s := range c.samples {
		peak = max(peak, s)
	}
	from := max(0, len(c.samples)-w)
	shades := lipgloss.Blend1D(max(w, 2), t.Pulse, t.Ice)
	var sb strings.Builder
	for i := 0; i < w; i++ {
		j := from + i
		if j >= len(c.samples) {
			sb.WriteString(" ")
			continue
		}
		level := int(c.samples[j] / peak * float64(len(t.Glyphs.Ramp)-1))
		ch := string(t.Glyphs.Ramp[min(max(level, 0), len(t.Glyphs.Ramp)-1)])
		sb.WriteString(lipgloss.NewStyle().Foreground(shades[i]).Render(ch))
	}
	return sb.String()
}

func (c *Telemetry) View(w, h int) string {
	c.w, c.h = w, h
	t := c.env.Theme
	s := c.snap
	var lines []string
	model := s.Model
	if model == "" {
		model = "no model"
	}
	head := render.Heading(model, t)
	if s.Provider != "" {
		head += " " + render.Dim("via "+s.Provider, t)
	}
	if s.Serving > 0 {
		head += " " + render.Warm(fmt.Sprintf("fallback %d", s.Serving), t)
	}
	if s.Streaming {
		head += " " + render.Cool("streaming", t)
	}
	lines = append(lines, render.Truncate(head, w, t))
	limit := s.ContextLimit
	if s.Threshold > 0 && (limit == 0 || s.Threshold < limit) {
		limit = s.Threshold
	}
	if limit > 0 {
		frac := float64(s.Context) / float64(limit)
		label := fmt.Sprintf("%s / %s", render.Tokens(s.Context), render.Tokens(limit))
		if s.Threshold > 0 && s.Threshold < s.ContextLimit {
			label += render.Dim(fmt.Sprintf(" compacts, window %s", render.Tokens(s.ContextLimit)), t)
		}
		lines = append(lines, render.Gauge(frac, max(w-len(label)-10, 4), t)+" "+render.Percent(frac)+" "+label)
	} else {
		lines = append(lines, render.KV("context", render.Tokens(s.Context), t))
	}
	tps := t.Glyphs.None
	if s.TokensPerSecond > 0 {
		tps = fmt.Sprintf("%.0f tok/s", s.TokensPerSecond)
	}
	row := []string{render.KV("rate", tps, t), render.KV("cache", render.Percent(s.CacheRatio), t)}
	if s.Priced || s.Cost > 0 {
		row = append(row, render.KV("cost", render.Money(s.Cost), t))
	}
	lines = append(lines, render.Truncate(strings.Join(row, "  "), w, t))
	if h > 4 {
		lines = append(lines, c.graph(w))
	}
	u := s.Usage
	lines = append(lines, render.Truncate(render.KV("in", render.Tokens(u.Input), t)+"  "+render.KV("out", render.Tokens(u.Output), t)+"  "+render.KV("cached", render.Tokens(u.CacheRead), t)+"  "+render.KV("written", render.Tokens(u.CacheWrite), t), w, t))
	lat := render.KV("latency", render.Elapsed(s.LastLatency), t)
	if s.AvgLatency > 0 {
		lat += render.Dim(" avg "+render.Elapsed(s.AvgLatency), t)
	}
	if s.FirstToken > 0 {
		lat += render.Dim(" first "+render.Elapsed(s.FirstToken), t)
	}
	lines = append(lines, render.Truncate(lat, w, t))
	reqs := render.KV("requests", fmt.Sprint(s.Requests), t)
	if s.Retries > 0 {
		reqs += "  " + render.Warm(fmt.Sprintf("%d retries", s.Retries), t)
	}
	if s.Failures > 0 {
		reqs += "  " + render.Bad(fmt.Sprintf("%d failed", s.Failures), t)
	}
	if s.Failovers > 0 {
		reqs += "  " + render.Bad(fmt.Sprintf("%d failovers", s.Failovers), t)
	}
	if s.LastRequestID != "" {
		reqs += "  " + render.Dim(render.Truncate(s.LastRequestID, 24, t), t)
	}
	lines = append(lines, render.Truncate(reqs, w, t))
	if s.RateLimit.Known() {
		rl := s.RateLimit
		var parts []string
		if rl.Requests > 0 {
			parts = append(parts, fmt.Sprintf("req %d/%d", rl.RequestsRemaining, rl.Requests))
		}
		if rl.Tokens > 0 {
			parts = append(parts, fmt.Sprintf("tok %s/%s", render.Tokens(rl.TokensRemaining), render.Tokens(rl.Tokens)))
		}
		if rl.InputTokens > 0 {
			parts = append(parts, fmt.Sprintf("in %s/%s", render.Tokens(rl.InputRemaining), render.Tokens(rl.InputTokens)))
		}
		if rl.OutputTokens > 0 {
			parts = append(parts, fmt.Sprintf("out %s/%s", render.Tokens(rl.OutputRemaining), render.Tokens(rl.OutputTokens)))
		}
		if rl.Reset > 0 {
			parts = append(parts, "reset "+render.Elapsed(rl.Reset))
		}
		lines = append(lines, render.Truncate(render.KV("limits", strings.Join(parts, " "), t), w, t))
	}
	room := h - len(lines)
	if room > 1 && len(c.events) > 0 {
		lines = append(lines, render.Divider(min(w, 30), t))
		from := max(0, len(c.events)-(room-1))
		for _, e := range c.events[from:] {
			lines = append(lines, render.Truncate(e, w, t))
		}
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (c *Telemetry) Status() string {
	s := c.snap
	if s.Streaming && s.TokensPerSecond > 0 {
		return fmt.Sprintf("%.0f tok/s", s.TokensPerSecond)
	}
	if s.Cost > 0 {
		return render.Money(s.Cost)
	}
	return render.Tokens(s.Usage.Total())
}
