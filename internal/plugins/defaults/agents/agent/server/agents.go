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
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Rows of one kind /agents lists before it says how many more there are
const agentRows = 20

// What /agents says: the agents running, the background processes and delegations; or the spawn pause set, or how delegation stands
func (a *Agent) Agents(ctx context.Context, id, argument string) (string, error) {
	switch word := strings.ToLower(strings.TrimSpace(argument)); word {
	case "pause", "resume":
		if a.tree.setPaused(word == "pause") {
			return "delegation · paused", nil
		}
		return "delegation · resumed", nil
	case "status":
		return a.delegationStatus(ctx, id)
	case "":
	default:
		return "", errors.New("Usage: /agents [pause|resume|status]")
	}
	now := time.Now()
	lines := []string{"🤖 **Active Agents & Tasks**", ""}
	var busy []string
	for _, live := range a.Live() {
		if !live.GetBusy() {
			continue
		}
		row := "`" + live.GetSessionId() + "` · running"
		a.mu.Lock()
		started := a.life.turns[live.GetSessionId()]
		a.mu.Unlock()
		if !started.IsZero() {
			row += " · " + terminal.ShortUptime(int(now.Sub(started).Seconds()))
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
			row += " · this chat"
		}
		busy = append(busy, row)
	}
	lines = append(lines, fmt.Sprintf("**Active agents:** %d", len(busy)))
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
				processes = append(processes, fmt.Sprintf("- `%s` · %s · `%s`", view.ID, terminal.ShortUptime(int(now.Sub(view.Started).Seconds())),
					strs.Clip(strings.Join(strings.Fields(view.Command), " "), 90)))
			}
		}
	}
	lines = append(lines, "", fmt.Sprintf("**Running background processes:** %d", len(processes)))
	lines = append(lines, capped(processes)...)
	units := a.async.listed()
	if len(units) > 0 {
		lines = append(lines, "", fmt.Sprintf("**Background delegations:** %d", len(units)))
		lines = append(lines, capped(units)...)
	}
	if len(busy) == 0 && len(processes) == 0 && len(units) == 0 {
		lines = append(lines, "", "No active agents or running tasks.")
	}
	return strings.Join(lines, "\n"), nil
}

// Rows up to the cap, the rest counted
func capped(rows []string) []string {
	if len(rows) <= agentRows {
		return rows
	}
	return append(slices.Clone(rows[:agentRows]), fmt.Sprintf("... and %d more", len(rows)-agentRows))
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
			doing := cmp.Or(c.doing, "between turns")
			c.mu.Unlock()
			children = append(children, fmt.Sprintf("  - child %d: %d api calls · %s · active %ds ago", c.index, f.calls, doing,
				int(time.Since(f.last).Seconds())))
		}
		if quiet := time.Since(latest); !latest.IsZero() && quiet >= time.Minute {
			row += fmt.Sprintf(" · quiet %ds", int(quiet.Seconds()))
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
	state := "active"
	if status.GetPaused() {
		state = "paused"
	}
	lines := []string{fmt.Sprintf("delegation · %s · caps d%d/%d", state, status.GetMaxSpawnDepth(), status.GetMaxConcurrentChildren())}
	if failed := status.GetFailed(); len(failed) > 0 {
		lines = append(lines, fmt.Sprintf("failed delegations · %d", len(failed)))
		for _, f := range failed {
			lines = append(lines, fmt.Sprintf("  ✗ %s · %s · %s", cmp.Or(f.GetGoal(), "subagent"), f.GetStatus(), cmp.Or(f.GetError(), "-")))
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
		lines := []string{"**Delegation backends**", ""}
		for _, info := range a.delegation.List(settings) {
			mark := ""
			if info.Name == current {
				mark = " ✓"
			}
			line := "- `" + info.Name + "`" + mark + " — " + info.Title
			if info.Summary != "" {
				line += ": " + info.Summary
			}
			if !info.Available {
				line += " (unavailable: " + info.Reason + ")"
			}
			lines = append(lines, line)
		}
		return strings.Join(append(lines, "", "Usage: `/delegation <backend>`"), "\n"), nil
	}
	if _, reason := a.delegation.Status(settings, name); reason != "" {
		return "", errors.New(reason)
	}
	if err := a.config.In(r.profileName()).Set(delegationconfig.Backend, name); err != nil {
		return "", fmt.Errorf("Could not save the delegation backend: %w", err)
	}
	return "Delegation backend set to `" + name + "`; delegate_task is built for it from the next message on.", nil
}

// Ends the subagents the session's spawn tree runs and its background delegations, as /stop asks; how many it ended
func (d delegates) Stop(session string) int {
	stopped := d.async.stop(session)
	for _, c := range d.owned(d.ctx, session) {
		if c.interrupt("stopped by /stop") {
			stopped++
		}
	}
	return stopped
}

// What /steer says: the text queued for the running turn after its next tool results, or sent as the next message when no turn runs
func (a *Agent) SteerMessage(ctx context.Context, id, text string) (string, error) {
	if text = strings.TrimSpace(text); text == "" {
		return "", errors.New("usage: /steer <prompt>")
	}
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	if ok, _ := a.steer(r, text); ok {
		return "steer queued — arrives after next tool call: \"" + steerPreview(text) + "\"", nil
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
	return "no active turn — queued for next: \"" + steerPreview(text) + "\"", nil
}

// What /busy says: what a message sent while the session's agent works does, or that it changed for the session
func (a *Agent) Busy(ctx context.Context, id, argument string) (string, error) {
	r, err := a.run(id)
	if err != nil {
		return "", err
	}
	switch mode := strings.ToLower(strings.TrimSpace(argument)); mode {
	case "", "status":
		return "busy input mode: " + a.settings(r).GetString(agentconfig.BusyInputMode), nil
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
		return "busy input mode: " + mode, nil
	}
	return "", errors.New("usage: /busy [queue|steer|interrupt|status]")
}

// Start of the text a steer's reply quotes
func steerPreview(text string) string {
	if runes := []rune(text); len(runes) > 50 {
		return string(runes[:50]) + "…"
	}
	return text
}
