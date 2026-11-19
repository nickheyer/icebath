//go:build !icebath_core && !icebath_noextras

package tui

import (
	"maps"
	"reflect"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
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

// Binds the arena, its keymap context and the /arena command
func init() {
	newArenaView = func(a *app) arenaView { return newArenaFeed(a) }
	tierKeys = append(tierKeys, arena.RegisterKeys)
	tierCommands = append(tierCommands, func(a *app) []slashCommand {
		return []slashCommand{{name: arena.Name, help: "watch the agents of the session work on a voxel platform, with their tickets, log and replay", run: func(r *slashRun) tea.Cmd {
			return a.openArena()
		}}}
	})
}

// Feed of an app with an empty journal
func newArenaFeed(a *app) *arenaFeed {
	return &arenaFeed{a: a, journal: arena.NewJournal(), turnTools: map[string]int{}, agents: map[string]*agentToolLog{}, cfg: arena.Default()}
}

// Counts the tool starts and the turn endings of a gateway event before the store takes it
func (f *arenaFeed) observe(ev gatewayEventMsg) {
	sid := f.a.store.ui.Sid
	if ev.SessionID != "" && sid != "" && ev.SessionID != sid {
		return
	}
	p := ev.Payload
	switch ev.Type {
	case tuigateway.ToolStartEvent.Name():
		start, _ := tuigateway.ToolStartEvent.Payload(p)
		f.turnTools[firstText(start.Name, "tool")]++
	case tuigateway.SubagentToolEvent.Name():
		tool, _ := tuigateway.SubagentToolEvent.Payload(p)
		id := subagentKey(tool)
		log := f.agents[id]
		if log == nil {
			log = &agentToolLog{counts: map[string]int{}}
			f.agents[id] = log
		}
		name := firstText(val(tool.ToolName), "delegate_task")
		log.counts[name]++
		log.last, log.at = name, f.a.store.now()
	case "message.complete":
		if done, _ := tuigateway.MessageCompleteEvent.Payload(p); val(done.Status) == "error" && !val(done.Partial) {
			f.outcome, f.failure = arena.OutcomeFailed, describeTurnFailure(done)
		} else {
			f.outcome, f.failure = arena.OutcomeDone, ""
		}
	case "error":
		stamped := infoOf(f.a.store.ui.Info).ExecutionGeneration != nil
		if failure, _ := tuigateway.ErrorEvent.Payload(p); f.a.store.ui.Busy && !(isGenericErrorEvent(ev) && stamped) {
			f.outcome, f.failure = arena.OutcomeFailed, firstText(failure.Message, "the turn failed")
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
	r := arena.Report{Session: ui.Sid, At: s.now(), Busy: ui.Busy, Prompt: f.a.lastUserMsg, Model: infoText(infoOf(ui.Info).Model), Streaming: s.turn.Streaming != "",
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
	usage := usageOf(ui.Usage)
	r.Usage = arena.Usage{Input: int(usage.Input), Output: int(usage.Output), Reasoning: int(usage.Reasoning), Cost: val(usage.CostUsd)}
	r.Context, r.ContextMax = int(val(usage.ContextUsed)), int(val(usage.ContextMax))
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

// Repaints an open arena in the session theme
func (f *arenaFeed) retheme(t uitheme.Theme) {
	if f.card != nil {
		f.card.Retheme(t)
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
	f := a.arena.(*arenaFeed)
	var cmds []tea.Cmd
	if !a.graphicsProbed {
		a.graphicsProbed = true
		cmds = append(cmds, a.graphics.Probe())
	}
	f.sync()
	if f.card == nil || !a.wm.Has(arena.Name) {
		env := &cards.Env{Theme: a.toolkit, Keys: a.keys, Ctx: a.ctx, WorkDir: a.store.sessionCwd, Graphics: a.graphics, Send: a.post}
		f.card = arena.New(env, f.journal, f, f.cfg, f.cfgErr)
	}
	ws := a.wm.Active()
	cmds = append(cmds, a.wm.Open(ws, f.card))
	if ws != nil && ws.Zoomed() != wm.Card(f.card) {
		cmds = append(cmds, a.wm.ToggleZoom())
	}
	cmds = append(cmds, ask(a.events.rpc, a.sys, tuigateway.DelegationStatus, tuigateway.ProfileParams{}, func(r *tuigateway.DelegationStatusResult) tea.Cmd {
		if r != nil {
			a.store.applyDelegationStatus(*r)
		}
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
	params := tuigateway.SubagentIDParams{SessionID: a.store.ui.Sid, SubagentID: m.ID}
	return ask(a.events.rpc, a.sys, tuigateway.SubagentInterrupt, params, func(r *tuigateway.SubagentInterruptResult) tea.Cmd {
		switch {
		case r == nil:
			return nil
		case !r.Found:
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
	params := tuigateway.SubagentSteerParams{SessionID: a.store.ui.Sid, SubagentID: m.ID, Text: text}
	return ask(a.events.rpc, a.sys, tuigateway.SubagentSteer, params, func(r *tuigateway.SubagentSteerResult) tea.Cmd {
		if r == nil {
			return nil
		}
		if r.Status == "rejected" {
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
	return call(a.gw, tuigateway.SubagentTail, tuigateway.SubagentIDParams{SessionID: a.store.ui.Sid, SubagentID: m.ID}, func(r *tuigateway.SubagentTailResult, err error) tea.Cmd {
		switch {
		case err != nil:
			return wm.Notify(rpcErrorMessage(err), wm.Error)
		case !r.Available || r.Text == "":
			return wm.Notify(m.Name+" has written no output yet", wm.Info)
		}
		return a.page(r.Text, m.Name+" · "+truncateEnd(m.Task, 40))
	})
}

// Pauses or resumes spawning of new agents
func (f *arenaFeed) Pause(paused bool) tea.Cmd {
	a := f.a
	return ask(a.events.rpc, a.sys, tuigateway.DelegationPause, tuigateway.DelegationPauseParams{Paused: &paused}, func(r *tuigateway.DelegationPauseResult) tea.Cmd {
		if r == nil {
			return nil
		}
		a.store.applyDelegationPause(r.Paused)
		if r.Paused {
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
