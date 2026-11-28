package app

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/internal/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the list of background processes and of the output of one
const processesContext = "tui.processes"

var processKeys = []keys.Action{
	{ID: "tui.processes.read", Context: processesContext, Label: "read", Doc: "Read the output of the background process", Default: []string{"enter"}},
	{ID: "tui.processes.kill", Context: processesContext, Label: "kill", Doc: "Kill the background process", Default: []string{"x"}},
	{ID: "tui.processes.kill.all", Context: processesContext, Label: "kill all", Doc: "Kill the running background processes of the session in hand",
		Default: []string{"X"}},
}

// How often an open list or output page reads the processes again, so times and output stay current
const refreshEvery = 2 * time.Second

// What /processes takes, said when it is given something else
const usage = "/processes takes an id to show its output, or kill and an id, such as /processes kill proc_ab12cd."

// Screen of the background processes of every session: listed under the titles of their sessions, their output read, killed
func processScreen(conn *connect.Client) tui.Screen {
	processes := terminalv1connect.NewProcessServiceClient(conn)
	sessions := sessionsv1connect.NewSessionServiceClient(conn)
	return tui.Screen{
		ID:      "processes",
		Group:   tui.GroupSessions,
		Order:   40,
		Title:   "Background processes",
		Short:   "See and stop background processes",
		Slash:   []string{"processes", "ps"},
		Usage:   "[id|kill <id>]",
		Actions: processKeys,
		Where:   map[string]string{processesContext: "background processes"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			r := &roster{kit: kit, processes: processes, sessions: sessions, titles: map[string]string{}}
			if shown, ok := kit.Focused().(tui.Sessioned); ok {
				r.first = shown.Session()
			}
			switch words := strings.Fields(argument); {
			case len(words) == 0:
				return r.open(nil)
			case len(words) == 2 && words[0] == "kill":
				return r.with(words[1], func(p *terminalv1.Process) tea.Cmd { return r.ask(p, r.list.Confirm) })
			case len(words) == 1 && words[0] != "kill":
				return r.with(words[0], func(p *terminalv1.Process) tea.Cmd { return r.page(p.GetId()) })
			}
			return wm.Notify(usage, wm.Warn)
		},
	}
}

// How a process stands
type standing int

const (
	running standing = iota
	done
	failed
	killed
	lost
)

// How the process stands: running until it exited, then by how it ended or its exit code
func status(p *terminalv1.Process) standing {
	if p.GetStatus() != "exited" {
		return running
	}
	switch p.GetCompletionReason() {
	case "failed_start":
		return failed
	case "killed":
		return killed
	case "lost":
		return lost
	}
	if p.GetHasExitCode() && p.GetExitCode() != 0 {
		return failed
	}
	return done
}

// Length of time in words, rounded down to its largest whole unit: 45 seconds, 3 minutes, 1 hour, 2 days
func span(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	for _, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if d >= unit {
			return config.Span(d.Truncate(unit))
		}
	}
	return config.Span(d.Truncate(time.Second))
}

// What became of a process that ended, as the start of a sentence: "Ended 20 seconds ago with exit code 1", "Killed 3 minutes ago"
func ended(p *terminalv1.Process, since time.Duration) string {
	ago := span(since) + " ago"
	switch status(p) {
	case killed:
		return "Killed " + ago
	case lost:
		return "Lost " + ago
	}
	if p.GetCompletionReason() == "failed_start" {
		return "Failed to start " + ago
	}
	if p.GetHasExitCode() {
		return "Ended " + ago + " with exit code " + strconv.Itoa(int(p.GetExitCode()))
	}
	return "Ended " + ago
}

// Text on one line, its runs of blanks made one space
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// Last line of the output that is not blank
func lastLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := oneLine(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// What a question calls a process: its command, cut short as a value, or what stands for one with none
func name(p *terminalv1.Process) string {
	command := oneLine(p.GetCommand())
	if command == "" {
		return p.GetId()
	}
	if ansi.StringWidth(command) > 48 {
		command = ansi.Truncate(command, 47, "") + "…"
	}
	return "“" + command + "”"
}

// Sentence saying no process has the id
func absent(id string) string {
	return "There is no background process " + id + "."
}

// Background processes of every session in a list under the titles of their sessions, the session of the chat in hand first, read again
// while the list or an output page shows. Enter reads the output of the process in hand, x kills it and X kills every running process
// of its session, each after a question
type roster struct {
	kit       *tui.Kit
	processes terminalv1connect.ProcessServiceClient
	sessions  sessionsv1connect.SessionServiceClient
	// Session of the chat in hand, whose processes come first
	first string
	hub   *cards.Hub
	list  *cards.List
	// Processes in the order shown: by session, running ones first within each
	rows []*terminalv1.Process
	// Titles of the sessions by id, read once for each session the processes name, and again while one has none
	titles map[string]string
	// When the processes were last read, so times count on between two reads
	read    time.Time
	loaded  bool
	err     error
	polling bool
}

// Opens the list over the screen, then runs then once the processes were first read
func (r *roster) open(then func() tea.Cmd) tea.Cmd {
	t := r.kit.Env.Theme
	r.list = cards.NewList(r.kit.Env, cards.ListSpec{
		Title:   "Background processes",
		Glyph:   t.Glyphs.Jobs,
		Key:     "processes",
		Context: processesContext,
		Primary: "tui.processes.read",
		Columns: []render.Column{{Flex: 1}, {}},
		Rows: func() [][]string {
			rows := make([][]string, len(r.rows))
			for i, p := range r.rows {
				rows[i] = []string{cmp.Or(oneLine(p.GetCommand()), "Background process"), r.state(p)}
			}
			return rows
		},
		Group:  func(i int) string { return r.heading(r.rows[i].GetSessionId()) },
		Detail: r.detail,
		Empty:  r.empty,
		Free:   map[string]bool{"hub.refresh": true},
		Hints:  r.hints,
		OnFocus: func(*cards.List) tea.Cmd {
			return tea.Batch(r.load(nil), r.poll())
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"tui.processes.read":     func(_ *cards.List, i int) tea.Cmd { return r.page(r.rows[i].GetId()) },
			"tui.processes.kill":     func(l *cards.List, i int) tea.Cmd { return r.ask(r.rows[i], l.Confirm) },
			"tui.processes.kill.all": func(l *cards.List, i int) tea.Cmd { return r.askAll(r.rows[i].GetSessionId(), l.Confirm) },
			"hub.refresh":            func(*cards.List, int) tea.Cmd { return r.load(nil) },
		},
	})
	r.list.ToggleDetail()
	r.hub = r.kit.Hub(r.list.Key(), r.list)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.load(then))
}

// Opens the list, then runs act with the process the id names, or the one whose id starts with it, in hand; says so when no process has
// the id
func (r *roster) with(id string, act func(p *terminalv1.Process) tea.Cmd) tea.Cmd {
	return r.open(func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			found, err := r.processes.List(r.kit.Env.Ctx, &terminalv1.ListRequest{Id: id})
			return func() tea.Cmd {
				switch {
				case err != nil:
					return r.hub.Fail("Could not read the background processes. " + strs.Full(tui.Reason(err)))
				case len(found.GetProcesses()) == 0:
					return r.hub.Fail(absent(id))
				}
				p := found.GetProcesses()[0]
				r.hold(p.GetId())
				return act(p)
			}
		})
	})
}

// Puts the process of the id in hand, when it is listed
func (r *roster) hold(id string) {
	for i, p := range r.rows {
		if p.GetId() == id {
			r.list.Select(i)
			return
		}
	}
}

// Buttons for the process in hand: reading its output, killing it while it runs, and killing every running process of its session when
// that is more than killing this one
func (r *roster) hints(_ *cards.List, i int) []wm.Hint {
	if i < 0 || i >= len(r.rows) {
		return nil
	}
	p := r.rows[i]
	out := []wm.Hint{{Action: "tui.processes.read"}}
	if status(p) == running {
		out = append(out, wm.Hint{Action: "tui.processes.kill"})
	}
	if n := r.runningIn(p.GetSessionId()); n > 1 || n == 1 && status(p) != running {
		out = append(out, wm.Hint{Action: "tui.processes.kill.all"})
	}
	return out
}

// How many processes of the session still run
func (r *roster) runningIn(session string) int {
	n := 0
	for _, p := range r.rows {
		if p.GetSessionId() == session && status(p) == running {
			n++
		}
	}
	return n
}

// What the list says while it has no rows: what it waits for, what failed and how to try again, or what would be here
func (r *roster) empty() string {
	switch {
	case r.err != nil:
		return "Could not read the background processes. " + strs.Full(tui.Reason(r.err)) + " " + r.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !r.loaded:
		return "Reading the background processes…"
	}
	return "No background processes. Commands the agent leaves running show here."
}

// Title of a session, or what stands for one that has none
func (r *roster) named(id string) string {
	switch {
	case id == "":
		return "Started outside a session"
	case r.titles[id] != "":
		return r.titles[id]
	}
	return "Untitled session"
}

// Heading of the processes of a session: its title, with the start of its id where another session listed reads the same
func (r *roster) heading(id string) string {
	said := r.named(id)
	for _, p := range r.rows {
		if other := p.GetSessionId(); other != id && r.named(other) == said {
			return said + " · " + id[:min(8, len(id))]
		}
	}
	return said
}

// The session a question names: by its heading, or as those started outside one
func (r *roster) of(session string) string {
	if session == "" {
		return "started outside a session"
	}
	return "of “" + r.heading(session) + "”"
}

// Time passed since the processes were read, so the times they report count on between two reads
func (r *roster) passed() time.Duration {
	if r.read.IsZero() {
		return 0
	}
	return max(time.Since(r.read), 0)
}

// How long the process ran, counted on to now while it runs
func (r *roster) uptime(p *terminalv1.Process) time.Duration {
	up := time.Duration(p.GetUptimeSeconds()) * time.Second
	if status(p) == running {
		up += r.passed()
	}
	return up
}

// Time since the process ended, counted on to now
func (r *roster) since(p *terminalv1.Process) time.Duration {
	return time.Duration(p.GetSinceExitSeconds())*time.Second + r.passed()
}

// State of the process in a few words, colored by how it stands
func (r *roster) state(p *terminalv1.Process) string {
	t := r.kit.Env.Theme
	switch status(p) {
	case running:
		return render.Warm("Running for "+span(r.uptime(p)), t)
	case done:
		return render.Good("Done", t)
	case killed:
		return render.Dim("Killed", t)
	case lost:
		return render.Dim("Lost", t)
	}
	if p.GetCompletionReason() == "failed_start" {
		return render.Bad("Failed to start", t)
	}
	return render.Bad("Failed with exit code "+strconv.Itoa(int(p.GetExitCode())), t)
}

// Lines under the list about the process in hand, beyond what its row says: its whole command where the row cuts it, when it ended, what
// it printed last, then where it runs and its ids
func (r *roster) detail(i int) []string {
	if i < 0 || i >= len(r.rows) {
		return nil
	}
	t := r.kit.Env.Theme
	p := r.rows[i]
	var lines []string
	if r.list.Cut(i, 0) {
		lines = append(lines, oneLine(p.GetCommand()))
	}
	switch status(p) {
	case running:
	case lost:
		lines = append(lines, "Lost "+span(r.since(p))+" ago. icebath can no longer follow it.")
	default:
		lines = append(lines, "Ended "+span(r.since(p))+" ago.")
	}
	if last := lastLine(p.GetOutputTail()); last != "" {
		lines = append(lines, render.Dim("Last output: "+last, t))
	} else {
		lines = append(lines, render.Dim("No output yet.", t))
	}
	var facts []string
	if cwd := p.GetCwd(); cwd != "" {
		facts = append(facts, "In "+cwd)
	}
	facts = append(facts, "Id: "+p.GetId())
	if pid := p.GetPid(); pid > 0 {
		facts = append(facts, "PID "+strconv.Itoa(int(pid)))
	}
	return append(lines, render.Dim(strings.Join(facts, " · "), t))
}

// Starts reading the processes every few seconds while the list or an output page shows, unless that already runs
func (r *roster) poll() tea.Cmd {
	if r.polling {
		return nil
	}
	r.polling = true
	return r.tick()
}

// Reads the processes after a while, then again while the screen still shows. The wait alone runs off the app's loop: what the roster
// holds, such as the titles it read, is touched on the loop only
func (r *roster) tick() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		time.Sleep(refreshEvery)
		return func() tea.Cmd {
			if r.hub == nil || !r.hub.Shown() {
				r.polling = false
				return nil
			}
			return r.load(r.tick)
		}
	})
}

// Reads the processes and the titles of the sessions they name whose titles are not known yet, then runs then; the process in hand stays
// in hand
func (r *roster) load(then func() tea.Cmd) tea.Cmd {
	known := maps.Clone(r.titles)
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.processes.List(ctx, &terminalv1.ListRequest{})
		// Sessions not read yet, and those that had no title then, which may have one by now
		var unknown []string
		for _, p := range response.GetProcesses() {
			if id := p.GetSessionId(); id != "" && known[id] == "" && !slices.Contains(unknown, id) {
				unknown = append(unknown, id)
			}
		}
		titles := map[string]string{}
		if len(unknown) > 0 {
			// Each session the processes name, whatever state it is in, so an open one has its title too
			listed, failed := r.sessions.List(ctx, &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{Ids: unknown, IncludeArchived: true, IncludeHidden: true,
				IncludeChildren: true, IncludeEmpty: true, IncludeOpen: true, IncludePinned: true}})
			if failed == nil {
				for _, id := range unknown {
					titles[id] = ""
				}
				for _, session := range listed.GetSessions() {
					titles[session.GetId()] = session.GetTitle()
				}
			}
		}
		read := time.Now()
		return func() tea.Cmd {
			held := ""
			if i := r.list.Index(); i >= 0 && i < len(r.rows) && r.loaded {
				held = r.rows[i].GetId()
			}
			r.err, r.loaded = err, true
			if err == nil {
				maps.Copy(r.titles, titles)
				r.rows, r.read = r.order(response.GetProcesses()), read
			}
			r.list.Refresh()
			if held != "" {
				r.hold(held)
			}
			if then != nil {
				return then()
			}
			return nil
		}
	})
}

// Processes in the order a person looks for them: those of the session of the chat in hand first, then each other session's in the order
// of its first process; within a session, running ones first, the longest running first, then those that ended, the latest first
func (r *roster) order(processes []*terminalv1.Process) []*terminalv1.Process {
	rows := slices.Clone(processes)
	slices.SortStableFunc(rows, func(a, b *terminalv1.Process) int {
		ra, rb := status(a) == running, status(b) == running
		switch {
		case ra && !rb:
			return -1
		case rb && !ra:
			return 1
		case ra:
			return cmp.Compare(b.GetUptimeSeconds(), a.GetUptimeSeconds())
		}
		return cmp.Compare(a.GetSinceExitSeconds(), b.GetSinceExitSeconds())
	})
	rank := map[string]int{}
	if r.first != "" {
		rank[r.first] = -1
	}
	for _, p := range rows {
		if _, ok := rank[p.GetSessionId()]; !ok {
			rank[p.GetSessionId()] = len(rank)
		}
	}
	slices.SortStableFunc(rows, func(a, b *terminalv1.Process) int { return cmp.Compare(rank[a.GetSessionId()], rank[b.GetSessionId()]) })
	return rows
}

// Process of the id as last read
func (r *roster) find(id string) *terminalv1.Process {
	for _, p := range r.rows {
		if p.GetId() == id {
			return p
		}
	}
	return nil
}

// Page of the output of the process over the list, read again while it shows so the output of one that runs follows; x kills it from there
func (r *roster) page(id string) tea.Cmd {
	t := r.kit.Env.Theme
	page := cards.NewPage(r.kit.Env, cards.PageSpec{
		Title:   "Output of " + id,
		Key:     "processes:output:" + id,
		Follow:  true,
		Context: processesContext,
		Lines: func(w int) []string {
			p := r.find(id)
			if p == nil {
				return []string{render.Dim("This process is no longer listed.", t)}
			}
			width := max(w, 1)
			lines := strings.Split(render.Wrap(render.Dim(oneLine(p.GetCommand()), t), width), "\n")
			lines = append(lines, "")
			tail := strings.TrimRight(p.GetOutputTail(), "\n")
			if strings.TrimSpace(tail) == "" {
				return append(lines, render.Dim("No output yet.", t))
			}
			for _, line := range strings.Split(tail, "\n") {
				lines = append(lines, strings.Split(render.Wrap(line, width), "\n")...)
			}
			return lines
		},
		Hints: func(*cards.Page) []wm.Hint {
			if p := r.find(id); p != nil && status(p) == running {
				return []wm.Hint{{Action: "tui.processes.kill"}}
			}
			return nil
		},
		Actions: map[string]func(*cards.Page) tea.Cmd{
			"tui.processes.kill": func(page *cards.Page) tea.Cmd {
				if p := r.find(id); p != nil {
					return r.ask(p, page.Confirm)
				}
				return nil
			},
		},
	})
	return tea.Batch(r.hub.Push(page), r.poll())
}

// Says how something ended under the list while it shows, else in a toast
func (r *roster) tell(text string, level wm.Level) tea.Cmd {
	if r.hub != nil && r.hub.Shown() {
		switch level {
		case wm.Error, wm.Warn:
			return r.hub.Fail(text)
		case wm.Success:
			return r.hub.Done(text)
		}
		return r.hub.Say(text, cards.NoteInfo)
	}
	return wm.Notify(text, level)
}

// Asks before it kills the process, which cannot be undone; one that ended already is only said to have
func (r *roster) ask(p *terminalv1.Process, confirm func(cards.Question, func() tea.Cmd)) tea.Cmd {
	if status(p) != running {
		return r.tell(p.GetId()+" had already ended.", wm.Info)
	}
	id := p.GetId()
	confirm(cards.Question{Text: "Kill " + name(p) + "? It stops at once, and only what it printed so far is kept.", Verb: "Kill"},
		func() tea.Cmd { return r.kill(id) })
	return nil
}

// Asks before it kills every running process of the session, saying which session that is and how many
func (r *roster) askAll(session string, confirm func(cards.Question, func() tea.Cmd)) tea.Cmd {
	n := r.runningIn(session)
	if n == 0 {
		return nil
	}
	q := cards.Question{Text: "Kill the " + strconv.Itoa(n) + " running processes " + r.of(session) + "? They stop at once, and only what they " +
		"printed so far is kept.", Verb: "Kill all"}
	if n == 1 {
		q = cards.Question{Text: "Kill the running process " + r.of(session) + "? It stops at once, and only what it printed so far is kept.",
			Verb: "Kill"}
	}
	confirm(q, func() tea.Cmd { return r.killAll(session) })
	return nil
}

// Kills the process with the id, in whichever session it runs, then reads the processes again
func (r *roster) kill(id string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.processes.Kill(r.kit.Env.Ctx, &terminalv1.KillRequest{Id: id})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return r.tell("Could not kill "+id+". "+strs.Full(tui.Reason(err)), wm.Error)
			case response.GetStatus() == "killed":
				return tea.Batch(r.load(nil), r.tell("Killed "+id+".", wm.Success))
			case response.GetStatus() == "already_exited":
				return tea.Batch(r.load(nil), r.tell(id+" had already ended.", wm.Info))
			case response.GetStatus() == "not_found":
				return tea.Batch(r.load(nil), r.tell(absent(id), wm.Warn))
			}
			return r.tell("Could not kill "+id+". "+strs.Full(response.GetError()), wm.Error)
		}
	})
}

// Kills every running process of the session, then reads them again
func (r *roster) killAll(session string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.processes.Stop(r.kit.Env.Ctx, &terminalv1.StopRequest{SessionId: session})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return r.tell("Could not kill the background processes. "+strs.Full(tui.Reason(err)), wm.Error)
			case response.GetKilled() == 0:
				return tea.Batch(r.load(nil), r.tell("No background process was running there.", wm.Info))
			}
			return tea.Batch(r.load(nil), r.tell("Killed "+render.Count(int(response.GetKilled()), "background process")+".", wm.Success))
		}
	})
}
