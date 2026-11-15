package tui

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl/arena"
)

// Subagents running side by side in a stock harness, until delegation.status reports the setting
const defaultConcurrentChildren = 3

// Tool starts of one subagent by name, with the newest and when it started
type agentToolLog struct {
	counts map[string]int
	last   string
	at     time.Time
}

// Feed of the arena, the journal of every turn and the tool starts the store does not count
type arenaFeed struct {
	a         *app
	journal   *arena.Journal
	card      arena.Card
	graphics  *gfx.Terminal
	probed    bool
	turnTools map[string]int
	agents    map[string]*agentToolLog
	busy      bool
	outcome   arena.Outcome
	failure   string
	raw       any
	parsed    bool
	cfg       arena.Config
	cfgErr    error
}

// Ordered handoff of messages to the program that never blocks the caller, for senders inside the update loop
type mailbox struct {
	mu    sync.Mutex
	queue []tea.Msg
	wake  chan struct{}
}

// Mailbox delivering through send on its own goroutine until the context ends
func newMailbox(ctx context.Context, send func(tea.Msg)) *mailbox {
	m := &mailbox{wake: make(chan struct{}, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			}
			m.mu.Lock()
			batch := m.queue
			m.queue = nil
			m.mu.Unlock()
			for _, msg := range batch {
				send(msg)
			}
		}
	}()
	return m
}

// Queues a message behind the ones before it and returns at once
func (m *mailbox) post(msg tea.Msg) {
	m.mu.Lock()
	m.queue = append(m.queue, msg)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Feed of an app with an empty journal and the terminal read from the environment
func newArenaFeed(a *app) *arenaFeed {
	return &arenaFeed{a: a, journal: arena.NewJournal(), graphics: gfx.New(), turnTools: map[string]int{}, agents: map[string]*agentToolLog{}, cfg: arena.Default()}
}

// Id the store gives a subagent of an event payload
func subagentKey(p map[string]any) string {
	if id, _ := p["subagent_id"].(string); id != "" {
		return id
	}
	label, _ := p["goal"].(string)
	if label == "" {
		label = "subagent"
	}
	return "sa:" + jsString(p["task_index"]) + ":" + label
}

// Counts the tool starts and the turn endings of a gateway event before the store takes it
func (f *arenaFeed) observe(ev gatewayEventMsg) {
	sid := f.a.store.ui.Sid
	if ev.SessionID != "" && sid != "" && ev.SessionID != sid {
		return
	}
	p := ev.Payload
	switch ev.Type {
	case "tool.start":
		f.turnTools[stringOr(p["name"], "tool")]++
	case "subagent.tool":
		id := subagentKey(p)
		log := f.agents[id]
		if log == nil {
			log = &agentToolLog{counts: map[string]int{}}
			f.agents[id] = log
		}
		name := stringOr(p["tool_name"], "delegate_task")
		log.counts[name]++
		log.last, log.at = name, f.a.store.now()
	case "message.complete":
		if p["status"] == "error" && !truthyJS(p["partial"]) {
			f.outcome, f.failure = arena.OutcomeFailed, describeTurnFailure(p)
		} else {
			f.outcome, f.failure = arena.OutcomeDone, ""
		}
	case "error":
		_, stamped := field(f.a.store.ui.Info, "execution_generation")
		if f.a.store.ui.Busy && !(isGenericErrorEvent(ev) && stamped) {
			f.outcome, f.failure = arena.OutcomeFailed, stringOr(p["message"], "the turn failed")
		}
	}
}

// Report of the session as the store holds it now, with the counts the feed kept
func (f *arenaFeed) report() arena.Report {
	s := f.a.store
	ui := s.ui
	if ui.Busy && !f.busy {
		f.turnTools, f.outcome, f.failure = map[string]int{}, arena.OutcomeRunning, ""
	}
	f.busy = ui.Busy
	r := arena.Report{Session: ui.Sid, At: s.now(), Busy: ui.Busy, Prompt: f.a.lastUserMsg, Model: infoText(ui.Info, "model"), Streaming: s.turn.Streaming != "",
		Paused: s.delegation.Paused, Thinking: lastLine(s.turn.Reasoning), Outcome: f.outcome, Failure: f.failure}
	if !ui.Busy && f.a.turn.interrupted || !ui.Busy && f.outcome == arena.OutcomeRunning {
		r.Outcome = arena.OutcomeStopped
	}
	if n := len(s.turn.Tools); n > 0 {
		r.Tools.Current, r.Tools.Since = s.turn.Tools[n-1].Name, s.turn.Tools[n-1].StartedAt
	}
	r.Tools.Counts = maps.Clone(f.turnTools)
	for _, t := range s.turn.Todos {
		r.Todos = append(r.Todos, arena.Todo{ID: t.ID, Parent: t.Parent, Content: t.Content, Status: t.Status})
	}
	for _, sa := range s.currentAgentRoster() {
		r.Agents = append(r.Agents, f.agentOf(sa))
	}
	if o := s.overlay.Approval; o != nil {
		r.Approval = strings.TrimSpace(o.Description + " " + o.Command)
	}
	if o := s.overlay.Clarify; o != nil {
		for _, q := range o.Questions {
			if _, answered := o.Answers[q.QID]; !answered {
				r.Clarify = q.Question
				break
			}
		}
	}
	num := func(k string) float64 { v, _ := ui.Usage[k].(float64); return v }
	r.Usage = arena.Usage{Input: int(num("input")), Output: int(num("output")), Reasoning: int(num("reasoning")), Cost: num("cost_usd")}
	r.Context, r.ContextMax = int(num("context_used")), int(num("context_max"))
	return r
}

// Last line of a text that holds any, trimmed
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Agent of the journal from the progress the store keeps and the tool starts the feed counted
func (f *arenaFeed) agentOf(sa subagentProgress) arena.Agent {
	ag := arena.Agent{ID: sa.ID, Parent: sa.ParentID, Goal: sa.Goal, Status: sa.Status, Model: sa.Model, Summary: sa.Summary, Depth: sa.Depth, ToolCount: sa.ToolCount,
		Thinking: sa.Thinking, Notes: sa.Notes, FilesRead: sa.FilesRead, FilesWritten: sa.FilesWritten, Toolsets: sa.Toolsets}
	for _, o := range sa.OutputTail {
		ag.Output = append(ag.Output, o.Tool+": "+o.Preview)
	}
	tokens := func(v *float64) int {
		if v == nil {
			return 0
		}
		return int(*v)
	}
	ag.Usage = arena.Usage{Input: tokens(sa.InputTokens), Output: tokens(sa.OutputTokens), Reasoning: tokens(sa.ReasoningTokens)}
	if sa.CostUSD != nil {
		ag.Usage.Cost = *sa.CostUSD
	}
	if sa.StartedAt != nil {
		ag.Started = time.UnixMilli(int64(*sa.StartedAt))
	}
	if sa.DurationSeconds != nil {
		ag.Duration = time.Duration(*sa.DurationSeconds * float64(time.Second))
	}
	if log := f.agents[sa.ID]; log != nil {
		ag.Tools = arena.Tools{Current: log.last, Since: log.at, Counts: maps.Clone(log.counts)}
	}
	return ag
}

// Folds the session into the journal, follows the settings and refreshes an open card
func (f *arenaFeed) sync() {
	f.journal.Observe(f.report())
	raw := f.a.store.ui.Arena
	slots := defaultConcurrentChildren
	if n := f.a.store.delegation.MaxConcurrentChildren; n != nil && *n >= 1 {
		slots = int(*n)
	}
	if !f.parsed || !reflect.DeepEqual(raw, f.raw) || slots != f.cfg.Team.Builders {
		f.parsed, f.raw = true, raw
		f.cfg, f.cfgErr = arena.Parse(raw)
		f.cfg.Team.Builders = slots
		if f.card != nil {
			f.card.Configure(f.cfg, f.cfgErr)
		}
	}
	if f.card != nil {
		f.card.Refresh()
	}
}

// Takes a terminal answer about graphics, repainting when what the terminal draws changed
func (f *arenaFeed) terminal(m tea.Msg) {
	if f.graphics.Update(m) {
		f.a.wm.Repaint()
	}
}

// Passes terminal focus changes to an open arena that the window manager does not focus
func (f *arenaFeed) focus(m tea.Msg) {
	if f.card != nil && f.a.wm.Focused() != wm.Card(f.card) {
		f.card.Update(m)
	}
}

// Opens the arena card zoomed over the stage of the workspace in view, focusing it when it is open already
func (a *app) openArena() tea.Cmd {
	f := a.arena
	var cmds []tea.Cmd
	if !f.probed {
		f.probed = true
		cmds = append(cmds, f.graphics.Probe())
	}
	f.sync()
	if f.card == nil || !a.wm.Has(arena.Name) {
		env := &cards.Env{Theme: a.toolkit, Keys: a.keys, Ctx: a.ctx, WorkDir: a.store.sessionCwd, Graphics: f.graphics, Send: a.post}
		f.card = arena.New(env, f.journal, f, f.cfg, f.cfgErr)
	}
	ws := a.wm.Active()
	cmds = append(cmds, a.wm.Open(ws, f.card))
	if ws != nil && ws.Zoomed() != wm.Card(f.card) {
		cmds = append(cmds, a.wm.ToggleZoom())
	}
	cmds = append(cmds, a.events.rpc("delegation.status", map[string]any{}, func(r map[string]any) tea.Cmd {
		a.store.applyDelegationStatus(r)
		return nil
	}))
	return tea.Batch(cmds...)
}

// Interrupts a subagent, or the turn for the director
func (f *arenaFeed) Interrupt(s *arena.Snapshot, m arena.Member) tea.Cmd {
	a := f.a
	if m.ID == "" {
		return a.interruptTurn()
	}
	return a.events.rpc("subagent.interrupt", map[string]any{"session_id": a.store.ui.Sid, "subagent_id": m.ID}, func(r map[string]any) tea.Cmd {
		switch {
		case r == nil:
			return nil
		case !truthyJS(r["found"]):
			return wm.Notify(m.Name+" already finished", wm.Info)
		}
		return wm.Notify(m.Name+" interrupted", wm.Success)
	})
}

// Sends guidance to a running subagent, steers the turn of the main agent, or prompts it while it is idle
func (f *arenaFeed) Steer(s *arena.Snapshot, m arena.Member, text string) tea.Cmd {
	a := f.a
	if m.ID == "" {
		if a.store.ui.Busy {
			return a.slash("/steer "+text, nil, false)
		}
		return a.submit(text)
	}
	return a.events.rpc("subagent.steer", map[string]any{"session_id": a.store.ui.Sid, "subagent_id": m.ID, "text": text}, func(r map[string]any) tea.Cmd {
		if r == nil {
			return nil
		}
		if r["status"] == "rejected" {
			return wm.Notify(m.Name+" took no guidance, it already finished", wm.Warn)
		}
		f.journal.Guide(s.Sprint, m.Name, text, a.store.now())
		return wm.Notify("guidance queued for "+m.Name+" at its next tool boundary", wm.Success)
	})
}

// Shows the newest output of a subagent in the pager, or focuses the session of the main agent
func (f *arenaFeed) Open(s *arena.Snapshot, m arena.Member) tea.Cmd {
	a := f.a
	if m.ID == "" {
		return a.wm.Focus(a.card)
	}
	return gatewayCall(a.gw, "subagent.tail", map[string]any{"session_id": a.store.ui.Sid, "subagent_id": m.ID}, func(v any, err error) tea.Cmd {
		r := asRPCResult(v)
		text, _ := r["text"].(string)
		switch {
		case err != nil:
			return wm.Notify(rpcErrorMessage(err), wm.Error)
		case !truthyJS(r["available"]) || text == "":
			return wm.Notify(m.Name+" has written no output yet", wm.Info)
		}
		return a.page(text, m.Name+" · "+truncateEnd(m.Task, 40))
	})
}

// Pauses or resumes spawning of new agents
func (f *arenaFeed) Pause(paused bool) tea.Cmd {
	a := f.a
	return a.events.rpc("delegation.pause", map[string]any{"paused": paused}, func(r map[string]any) tea.Cmd {
		if r == nil {
			return nil
		}
		a.store.applyDelegationStatus(map[string]any{"paused": r["paused"]})
		if truthyJS(r["paused"]) {
			return wm.Notify(tr("slashCmd.ops.agents.paused"), wm.Info)
		}
		return wm.Notify(tr("slashCmd.ops.agents.resumed"), wm.Info)
	})
}

// Interrupts the turn while it runs, else every agent of the turn still running
func (f *arenaFeed) Stop(s *arena.Snapshot) tea.Cmd {
	a := f.a
	if a.store.ui.Busy && s.Newest {
		return a.interruptTurn()
	}
	var cmds []tea.Cmd
	for _, m := range s.Members {
		if m.ID != "" && (m.Status == "running" || m.Status == "queued") {
			cmds = append(cmds, f.Interrupt(s, m))
		}
	}
	return tea.Batch(cmds...)
}

// Runs the newest prompt of the session again
func (f *arenaFeed) Retry() tea.Cmd { return f.a.slash("/retry", nil, false) }

var _ arena.Harness = (*arenaFeed)(nil)
