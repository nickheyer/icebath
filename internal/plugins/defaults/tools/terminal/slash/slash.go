package slash

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
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
		{Name: "processes", Aliases: []string{"ps"}, Usage: "[id|kill <id>]", Help: "List the background processes of the session, read their output or kill one", Run: c.processes},
		{Name: "stop", Help: "Kill all running background processes of the session, and the subagents running for it", Run: c.stop},
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
		return "No background processes in this session."
	}
	var out strings.Builder
	for _, view := range views {
		fmt.Fprintf(&out, "- `%s` %s · `%s`\n", view.ID, state(view), oneLine(view.Command, 80))
		if line := lastLine(view.Tail); line != "" {
			fmt.Fprintf(&out, "  last: %s\n", oneLine(line, 120))
		}
	}
	out.WriteString("\n`/processes <id>` shows one's output, `/processes kill <id>` kills it.")
	return out.String()
}

// How the process stands: running and for how long, or how it ended and how long ago
func state(view terminal.View) string {
	if view.Status == "running" {
		return "running · " + terminal.ShortUptime(int(time.Since(view.Started).Seconds()))
	}
	verdict := "done"
	switch {
	case view.Reason == "killed", view.Reason == "lost":
		verdict = view.Reason
	case view.Reason == "failed_start", view.Code != nil && *view.Code != 0:
		verdict = "failed"
	}
	if view.Code != nil && view.Reason != "killed" && view.Reason != "lost" {
		verdict += fmt.Sprintf(" · exit %d", *view.Code)
	}
	return verdict + " · " + terminal.ShortUptime(int(time.Since(view.Ended).Seconds())) + " ago"
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
		return "Killed " + view.ID + ".", nil
	case "already_exited":
		return view.ID + " had already exited.", nil
	case "not_found":
		return "", fmt.Errorf("%s is no longer a process of this session.", view.ID)
	}
	return "", fmt.Errorf("Could not kill %s: %s", view.ID, report.Error)
}

// Kills every running background process of the session and ends the background work its tools keep, such as running subagents
func (c commands) stop(ctx context.Context, call *slash.Call) (string, error) {
	killed := c.host.Jobs.KillAll(ctx, call.SessionID, "process.stop", false)
	text := fmt.Sprintf("Stopped %d background processes.", killed)
	if killed == 1 {
		text = "Stopped 1 background process."
	}
	switch ended := c.tools.Stop(call.SessionID); ended {
	case 0:
	case 1:
		text += " Interrupted 1 subagent or background delegation; its partial result still returns."
	default:
		text += fmt.Sprintf(" Interrupted %d subagents or background delegations; their partial results still return.", ended)
	}
	return text, nil
}
