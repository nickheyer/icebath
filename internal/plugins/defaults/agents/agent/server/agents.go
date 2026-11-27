package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/delegation"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Rows of one kind /agents lists before it says how many more there are
const agentRows = 20

// What /agents says: the agents running, the background processes and delegations; or the spawn pause set, or how delegation stands
func (a *Agent) Agents(ctx context.Context, id, argument string) (string, error) {
	switch word := strings.ToLower(strings.TrimSpace(argument)); word {
	case "pause", "resume":
		if a.tree.setPaused(word == "pause") {
			return "New subagents are paused. Type /agents resume to let them start again.", nil
		}
		return "New subagents may start again.", nil
	case "status":
		return a.delegationStatus(ctx, id)
	case "":
	default:
		return "", errors.New("Usage: /agents [pause|resume|status]")
	}
	now := time.Now()
	lines := []string{"**Agents and tasks at work**", ""}
	var busy []string
	for _, live := range a.Live() {
		if !live.GetBusy() {
			continue
		}
		row := "`" + live.GetSessionId() + "` · replying"
		a.mu.Lock()
		started := a.life.turns[live.GetSessionId()]
		a.mu.Unlock()
		if !started.IsZero() {
			row += " for " + strs.Span(rounded(now.Sub(started)))
		}
		r, err := a.run(live.GetSessionId())
		if err == nil {
			if _, route, err := a.current(r); err == nil && route.Model != "" {
				row += " · `" + route.Model + "`"
			}
			r.mu.Lock()
			doing, sub := r.doing, r.child
			r.mu.Unlock()
			if sub != nil {
				row += " · subagent " + sub.id
			}
			if doing != "" {
				row += " · " + doing
			}
		}
		if live.GetSessionId() == id {
			row += " · this session"
		}
		busy = append(busy, row)
	}
	lines = append(lines, "**Agents replying:** "+strs.Number(float64(len(busy))))
	for i, row := range capped(busy) {
		if i < agentRows {
			row = fmt.Sprintf("%d. %s", i+1, row)
		}
		lines = append(lines, row)
	}
	var processes []string
	if a.terminal != nil {
		for _, view := range a.terminal.Jobs.Views("") {
			if view.Status == "running" {
				processes = append(processes, fmt.Sprintf("- `%s` · running for %s · `%s`", view.ID, strs.Span(rounded(now.Sub(view.Started))),
					strs.Clip(strings.Join(strings.Fields(view.Command), " "), 90)))
			}
		}
	}
	lines = append(lines, "", "**Background processes running:** "+strs.Number(float64(len(processes))))
	lines = append(lines, capped(processes)...)
	units := a.async.listed()
	if len(units) > 0 {
		lines = append(lines, "", "**Subagents working in the background:** "+strs.Number(float64(len(units))))
		lines = append(lines, capped(units)...)
	}
	if len(busy) == 0 && len(processes) == 0 && len(units) == 0 {
		lines = append(lines, "", "No agent, subagent or background process is working right now.")
	}
	return strings.Join(lines, "\n"), nil
}

// Rows up to the cap, the rest counted
func capped(rows []string) []string {
	if len(rows) <= agentRows {
		return rows
	}
	return append(slices.Clone(rows[:agentRows]), "And "+strs.Number(float64(len(rows)-agentRows))+" more")
}

// Lines of the background units running: their id and goal, how long they went quiet, and each subagent's calls and step
func (g *background) listed() []string {
	g.mu.Lock()
	units := make([]*unit, 0, len(g.units))
	for _, u := range g.units {
		units = append(units, u)
	}
	g.mu.Unlock()
	slices.SortFunc(units, func(x, y *unit) int { return strings.Compare(x.id, y.id) })
	var lines []string
	for _, u := range units {
		row := "- `" + u.id + "` · running"
		var latest time.Time
		var children []string
		for _, c := range u.b.children {
			if c == nil {
				continue
			}
			f := c.fingerprint()
			latest = maxTime(latest, f.last)
			c.mu.Lock()
			doing := cmp.Or(c.doing, "between steps")
			c.mu.Unlock()
			children = append(children, fmt.Sprintf("  - subagent %d: %s · %s · last active %s ago", c.index, strs.Count(f.calls, "model call"), doing,
				strs.Span(rounded(time.Since(f.last)))))
		}
		if quiet := time.Since(latest); !latest.IsZero() && quiet >= time.Minute {
			row += " · quiet for " + strs.Span(rounded(quiet))
		}
		if len(u.b.tasks) > 0 {
			row += " · " + strs.Clip(strings.Join(strings.Fields(u.b.tasks[0].goal), " "), 70)
		}
		lines = append(lines, row)
		lines = append(lines, children...)
	}
	return lines
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// How delegation stands for the session: active or paused, the depth and parallel caps, and its failed background tasks
func (a *Agent) delegationStatus(ctx context.Context, id string) (string, error) {
	status, err := a.Subagents(ctx, id)
	if err != nil {
		return "", err
	}
	state := "may start"
	if status.GetPaused() {
		state = "are paused"
	}
	lines := []string{fmt.Sprintf("New subagents %s. They nest up to %s deep, with at most %s running at once.", state,
		strs.Number(float64(status.GetMaxSpawnDepth())), strs.Number(float64(status.GetMaxConcurrentChildren())))}
	if failed := status.GetFailed(); len(failed) > 0 {
		lines = append(lines, "", "**Failed subagents:** "+strs.Number(float64(len(failed))))
		for _, f := range failed {
			lines = append(lines, fmt.Sprintf("- %s · %s · %s", cmp.Or(f.GetGoal(), "Subagent"), f.GetStatus(), cmp.Or(f.GetError(), "No reason given")))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// What /delegation says: every backend and whether it can run, or the backend of the profile set to another
func (a *Agent) Delegation(id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	settings := a.settings(r)
	name := strings.ToLower(strings.TrimSpace(argument))
	if name == "" {
		current := delegation.Configured(settings)
		lines := []string{"**Where subagents run**", ""}
		for _, info := range a.delegation.List(settings) {
			line := "- `" + info.Name + "` — " + info.Title
			if info.Summary != "" {
				line += ": " + info.Summary
			}
			if info.Name == current {
				line += " · In use"
			}
			if !info.Available {
				line += " · Can't be used now. " + sentence(info.Reason)
			}
			lines = append(lines, line)
		}
		return strings.Join(append(lines, "", "Usage: /delegation [name]"), "\n"), nil
	}
	if _, reason := a.delegation.Status(settings, name); reason != "" {
		return "", errors.New(reason)
	}
	if err := a.config.In(r.profileName()).Set(delegationconfig.Backend, name); err != nil {
		return "", err
	}
	return "Subagents run with `" + name + "` from your next message on.", nil
}

// Ends the subagents the session's spawn tree runs and its background delegations, as /stop asks; how many it ended
func (d delegates) Stop(session string) int {
	stopped := d.async.stop(session)
	for _, c := range d.owned(d.ctx, session) {
		if c.interrupt("You stopped it with /stop.") {
			stopped++
		}
	}
	return stopped
}

// What /steer says: the text queued for the running turn after its next tool results, or sent as the next message when no turn runs
func (a *Agent) SteerMessage(ctx context.Context, id, text string) (string, error) {
	if text = strings.TrimSpace(text); text == "" {
		return "", errors.New("Usage: /steer <message>")
	}
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	if ok, _ := a.steer(r, text); ok {
		return "The agent gets your message after its next tool call: “" + steerPreview(text) + "”.", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := a.blocked(ctx, r); err != nil {
		return "", err
	}
	a.admit(r, text)
	r.queue = append(r.queue, queued{text: text})
	if !r.busy {
		if err := a.start(r); err != nil {
			return "", err
		}
	}
	return "No reply is running, so your message is sent as a new one: “" + steerPreview(text) + "”.", nil
}

// What /busy says: what a message sent while the session's agent works does, or that it changed for the session
func (a *Agent) Busy(ctx context.Context, id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	switch mode := strings.ToLower(strings.TrimSpace(argument)); mode {
	case "", "status":
		return "Enter while busy is set to " + busyTitle(a.settings(r).GetString(agentconfig.BusyInputMode)) + ".", nil
	case agentconfig.BusyQueue, agentconfig.BusySteer, agentconfig.BusyInterrupt:
		store, err := a.store()
		if err != nil {
			return "", err
		}
		r.mu.Lock()
		merged := r.overrides.Merge(map[string]any{agentconfig.BusyInputMode: mode}, nil)
		r.mu.Unlock()
		if err := store.Update(ctx, id, merged.Change()); err != nil {
			return "", err
		}
		r.mu.Lock()
		r.overrides = merged
		r.mu.Unlock()
		// The windows show the mode while the agent works
		a.changed(r)
		return "Enter while busy is now " + busyTitle(mode) + " for this session.", nil
	}
	return "", errors.New("Usage: /busy [queue|steer|interrupt|status]")
}

// Title of what enter does while the agent works, as the settings screen shows it
func busyTitle(mode string) string {
	switch mode {
	case agentconfig.BusySteer:
		return "Steer"
	case agentconfig.BusyInterrupt:
		return "Interrupt"
	}
	return "Queue it"
}

// Start of the text a steer's reply quotes
func steerPreview(text string) string {
	if runes := []rune(text); len(runes) > 50 {
		return string(runes[:50]) + "…"
	}
	return text
}
