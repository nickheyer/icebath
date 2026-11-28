package slash

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct {
	host *terminal.Terminal
	// Tools of every plugin, whose background work of a session /stop ends with its processes
	tools *tools.Registry
	// Settings and session stores, whose titles name the sessions /processes lists the processes under
	config *config.Config
	stores *sessions.Registry
}

func Register(registry *slash.Registry, cfg *config.Config, stores *sessions.Registry, host *terminal.Terminal, toolset *tools.Registry) {
	registry.Register(commands{host: host, tools: toolset, config: cfg, stores: stores})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{
		{Name: "processes", Aliases: []string{"ps"}, Usage: "[id|kill <id>]", Help: "See and stop background processes", Run: c.processes},
		{Name: "stop", Help: "Stop this session's processes and subagents", Run: c.stop},
	}
}

// What /processes takes, said when it is given something else
var errUsage = errors.New("/processes takes an id to show its output, or kill and an id, such as /processes kill proc_ab12cd.")

// Lists every background process under the title of its session, this session's first, shows one's output, or kills one, in whichever
// session it runs: what the processes screen of the app does, by the same rules. That is for the person at this computer; typed in a
// session of any other origin, such as one a chat app, a schedule or another program started, it lists, shows and kills the processes of
// that session alone
func (c commands) processes(ctx context.Context, call *slash.Call) (string, error) {
	scope := ""
	if !c.own(ctx, call.SessionID) {
		scope = call.SessionID
	}
	switch words := strings.Fields(call.Argument); {
	case len(words) == 0:
		return c.list(ctx, call.SessionID, scope), nil
	case len(words) == 2 && words[0] == "kill":
		return c.kill(ctx, words[1], scope)
	case len(words) == 1 && words[0] != "kill":
		return c.read(words[0], scope)
	}
	return "", errUsage
}

// Origins of the sessions a person works in at this computer: the app, and icebath chat at a terminal, with one question or many. A
// session of a subagent, of a schedule, of a chat app or of a program that names an origin of its own is none of them
var here = map[string]bool{"": true, "tui": true, "cli": true, "oneshot": true, "local": true, "desktop": true}

// Whether the session is one the person at this computer works in, who may see and kill the processes of every session; false for one
// that can't be read
func (c commands) own(ctx context.Context, session string) bool {
	store, err := c.stores.Store(c.config.GetString(sessionsconfig.Store))
	if err != nil {
		return false
	}
	found, err := store.Get(ctx, session)
	return err == nil && here[strings.ToLower(strings.TrimSpace(found.GetSource()))]
}

// The process of the id as the command may see it: any for the person at this computer, else one of the session alone
func (c commands) find(id, scope string) (terminal.View, bool) {
	view, ok := c.host.Jobs.Find(id)
	if !ok || scope != "" && view.Session != scope {
		return terminal.View{}, false
	}
	return view, true
}

func (c commands) list(ctx context.Context, session, scope string) string {
	views := order(c.host.Jobs.Views(scope))
	if len(views) == 0 {
		return "No background processes. Commands the agent leaves running show here."
	}
	// This session first, then the others in the order of their first process
	var ids []string
	for _, view := range views {
		if !slices.Contains(ids, view.Session) {
			ids = append(ids, view.Session)
		}
	}
	if at := slices.Index(ids, session); at > 0 {
		ids = slices.Insert(slices.Delete(ids, at, at+1), 0, session)
	}
	titles := c.titles(ctx, ids)
	var out strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&out, "**%s**\n", heading(id, titles))
		for _, view := range views {
			if view.Session != id {
				continue
			}
			fmt.Fprintf(&out, "- `%s` · %s · `%s`\n", view.ID, state(view), oneLine(view.Command, 80))
			if line := lastLine(view.Tail); line != "" {
				fmt.Fprintf(&out, "  Last output: %s\n", oneLine(line, 120))
			}
		}
	}
	out.WriteString("\n/processes and an id shows the output of one, and /processes kill and an id kills it.")
	return out.String()
}

// Processes in the order a person looks for them: running ones first, the longest running first, then those that ended, the latest first
func order(views []terminal.View) []terminal.View {
	views = slices.Clone(views)
	slices.SortStableFunc(views, func(a, b terminal.View) int {
		ra, rb := a.Status == "running", b.Status == "running"
		switch {
		case ra && !rb:
			return -1
		case rb && !ra:
			return 1
		case ra:
			return a.Started.Compare(b.Started)
		}
		return b.Ended.Compare(a.Ended)
	})
	return views
}

// Titles of the sessions by id, read from the store the settings name; one it cannot read has none
func (c commands) titles(ctx context.Context, ids []string) map[string]string {
	titles := map[string]string{}
	store, err := c.stores.Store(c.config.GetString(sessionsconfig.Store))
	if err != nil {
		return titles
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if session, err := store.Get(ctx, id); err == nil {
			titles[id] = session.GetTitle()
		}
	}
	return titles
}

// Heading of the processes of a session: its title, or what stands for a session that has none
func heading(id string, titles map[string]string) string {
	switch {
	case id == "":
		return "Started outside a session"
	case titles[id] != "":
		return titles[id]
	}
	return "Untitled session"
}

// Length of time in words in the largest whole unit it has passed, such as 5 minutes
func span(length time.Duration) string {
	if length < time.Second {
		return "under a second"
	}
	for _, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if length >= unit {
			return strs.Span(length.Truncate(unit))
		}
	}
	return strs.Span(length.Truncate(time.Second))
}

// How the process stands: running and for how long, or how it ended and how long ago
func state(view terminal.View) string {
	if view.Status == "running" {
		return "Running for " + span(time.Since(view.Started))
	}
	ago := span(time.Since(view.Ended)) + " ago"
	switch {
	case view.Reason == "killed":
		return "Killed " + ago
	case view.Reason == "lost":
		return "Lost " + ago
	case view.Reason == "failed_start":
		return "Failed to start " + ago
	case view.Code != nil:
		return fmt.Sprintf("Ended %s with exit code %d", ago, *view.Code)
	}
	return "Ended " + ago
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

// Sentence saying no process has the id
func absent(id string) error {
	return fmt.Errorf("There is no background process %s.", id)
}

func (c commands) read(id, scope string) (string, error) {
	view, ok := c.find(id, scope)
	if !ok {
		return "", absent(id)
	}
	head := fmt.Sprintf("`%s` · %s · `%s`", view.ID, state(view), oneLine(view.Command, 80))
	if strings.TrimSpace(view.Tail) == "" {
		return head + "\n\nNo output yet.", nil
	}
	return head + "\n\n```\n" + strings.TrimRight(view.Tail, "\n") + "\n```", nil
}

// Kills the process the command may see, in whichever session it runs, by the same call as the processes screen of the app
func (c commands) kill(ctx context.Context, id, scope string) (string, error) {
	view, ok := c.find(id, scope)
	if !ok {
		return "", absent(id)
	}
	report := c.host.Jobs.Kill(ctx, nil, "", view.ID, "process.kill", false)
	switch report.Status {
	case "killed":
		return "Killed " + view.ID + ".", nil
	case "already_exited":
		return view.ID + " had already ended.", nil
	case "not_found":
		return "", absent(view.ID)
	}
	return "", fmt.Errorf("Could not kill %s. %s", view.ID, strs.Full(cmp.Or(report.Error, "The reason is not known.")))
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
