package slash

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct {
	host *terminal.Terminal
	// Tools of every plugin, whose background work of a session /stop ends with its processes
	tools *tools.Registry
}

func Register(registry *slash.Registry, host *terminal.Terminal, toolset *tools.Registry) {
	registry.Register(commands{host: host, tools: toolset})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{
		{Name: "processes", Aliases: []string{"ps"}, Usage: "[id|kill <id>]", Help: "See and stop this session's background processes", Run: c.processes},
		{Name: "stop", Help: "Stop this session's processes and subagents", Run: c.stop},
	}
}

// Lists the session's background processes, shows one's output, or kills one
func (c commands) processes(ctx context.Context, call *slash.Call) (string, error) {
	words := strings.Fields(call.Argument)
	switch {
	case len(words) == 0:
		return c.list(call.SessionID), nil
	case len(words) == 2 && words[0] == "kill":
		return c.kill(ctx, call.SessionID, words[1])
	case len(words) == 1:
		return c.read(call.SessionID, words[0])
	}
	return "", fmt.Errorf("Usage: /processes [id|kill <id>]")
}

func (c commands) list(session string) string {
	views := c.host.Jobs.Views(session)
	if len(views) == 0 {
		return "This session has no background processes. One appears here when the agent runs a command in the background."
	}
	var out strings.Builder
	for _, view := range views {
		fmt.Fprintf(&out, "- `%s` %s · `%s`\n", view.ID, state(view), oneLine(view.Command, 80))
		if line := lastLine(view.Tail); line != "" {
			fmt.Fprintf(&out, "  Last line: %s\n", oneLine(line, 120))
		}
	}
	out.WriteString("\n/processes <id> shows the output of one, and /processes kill <id> stops it.")
	return out.String()
}

// Length of time in words in the largest whole unit it has passed, such as 5 minutes
func span(length time.Duration) string {
	unit := time.Second
	switch {
	case length < time.Second:
		return "under a second"
	case length >= 24*time.Hour:
		unit = 24 * time.Hour
	case length >= time.Hour:
		unit = time.Hour
	case length >= time.Minute:
		unit = time.Minute
	}
	return strs.Span(length.Truncate(unit))
}

// How the process stands: running and for how long, or how it ended and how long ago
func state(view terminal.View) string {
	if view.Status == "running" {
		return "Running for " + span(time.Since(view.Started))
	}
	verdict := "Finished"
	switch {
	case view.Reason == "killed":
		verdict = "Stopped"
	case view.Reason == "lost":
		verdict = "Lost"
	case view.Reason == "failed_start", view.Code != nil && *view.Code != 0:
		verdict = "Failed"
	}
	if view.Code != nil && view.Reason != "killed" && view.Reason != "lost" {
		verdict += fmt.Sprintf(" with exit code %d", *view.Code)
	}
	return verdict + ", " + span(time.Since(view.Ended)) + " ago"
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

func lastLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// The session's process of the id or unique prefix
func (c commands) find(session, id string) (terminal.View, error) {
	var found []terminal.View
	for _, view := range c.host.Jobs.Views(session) {
		if view.ID == id {
			return view, nil
		}
		if strings.HasPrefix(view.ID, id) || strings.HasPrefix(view.ID, "proc_"+id) {
			found = append(found, view)
		}
	}
	if len(found) != 1 {
		return terminal.View{}, fmt.Errorf("%s is no longer a process of this session.", id)
	}
	return found[0], nil
}

func (c commands) read(session, id string) (string, error) {
	view, err := c.find(session, id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(view.Tail) == "" {
		return view.ID + " has written no output yet.", nil
	}
	return fmt.Sprintf("`%s` %s · `%s`\n\n```\n%s\n```", view.ID, state(view), oneLine(view.Command, 80), strings.TrimRight(view.Tail, "\n")), nil
}

func (c commands) kill(ctx context.Context, session, id string) (string, error) {
	view, err := c.find(session, id)
	if err != nil {
		return "", err
	}
	report := c.host.Jobs.Kill(ctx, nil, session, view.ID, "process.kill", false)
	switch report.Status {
	case "killed":
		return "Stopped " + view.ID + ".", nil
	case "already_exited":
		return view.ID + " had already finished.", nil
	case "not_found":
		return "", fmt.Errorf("%s is no longer a process of this session.", view.ID)
	}
	return "", fmt.Errorf("Could not stop %s: %s.", view.ID, strings.TrimSuffix(report.Error, "."))
}

// Kills every running background process of the session and ends the background work its tools keep, such as running subagents
func (c commands) stop(ctx context.Context, call *slash.Call) (string, error) {
	killed := c.host.Jobs.KillAll(ctx, call.SessionID, "process.stop", false)
	ended := c.tools.Stop(call.SessionID)
	var said []string
	if killed > 0 {
		said = append(said, "Stopped "+strs.Count(killed, "background process")+".")
	}
	switch ended {
	case 0:
	case 1:
		said = append(said, "Stopped 1 subagent, and what it did so far still comes back.")
	default:
		said = append(said, "Stopped "+strs.Count(ended, "subagent")+", and what they did so far still comes back.")
	}
	if len(said) == 0 {
		return "Nothing was running in this session.", nil
	}
	return strings.Join(said, " "), nil
}
