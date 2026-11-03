package cards

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Samples of throughput kept for the graph
const sampleLimit = 120

// Attempt log lines kept over every session
const noteLimit = 256

// Keymap context of the metrics panel
const metricsContext = "metrics"

// Registers the metrics panel context and its switch between the focused session and the totals
func init() {
	if err := keys.AddContext(metricsContext, "metrics panel"); err != nil {
		panic(err)
	}
	if err := keys.Add(keys.Action{ID: "metrics.total", Context: metricsContext, Label: "totals", Doc: "switch between the focused session and the totals over every session", Default: []string{"t"}}); err != nil {
		panic(err)
	}
}

// Model layer gauges of one session or of every session, context fill, throughput, cache, cost, latency and rate limits
type Telemetry struct {
	env *Env
	// Session shown, empty until the app shows one
	session string
	// Shows the totals over every session with a row each in place of the one session
	total   bool
	snap    harness.Telemetry
	samples []float64
	events  []meterNote
	timer   ticker
}

// Line of the attempt log with the session it came from, empty for an attempt of no session
type meterNote struct {
	at      time.Time
	session string
	text    string
}

func NewTelemetry(env *Env) *Telemetry {
	t := &Telemetry{env: env, timer: ticker{every: 250 * time.Millisecond}}
	t.reload()
	return t
}

func (c *Telemetry) Title() string { return "METRICS" }
func (c *Telemetry) Glyph() string { return c.env.Theme.Glyphs.Metrics }
func (c *Telemetry) Key() string   { return "metrics" }

// Restarts sampling a stream that went on while the card was closed and missed its tick
func (c *Telemetry) Init() tea.Cmd {
	c.timer.reset()
	c.reload()
	if c.streaming() {
		return c.timer.start()
	}
	return nil
}

func (c *Telemetry) Contexts() []string { return []string{metricsContext, keys.Panel} }

// Actions the panel answers, the switch named for the view it leads to
func (c *Telemetry) Hints() []wm.Hint {
	if c.total {
		return []wm.Hint{{Action: "metrics.total", Label: "session"}}
	}
	return []wm.Hint{{Action: "metrics.total"}}
}

// Telemetry as last seen
func (c *Telemetry) Snapshot() harness.Telemetry { return c.snap }

// Follows the session and rereads the meter, dropping the throughput history when the session changed
func (c *Telemetry) Show(session string) {
	if session != c.session {
		c.samples = c.samples[:0]
		c.session = session
	}
	c.reload()
}

// Rereads the meter, the totals in total mode and the shown session otherwise
func (c *Telemetry) reload() {
	if c.env.Meter == nil {
		return
	}
	if c.total {
		c.snap = c.env.Meter.Snapshot()
		return
	}
	c.snap, _ = c.env.Meter.SnapshotOf(c.session)
}

// Reports whether a turn streams that the panel follows, the shown session or any session in total mode
func (c *Telemetry) streaming() bool {
	if c.env.Meter == nil {
		return false
	}
	if !c.total {
		return c.snap.Streaming
	}
	for _, id := range c.env.Meter.Sessions() {
		if s, _ := c.env.Meter.SnapshotOf(id); s.Streaming {
			return true
		}
	}
	return false
}

// Session the meter charges an event to, empty when it charges none
func (c *Telemetry) sessionOf(msg harness.EventMsg) string {
	if c.env.Meter == nil {
		return ""
	}
	return c.env.Meter.SessionOf(msg.Run, msg.Event)
}

// Title of a session as the app names it
func (c *Telemetry) titleOf(id string) string {
	if c.env.SessionTitle != nil {
		return c.env.SessionTitle(id)
	}
	return id
}

func (c *Telemetry) note(session, s string) {
	c.events = append(c.events, meterNote{at: time.Now(), session: session, text: s})
	if len(c.events) > noteLimit {
		c.events = c.events[len(c.events)-noteLimit:]
	}
}

func (c *Telemetry) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.AttemptMsg:
		switch {
		case c.total:
			c.reload()
		case msg.Session == c.session:
			c.snap = msg.Telemetry
		}
		a := msg.Attempt
		switch {
		case a.Err != nil && a.Wait > 0:
			c.note(msg.Session, render.Warm(fmt.Sprintf("retry in %s after %d from %s", render.Elapsed(a.Wait), a.Status, urlHost(a.URL)), c.env.Theme))
		case a.Err != nil:
			c.note(msg.Session, render.Bad(render.Truncate(a.Err.Error(), 60, c.env.Theme)+" from "+urlHost(a.URL), c.env.Theme))
		default:
			c.note(msg.Session, render.Dim(fmt.Sprintf("%d in %s", a.Status, render.Elapsed(a.Duration)), c.env.Theme))
		}
	case harness.EventMsg:
		c.reload()
		if msg.Event.Type == agent.EventCompactEnd {
			c.note(c.sessionOf(msg), render.Warm("compacted", c.env.Theme))
		}
		if c.streaming() {
			return c, c.timer.start()
		}
	case wm.ActionMsg:
		if msg.ID == "metrics.total" {
			c.total = !c.total
			c.reload()
			if c.streaming() {
				return c, c.timer.start()
			}
		}
	case tickMsg:
		if !c.timer.owns(msg) {
			return c, nil
		}
		c.reload()
		if !c.total {
			c.samples = append(c.samples, c.snap.TokensPerSecond)
			if len(c.samples) > sampleLimit {
				c.samples = c.samples[len(c.samples)-sampleLimit:]
			}
		}
		if c.streaming() {
			return c, c.timer.start()
		}
	}
	return c, nil
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
	if c.total {
		return c.totals(w, h)
	}
	t := c.env.Theme
	if c.session == "" {
		return emptyView(w, h, "no session, "+c.env.keyed("metrics.total", "shows totals"), t)
	}
	s := c.snap
	var lines []string
	model := s.Model
	if model == "" {
		model = "no model"
	}
	head := render.Heading(c.titleOf(c.session), t) + " " + model
	if s.Provider != "" {
		head += " " + render.Dim("via "+s.Provider, t)
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
		lines = append(lines, render.KV("context", render.Tokens(s.Context), t)+render.Dim(" of an unknown window, set window on the profile so compaction can run", t))
	}
	tps := t.Glyphs.None
	if s.TokensPerSecond > 0 {
		tps = fmt.Sprintf("%.0f tok/s", s.TokensPerSecond)
	}
	row := []string{render.KV("rate", tps, t), render.KV("cache", render.Percent(s.CacheRatio)+render.Dim(" last, "+render.Percent(cachedShare(s.Usage))+" overall", t), t)}
	if s.Priced || s.Cost > 0 {
		row = append(row, render.KV("cost", render.Money(s.Cost), t))
	}
	lines = append(lines, render.Truncate(strings.Join(row, "  "), w, t))
	if h > 4 {
		lines = append(lines, c.graph(w))
	}
	lines = append(lines, c.counters(s, w)...)
	if s.RateLimit.Known() {
		lines = append(lines, render.Truncate(render.KV("limits", limits(s), t), w, t))
	}
	return c.finish(c.notes(lines, w, h), h)
}

// Totals over every session followed by a row per session, the shown one lit
func (c *Telemetry) totals(w, h int) string {
	t := c.env.Theme
	s := c.snap
	var ids []string
	if c.env.Meter != nil {
		ids = c.env.Meter.Sessions()
	}
	head := render.Heading("every session", t) + " " + render.Dim(render.Count(len(ids), "session"), t)
	lines := []string{render.Truncate(head, w, t)}
	row := []string{render.KV("context", render.Tokens(s.Context), t)}
	if s.Priced || s.Cost > 0 {
		row = append(row, render.KV("cost", render.Money(s.Cost), t))
	}
	lines = append(lines, render.Truncate(strings.Join(row, "  "), w, t))
	lines = append(lines, c.counters(s, w)...)
	cols := []render.Column{{Title: "SESSION", Flex: 2}, {Title: "MODEL", Flex: 1}, {Title: "CONTEXT", Right: true}, {Title: "OUT", Right: true}, {Title: "PROMPT", Right: true}, {Title: "CACHED", Right: true}, {Title: "COST", Right: true}}
	rows := make([][]string, 0, len(ids))
	sel := -1
	for i, id := range ids {
		st, _ := c.env.Meter.SnapshotOf(id)
		if id == c.session {
			sel = i
		}
		model := st.Model
		if model == "" {
			model = t.Glyphs.None
		}
		cost := t.Glyphs.None
		if st.Priced || st.Cost > 0 {
			cost = render.Money(st.Cost)
		}
		rows = append(rows, []string{c.titleOf(id), model, render.Tokens(st.Context), render.Tokens(st.Usage.Output), render.Tokens(st.Usage.Context()), render.Percent(cachedShare(st.Usage)), cost})
	}
	if room := h - len(lines); len(rows) > 0 && room > render.TableHead {
		table, _ := window(strings.Split(render.Table(cols, rows, w, sel, t), "\n"), max(sel, 0), room)
		lines = append(lines, table...)
	}
	return c.finish(c.notes(lines, w, h), h)
}

// Usage by kind, tool shares, latency and request counters of the telemetry
func (c *Telemetry) counters(s harness.Telemetry, w int) []string {
	t := c.env.Theme
	u := s.Usage
	lines := []string{render.Truncate(render.KV("in", render.Tokens(u.Input), t)+"  "+render.KV("out", render.Tokens(u.Output), t)+"  "+render.KV("cached", render.Tokens(u.CacheRead), t)+"  "+render.KV("written", render.Tokens(u.CacheWrite), t), w, t)}
	if len(s.Tools) > 0 {
		lines = append(lines, render.Truncate(render.KV("tools", toolShares(s), t), w, t))
	}
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
	if s.LastRequestID != "" {
		reqs += "  " + render.Dim(render.Truncate(s.LastRequestID, 24, t), t)
	}
	return append(lines, render.Truncate(reqs, w, t))
}

// Requests and tokens left in each rate limit window the endpoint reported
func limits(s harness.Telemetry) string {
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
	return strings.Join(parts, " ")
}

// Lines followed by the newest log lines that fit, of the shown session or every session in total mode
func (c *Telemetry) notes(lines []string, w, h int) []string {
	t := c.env.Theme
	var shown []string
	for _, n := range c.events {
		if !c.total && n.session != c.session {
			continue
		}
		text := n.at.Format("15:04:05") + " "
		if c.total && n.session != "" {
			text += render.Dim(render.Truncate(c.titleOf(n.session), 16, t), t) + " "
		}
		shown = append(shown, text+n.text)
	}
	room := h - len(lines)
	if room > 1 && len(shown) > 0 {
		lines = append(lines, render.Divider(min(w, 30), t))
		for _, e := range shown[max(0, len(shown)-(room-1)):] {
			lines = append(lines, render.Truncate(e, w, t))
		}
	}
	return lines
}

// Lines cut to the height and joined
func (c *Telemetry) finish(lines []string, h int) string {
	if len(lines) > h {
		lines = lines[:max(h, 0)]
	}
	return strings.Join(lines, "\n")
}

// Tools by estimated result tokens, largest first, each with its call count
func toolShares(s harness.Telemetry) string {
	names := slices.SortedFunc(maps.Keys(s.Tools), func(a, b string) int {
		if d := s.Tools[b].Tokens - s.Tools[a].Tokens; d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, 0, len(names)+1)
	parts = append(parts, render.Tokens(s.ToolTokens)+" total")
	for _, name := range names {
		st := s.Tools[name]
		parts = append(parts, fmt.Sprintf("%s %s (%d)", name, render.Tokens(st.Tokens), st.Calls))
	}
	return strings.Join(parts, "  ")
}

func (c *Telemetry) Status() string {
	s := c.snap
	if s.Streaming && s.TokensPerSecond > 0 {
		return fmt.Sprintf("%.0f tok/s", s.TokensPerSecond)
	}
	if s.Cost > 0 {
		return render.Money(s.Cost)
	}
	return render.Tokens(s.Usage.Output) + " out"
}

// Share of every prompt token of a usage that was served from cache
func cachedShare(u protocol.Usage) float64 {
	if n := u.Context(); n > 0 {
		return float64(u.CacheRead) / float64(n)
	}
	return 0
}

// Host of an endpoint URL, the text itself when it does not parse
func urlHost(s string) string {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return s
}
