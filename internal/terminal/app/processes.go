package app

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
	terminalv1 "github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/terminal/v1/terminalv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of every list of background processes, of one session or of all of them
const ProcessesContext = "tui.processes"

// What the context of the lists is called where keys are listed
const ProcessesWhere = "background processes"

// Keys of the lists of background processes, registered by each screen that shows one
var ProcessActions = []keys.Action{
	{ID: "tui.processes.read", Context: ProcessesContext, Label: "read", Doc: "Read the output of the background process", Default: []string{"enter"}},
	{ID: "tui.processes.kill", Context: ProcessesContext, Label: "kill", Doc: "Kill the background process", Default: []string{"x"}},
	{ID: "tui.processes.kill.all", Context: ProcessesContext, Label: "kill all", Doc: "Kill every background process of the list that still runs",
		Default: []string{"X"}},
}

// How often an open list or output page reads the processes again, so times and output stay current
const refreshEvery = 2 * time.Second

// States of a process, as Status says them
const (
	Running = "running"
	Done    = "done"
	Failed  = "failed"
	Killed  = "killed"
	Lost    = "lost"
)

// How a process stands: running until it exited, then by how it ended or its exit code
func Status(p *terminalv1.Process) string {
	if p.GetStatus() != "exited" {
		return Running
	}
	switch p.GetCompletionReason() {
	case "failed_start":
		return Failed
	case "killed", "lost":
		return p.GetCompletionReason()
	}
	if p.GetHasExitCode() && p.GetExitCode() != 0 {
		return Failed
	}
	return Done
}

// Length of time in words, rounded down to its largest whole unit: 45 seconds, 3 minutes, 1 hour, 2 days
func Span(d time.Duration) string {
	for _, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if d >= unit {
			return config.Span(d.Truncate(unit))
		}
	}
	return config.Span(max(d, 0).Truncate(time.Second))
}

// What became of a process that ended, as the start of a sentence: "Ended 20 seconds ago with exit code 1", "Killed 3 minutes ago"
func Ended(p *terminalv1.Process, since time.Duration) string {
	ago := Span(since) + " ago"
	switch Status(p) {
	case Killed:
		return "Killed " + ago
	case Lost:
		return "Lost " + ago + ". icebath can no longer follow it"
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
func OneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// Last line of the output that is not blank
func LastLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := OneLine(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// Sentence of a reason a server gave: capital first letter, ending in a period
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ToUpper(text[:1]) + text[1:]
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}

// Background processes in a list, of one session or of every session, read again while the list or an output page shows. Enter reads
// the output of the highlighted one, x kills it after a yes, X kills every one still running
type Roster struct {
	kit       *tui.Kit
	processes terminalv1connect.ProcessServiceClient
	sessions  sessionsv1connect.SessionServiceClient
	// Session whose processes are listed, every session's when empty
	session string
	hub     *cards.Hub
	list    *cards.List
	// Processes in the order shown: by session when every session's are listed, running ones first within each
	rows []*terminalv1.Process
	// Titles of the sessions by id, read once and again when a process names a session not among them
	titles map[string]string
	// When the processes were last read, so times count on between two reads
	read    time.Time
	loaded  bool
	err     error
	polling bool
}

// Roster of the processes of the session, of every session when it is empty
func NewRoster(kit *tui.Kit, processes terminalv1connect.ProcessServiceClient, sessions sessionsv1connect.SessionServiceClient, session string) *Roster {
	return &Roster{kit: kit, processes: processes, sessions: sessions, session: session, titles: map[string]string{}}
}

// Opens the list over the screen
func (r *Roster) Open() tea.Cmd {
	t := r.kit.Env.Theme
	title := "Background processes"
	if r.session != "" {
		title = "Session processes"
	}
	r.list = cards.NewList(r.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Jobs,
		Key:     "processes:" + r.session,
		Context: ProcessesContext,
		Primary: "tui.processes.read",
		Columns: []render.Column{{Flex: 1}, {}},
		Rows: func() [][]string {
			rows := make([][]string, len(r.rows))
			for i, p := range r.rows {
				rows[i] = []string{cmp.Or(OneLine(p.GetCommand()), "Background process"), r.state(p)}
			}
			return rows
		},
		Group: func(i int) string {
			if r.session != "" {
				return ""
			}
			return r.sessionTitle(r.rows[i].GetSessionId())
		},
		Detail: func(i int) []string { return r.detail(r.rows[i]) },
		Empty: func() string {
			none := "No background processes. Commands the agent leaves running show here."
			if r.session != "" {
				none = "This session has no background processes. Commands the agent leaves running show here."
			}
			return r.waiting(none)
		},
		Free:  map[string]bool{"tui.processes.kill.all": true, "hub.refresh": true},
		Hints: r.hints,
		OnFocus: func(*cards.List) tea.Cmd {
			return tea.Batch(r.load(), r.poll())
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"tui.processes.read": func(_ *cards.List, i int) tea.Cmd { return r.page(r.rows[i].GetId()) },
			"tui.processes.kill": func(l *cards.List, i int) tea.Cmd {
				p := r.rows[i]
				if Status(p) != Running {
					return nil
				}
				l.Confirm("Kill "+p.GetId()+"? Its command stops at once.", func() tea.Cmd { return r.Kill(p.GetId()) })
				return nil
			},
			"tui.processes.kill.all": func(l *cards.List, _ int) tea.Cmd {
				running := r.running()
				if running == 0 {
					return nil
				}
				question := "Kill all " + render.Count(running, "running background process") + "?"
				if running == 1 {
					question = "Kill the 1 running background process?"
				}
				l.Confirm(question, r.killAll)
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return r.load() },
		},
	})
	r.list.ToggleDetail()
	r.hub = r.kit.Hub(r.list.Key(), r.list)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.load())
}

// Buttons for the process under the cursor: reading its output, killing it while it runs, and killing every one that runs
func (r *Roster) hints(_ *cards.List, i int) []wm.Hint {
	var out []wm.Hint
	if i >= 0 && i < len(r.rows) {
		out = append(out, wm.Hint{Action: "tui.processes.read"})
		if Status(r.rows[i]) == Running {
			out = append(out, wm.Hint{Action: "tui.processes.kill"})
		}
	}
	if r.running() > 0 {
		out = append(out, wm.Hint{Action: "tui.processes.kill.all"})
	}
	return out
}

// How many of the processes listed still run
func (r *Roster) running() int {
	n := 0
	for _, p := range r.rows {
		if Status(p) == Running {
			n++
		}
	}
	return n
}

// What the list says while it has no rows: what it waits for, what failed and how to try again, or that there are none
func (r *Roster) waiting(none string) string {
	switch {
	case r.err != nil:
		return "Could not read the background processes. " + sentence(tui.Reason(r.err)) + " " + r.kit.Env.Keys.Display("hub.refresh") + " tries again."
	case !r.loaded:
		return "Reading the background processes…"
	}
	return none
}

// Title of a session, or what stands for one that has none
func (r *Roster) sessionTitle(id string) string {
	switch {
	case id == "":
		return "Started outside a session"
	case r.titles[id] != "":
		return r.titles[id]
	}
	return "Untitled session"
}

// Time passed since the processes were read, so the times they report count on between two reads
func (r *Roster) passed() time.Duration {
	if r.read.IsZero() {
		return 0
	}
	return max(time.Since(r.read), 0)
}

// How long the process ran, counted on to now while it runs
func (r *Roster) uptime(p *terminalv1.Process) time.Duration {
	up := time.Duration(p.GetUptimeSeconds()) * time.Second
	if Status(p) == Running {
		up += r.passed()
	}
	return up
}

// Time since the process ended, counted on to now
func (r *Roster) since(p *terminalv1.Process) time.Duration {
	return time.Duration(p.GetSinceExitSeconds())*time.Second + r.passed()
}

// State of the process in a few words, colored by how it stands
func (r *Roster) state(p *terminalv1.Process) string {
	t := r.kit.Env.Theme
	switch Status(p) {
	case Running:
		return render.Warm("Running for "+Span(r.uptime(p)), t)
	case Done:
		return render.Good("Done", t)
	case Killed:
		return render.Dim("Killed", t)
	case Lost:
		return render.Dim("Lost", t)
	}
	if p.GetCompletionReason() == "failed_start" {
		return render.Bad("Failed to start", t)
	}
	return render.Bad("Failed with exit code "+strconv.Itoa(int(p.GetExitCode())), t)
}

// Lines under the list about the process: its whole command, how it stands and what it printed last, then where it runs and its ids
func (r *Roster) detail(p *terminalv1.Process) []string {
	t := r.kit.Env.Theme
	lines := []string{OneLine(p.GetCommand())}
	if Status(p) == Running {
		lines = append(lines, "Running for "+Span(r.uptime(p))+".")
	} else {
		lines = append(lines, Ended(p, r.since(p))+".")
	}
	if last := LastLine(p.GetOutputTail()); last != "" {
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
func (r *Roster) poll() tea.Cmd {
	if r.polling {
		return nil
	}
	r.polling = true
	return r.tick()
}

// Reads the processes after a while, then again while the screen still shows
func (r *Roster) tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg {
		return r.fetch(func() tea.Cmd {
			if r.hub == nil || !r.hub.Shown() {
				r.polling = false
				return nil
			}
			return r.tick()
		})()
	})
}

func (r *Roster) load() tea.Cmd { return r.fetch(nil) }

// Reads the processes and, when one names a session whose title is not known yet, the titles of the sessions, then runs then
func (r *Roster) fetch(then func() tea.Cmd) tea.Cmd {
	known := maps.Clone(r.titles)
	return tui.Async(func() func() tea.Cmd {
		ctx := r.kit.Env.Ctx
		response, err := r.processes.List(ctx, &terminalv1.ListRequest{SessionId: r.session})
		var titles map[string]string
		if err == nil && r.session == "" && slices.ContainsFunc(response.GetProcesses(), func(p *terminalv1.Process) bool {
			_, ok := known[p.GetSessionId()]
			return p.GetSessionId() != "" && !ok
		}) {
			if listed, err := r.sessions.List(ctx, &sessionsv1.ListRequest{}); err == nil {
				titles = map[string]string{}
				for _, session := range listed.GetSessions() {
					titles[session.GetId()] = session.GetTitle()
				}
				for _, p := range response.GetProcesses() {
					if _, ok := titles[p.GetSessionId()]; !ok {
						titles[p.GetSessionId()] = ""
					}
				}
			}
		}
		read := time.Now()
		return func() tea.Cmd {
			r.err, r.loaded = err, true
			if err == nil {
				if titles != nil {
					r.titles = titles
				}
				r.rows, r.read = r.order(response.GetProcesses()), read
			}
			if r.list != nil {
				r.list.Refresh()
			}
			if then != nil {
				return then()
			}
			return nil
		}
	})
}

// Processes in the order a person looks for them: running ones first, the longest running first, then those that ended, the latest first;
// every session's grouped by session, the sessions in the order of their first process
func (r *Roster) order(processes []*terminalv1.Process) []*terminalv1.Process {
	rows := slices.Clone(processes)
	slices.SortStableFunc(rows, func(a, b *terminalv1.Process) int {
		ra, rb := Status(a) == Running, Status(b) == Running
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
	if r.session != "" {
		return rows
	}
	rank := map[string]int{}
	for _, p := range rows {
		if _, ok := rank[p.GetSessionId()]; !ok {
			rank[p.GetSessionId()] = len(rank)
		}
	}
	slices.SortStableFunc(rows, func(a, b *terminalv1.Process) int { return cmp.Compare(rank[a.GetSessionId()], rank[b.GetSessionId()]) })
	return rows
}

// Process of the id as last read, or the first whose id starts with it
func (r *Roster) find(id string) *terminalv1.Process {
	for _, p := range r.rows {
		if p.GetId() == id {
			return p
		}
	}
	for _, p := range r.rows {
		if strings.HasPrefix(p.GetId(), id) || strings.HasPrefix(p.GetId(), "proc_"+id) {
			return p
		}
	}
	return nil
}

// Page of the output of the process, over the list when it is open, read again while it shows so the output of one that runs follows
func (r *Roster) page(id string) tea.Cmd {
	t := r.kit.Env.Theme
	page := cards.NewPage(r.kit.Env, cards.PageSpec{
		Title:  "Output of " + id,
		Key:    "processes:output:" + id,
		Follow: true,
		Lines: func(w int) []string {
			p := r.find(id)
			if p == nil {
				return []string{render.Dim("This process is no longer listed.", t)}
			}
			width := max(w, 1)
			lines := strings.Split(render.Wrap(render.Dim(OneLine(p.GetCommand()), t), width), "\n")
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
	})
	if r.hub != nil && r.hub.Shown() {
		return r.hub.Push(page)
	}
	r.hub = r.kit.Hub(page.Key(), page)
	return tea.Batch(r.kit.WM.ShowOverlay(r.hub), r.poll())
}

// Shows the output of the process with the id, or the start of one, read anew
func (r *Roster) Read(id string) tea.Cmd {
	return r.fetch(func() tea.Cmd {
		if r.err != nil {
			return wm.Notify("Could not read the background processes. "+sentence(tui.Reason(r.err)), wm.Error)
		}
		p := r.find(id)
		if p == nil {
			return wm.Notify(r.absent(id), wm.Warn)
		}
		return r.page(p.GetId())
	})
}

// Sentence saying no process has the id
func (r *Roster) absent(id string) string {
	if r.session != "" {
		return "This session has no background process " + id + "."
	}
	return "There is no background process " + id + "."
}

// Says how something ended under the list while it shows, else in a toast
func (r *Roster) tell(text string, level wm.Level) tea.Cmd {
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

// Kills the process with the id, or the start of one, then reads the processes again
func (r *Roster) Kill(id string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.processes.Kill(r.kit.Env.Ctx, &terminalv1.KillRequest{Id: id, SessionId: r.session})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return r.tell("Could not kill "+id+". "+sentence(tui.Reason(err)), wm.Error)
			case response.GetStatus() == "killed":
				return tea.Batch(r.load(), r.tell("Killed "+id+".", wm.Success))
			case response.GetStatus() == "already_exited":
				return tea.Batch(r.load(), r.tell("The process "+id+" had already ended.", wm.Info))
			case response.GetStatus() == "not_found":
				return tea.Batch(r.load(), r.tell(r.absent(id), wm.Warn))
			}
			return r.tell("Could not kill "+id+". "+sentence(response.GetError()), wm.Error)
		}
	})
}

// Kills every process of the list that still runs, then reads them again
func (r *Roster) killAll() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := r.processes.Stop(r.kit.Env.Ctx, &terminalv1.StopRequest{SessionId: r.session})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return r.tell("Could not kill the background processes. "+sentence(tui.Reason(err)), wm.Error)
			case response.GetKilled() == 0:
				return tea.Batch(r.load(), r.tell("No background process was running.", wm.Info))
			}
			return tea.Batch(r.load(), r.tell("Killed "+render.Count(int(response.GetKilled()), "background process")+".", wm.Success))
		}
	})
}
