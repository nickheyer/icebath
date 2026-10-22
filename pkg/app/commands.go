package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Models an endpoint listed, or the error that stopped the listing, quiet ones only filling the prompt suggestions
type modelsMsg struct {
	list  []profiles.Model
	err   error
	quiet bool
}

// Commands the palette offers beyond the window manager's own, each keyed by the keymap action that fires it
func (m *Model) commands() []wm.Command {
	busy := func() bool { d := m.current(); return d != nil && m.app.Busy(d.live) }
	idle := func() bool { d := m.current(); return d != nil && !m.app.Busy(d.live) }
	has := func() bool { return m.current() != nil }
	pending := func() bool { d := m.current(); return d != nil && len(d.approval.Pending()) > 0 }
	run := func(name, args string) func() tea.Cmd {
		return func() tea.Cmd { return m.command(m.current(), name, args) }
	}
	return []wm.Command{
		{ID: "workspace.new", Title: "Workspace: new session", Run: m.newWorkspace},
		{ID: "workspace.close", Title: "Workspace: close", When: func() bool { return len(m.desks) > 1 }, Run: m.closeWorkspace},
		{ID: "open", Title: "Open a panel…", Run: m.pickPanel},
		{ID: "keys.edit", Title: "Keybinds: edit", Run: m.editKeys},
		{ID: "session.open", Title: "Session: resume a stored one…", Run: m.pickSession},
		{ID: "session.fork", Title: "Session: fork into a new workspace", When: idle, Run: run("fork", "")},
		{ID: "session.fork.at", Title: "Session: fork at a turn…", When: idle, Run: m.pickTurn},
		{ID: "session.rename", Title: "Session: rename…", When: has, Run: m.promptRename},
		{ID: "session.compact", Title: "Session: compact now", When: idle, Run: run("compact", "")},
		{ID: "session.export", Title: "Session: export transcript…", When: has, Run: m.promptExport},
		{ID: "run.cancel", Title: "Run: cancel", When: busy, Run: run("cancel", "")},
		{ID: "run.interrupt", Title: "Run: stop after the current tool", When: busy, Run: run("interrupt", "")},
		{ID: "mission.start", Title: "Mission: start…", When: idle, Run: m.promptMission},
		{ID: "mission.open", Title: "Mission: open…", When: func() bool { d := m.current(); return d != nil && d.live.Missions.Len() > 0 }, Run: m.pickMission},
		{ID: "answer.allow", Title: "Approval: allow", When: pending, Run: m.answer(policy.Yes)},
		{ID: "answer.deny", Title: "Approval: deny", When: pending, Run: m.answer(policy.No)},
		{ID: "answer.always", Title: "Approval: always allow", When: pending, Run: m.answer(policy.Always)},
		{ID: "answer.never", Title: "Approval: never allow", When: pending, Run: m.answer(policy.Never)},
		{ID: "model.switch", Title: "Model: switch…", When: idle, Run: m.fetchModels},
		{ID: "profile.switch", Title: "Profile: switch…", When: idle, Run: m.pickProfile},
		{ID: "mode.set", Title: "Permission mode…", Run: m.pickMode},
		{ID: "theme.set", Title: "Theme…", Run: m.pickTheme},
		{ID: "motion.set", Title: "Motion…", Run: m.pickMotion},
		{ID: "cascade.toggle", Title: "Cascade: toggle the focused border comet", Run: func() tea.Cmd { return m.setKnob("cascade", strconv.FormatBool(!m.wm.Cascade()), false) }},
		{ID: "open.agents", Title: "Open: agents", Run: run("agents", "")},
		{ID: "open.metrics", Title: "Open: metrics", Run: run("metrics", "")},
		{ID: "open.files", Title: "Open: files touched", Run: run("files", "")},
		{ID: "open.jobs", Title: "Open: background jobs", Run: run("jobs", "")},
		{ID: "open.sessions", Title: "Open: stored sessions", Run: run("sessions", "")},
		{ID: "open.config", Title: "Open: configuration", Run: run("config", "")},
		{ID: "open.findings", Title: "Open: findings", When: has, Run: run("findings", "")},
		{ID: "agent.open", Title: "Agent: open transcript…", When: func() bool { d := m.current(); return d != nil && len(m.childRuns(d)) > 0 }, Run: m.pickAgent},
		{ID: "interrupt", Title: "Interrupt: cancel the run, or quit when nothing runs", Run: m.interrupt},
		{ID: "quit", Title: "Quit", Run: m.shutdown},
	}
}

// Opens the configuration on its keybind tab
func (m *Model) editKeys() tea.Cmd {
	return tea.Batch(m.wm.OpenHere(m.config), m.config.ShowKeys())
}

// Slash commands the composer lists as they are typed, with the values their arguments may take
func (m *Model) slashCommands() []cards.Command {
	models := func(string) []string {
		out := make([]string, 0, len(m.models))
		for _, md := range m.models {
			out = append(out, md.ID)
		}
		return out
	}
	modes := func(string) []string {
		var out []string
		for _, md := range mode.All() {
			out = append(out, string(md))
		}
		return out
	}
	profileNames := func(string) []string {
		var out []string
		for _, c := range m.profileChoices() {
			out = append(out, c.name)
		}
		return out
	}
	themes := func(string) []string { return theme.Names() }
	return []cards.Command{
		{Name: "model", Desc: "switch the model", Arg: "id", Suggest: models, Action: "model.switch"},
		{Name: "mode", Desc: "cycle or set the permission mode", Arg: "name", Suggest: modes, Action: "mode.cycle"},
		{Name: "profile", Desc: "switch the endpoint profile", Arg: "name", Suggest: profileNames, Action: "profile.switch"},
		{Name: "attach", Desc: "attach an image to the next message", Arg: "path"},
		{Name: "mission", Desc: "start a mission with an objective", Arg: "objective", Action: "mission.start"},
		{Name: "rename", Desc: "name the session", Arg: "title", Action: "session.rename"},
		{Name: "compact", Desc: "summarize the transcript now", Action: "session.compact"},
		{Name: "fork", Desc: "fork the session into a new workspace", Action: "session.fork"},
		{Name: "new", Desc: "open a fresh session in a new workspace", Action: "workspace.new"},
		{Name: "close", Desc: "close this workspace", Action: "workspace.close"},
		{Name: "sessions", Desc: "open the stored sessions", Action: "open.sessions"},
		{Name: "agents", Desc: "open the agent roster", Action: "open.agents"},
		{Name: "findings", Desc: "open the findings", Action: "open.findings"},
		{Name: "config", Desc: "open the configuration", Action: "open.config"},
		{Name: "keys", Desc: "edit the keybinds", Action: "keys.edit"},
		{Name: "metrics", Desc: "open the metrics", Action: "open.metrics"},
		{Name: "files", Desc: "open the files touched", Action: "open.files"},
		{Name: "jobs", Desc: "open the background jobs", Action: "open.jobs"},
		{Name: "theme", Desc: "switch the palette", Arg: "name", Suggest: themes, Action: "theme.set"},
		{Name: "cancel", Desc: "stop the run", Action: "run.cancel"},
		{Name: "interrupt", Desc: "stop after the current tool", Action: "run.interrupt"},
		{Name: "export", Desc: "write the transcript as markdown", Arg: "path", Action: "session.export"},
		{Name: "palette", Desc: "open the command palette", Action: "palette"},
		{Name: "quit", Desc: "leave icebath", Action: "quit"},
	}
}

// Carries out a slash command for a desk
func (m *Model) command(d *desk, name, args string) tea.Cmd {
	if d == nil {
		d = m.current()
	}
	open := func(c wm.Card) tea.Cmd { return m.wm.OpenHere(c) }
	switch name {
	case "palette":
		return m.wm.OpenPalette()
	case "model":
		if args == "" {
			return m.fetchModels()
		}
		return m.setModel(args)
	case "mode":
		if args == "" {
			return m.setKnob("mode", string(m.app.Mode.Mode().Next()), false)
		}
		return m.setKnob("mode", args, false)
	case "profile":
		if args == "" {
			return m.pickProfile()
		}
		prof, model, _ := strings.Cut(args, " ")
		return m.switchProfile(prof, strings.TrimSpace(model))
	case "mission":
		if strings.TrimSpace(args) == "" {
			return m.promptMission()
		}
		return m.startMission(d, args)
	case "rename":
		if d == nil {
			return nil
		}
		if strings.TrimSpace(args) == "" {
			return m.promptRename()
		}
		d.live.Session.Rename(strings.TrimSpace(args))
		m.wm.Repaint()
		return m.wm.Toast("renamed to "+strings.TrimSpace(args), wm.Success, 0)
	case "compact":
		return m.compactNow(d)
	case "fork":
		if d == nil {
			return nil
		}
		return m.forkAt(d, d.live, d.live.Session.Len())
	case "new":
		return m.newWorkspace()
	case "close":
		return m.closeWorkspace()
	case "sessions":
		return open(m.sessions)
	case "agents":
		return open(m.roster)
	case "findings":
		if d == nil {
			return nil
		}
		return open(m.findingsOf(d))
	case "config":
		return open(m.config)
	case "keys":
		return m.editKeys()
	case "metrics":
		return open(m.metrics)
	case "files":
		return open(m.files)
	case "jobs":
		return open(m.jobs)
	case "theme":
		if args == "" {
			return m.pickTheme()
		}
		return m.setKnob("theme", args, false)
	case "cancel":
		if d == nil {
			return nil
		}
		n := 0
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" && r.Session == d.live.Session && m.app.Control.Cancel(r.ID) == nil {
				n++
			}
		}
		if n == 0 {
			return m.wm.Toast("nothing is running", wm.Info, 0)
		}
		return m.wm.Toast("cancelling", wm.Warn, 0)
	case "interrupt":
		if d == nil {
			return nil
		}
		n := 0
		for _, r := range m.app.Control.Active() {
			if r.Parent == "" && r.Session == d.live.Session && m.app.Control.Interrupt(r.ID) == nil {
				n++
			}
		}
		if n == 0 {
			return m.wm.Toast("nothing is running", wm.Info, 0)
		}
		return m.wm.Toast("stopping after the current tool", wm.Warn, 0)
	case "export":
		return m.exportTranscript(d, args)
	case "attach":
		if d == nil {
			return nil
		}
		return d.session.Composer.Attach(args)
	case "quit":
		return m.shutdown()
	}
	return m.wm.Toast("unknown command /"+name, wm.Warn, 0)
}

// Shows a picker over the items
func (m *Model) pick(placeholder string, items []wm.Item) tea.Cmd {
	p := wm.NewPalette(m.theme, items)
	p.Placeholder = placeholder
	return m.wm.ShowOverlay(p)
}

// Shows a one line prompt prefilled with value
func (m *Model) prompt(placeholder, value string, submit func(string) tea.Cmd) tea.Cmd {
	return m.wm.ShowOverlay(wm.NewPrompt(m.theme, placeholder, value, submit))
}

func (m *Model) answer(v policy.Verdict) func() tea.Cmd {
	return func() tea.Cmd {
		d := m.current()
		if d == nil || len(d.approval.Pending()) == 0 {
			return nil
		}
		id := d.approval.Pending()[0].ID
		cmd, _ := m.cardMsg(cards.AnswerMsg{ID: id, Verdict: v})
		return cmd
	}
}

// Lists the models of the primary profile off the event loop, then opens the picker
func (m *Model) fetchModels() tea.Cmd {
	return tea.Batch(m.wm.Toast("listing models", wm.Info, 0), m.listModels(false))
}

// Lists the models of the primary profile off the event loop
func (m *Model) listModels(quiet bool) tea.Cmd {
	ctx := m.ctx
	p := m.app.Primary.Profile
	return func() tea.Msg {
		list, err := m.env.Models(ctx, p)
		return modelsMsg{list: list, err: err, quiet: quiet}
	}
}

func (m *Model) pickModel(msg modelsMsg) tea.Cmd {
	if msg.err != nil {
		return m.wm.Toast(msg.err.Error(), wm.Error, 0)
	}
	items := make([]wm.Item, 0, len(msg.list))
	for _, md := range msg.list {
		id := md.ID
		detail := md.Name
		if md.Context > 0 {
			detail = strings.TrimSpace(detail + " " + render.Tokens(md.Context) + " ctx")
		}
		keys := ""
		if id == m.app.Primary.Model {
			keys = "current"
		}
		items = append(items, wm.Item{ID: id, Title: id, Detail: detail, Keys: keys, Run: func() tea.Cmd { return m.setModel(id) }})
	}
	if len(items) == 0 {
		return m.wm.Toast("the endpoint lists no models", wm.Warn, 0)
	}
	return m.pick("switch model", items)
}

// Profile a picker offers with what it is
type profileChoice struct {
	name   string
	detail string
}

// Written profiles that are on, then the built in providers no profile takes the place of
func (m *Model) profileChoices() []profileChoice {
	var out []profileChoice
	for _, in := range m.app.Profiles {
		if in.Profile.Disabled {
			continue
		}
		provider := in.Written.Template
		if provider == "" {
			provider = "custom"
		}
		out = append(out, profileChoice{in.Profile.Name, "profile from " + provider + ", " + in.Profile.Model()})
	}
	for _, p := range profiles.Defaults() {
		if !m.app.Shadowed(p.Name) {
			out = append(out, profileChoice{p.Name, "provider as is, key from " + p.Key.Describe()})
		}
	}
	return out
}

func (m *Model) pickProfile() tea.Cmd {
	var items []wm.Item
	for _, c := range m.profileChoices() {
		name := c.name
		keys := ""
		if name == m.app.Primary.Profile.Name {
			keys = "current"
		}
		items = append(items, wm.Item{ID: name, Title: name, Detail: c.detail, Keys: keys, Run: func() tea.Cmd { return m.switchProfile(name, "") }})
	}
	return m.pick("switch profile", items)
}

func (m *Model) pickMode() tea.Cmd {
	var items []wm.Item
	for _, md := range mode.All() {
		md := md
		keys := ""
		if md == m.app.Mode.Mode() {
			keys = "current"
		}
		items = append(items, wm.Item{ID: string(md), Title: md.Label(), Detail: string(md), Keys: keys, Run: func() tea.Cmd {
			cmd, _ := m.cardMsg(cards.SetModeMsg{Mode: md})
			return cmd
		}})
	}
	return m.pick("permission mode", items)
}

func (m *Model) pickTheme() tea.Cmd {
	var items []wm.Item
	for _, name := range theme.Names() {
		name := name
		keys := ""
		if name == m.theme.Name {
			keys = "current"
		}
		items = append(items, wm.Item{ID: name, Title: name, Keys: keys, Run: func() tea.Cmd { return m.setKnob("theme", name, false) }})
	}
	return m.pick("theme", items)
}

func (m *Model) pickMotion() tea.Cmd {
	levels := []struct {
		label string
		value string
	}{{"full springs", "1"}, {"half speed", "0.5"}, {"snap, no animation", "0"}}
	var items []wm.Item
	for _, l := range levels {
		value := l.value
		items = append(items, wm.Item{ID: value, Title: l.label, Detail: value, Run: func() tea.Cmd { return m.setKnob("motion", value, false) }})
	}
	return m.pick("motion", items)
}

func (m *Model) pickSession() tea.Cmd {
	m.sessions.Reload()
	m.wm.Repaint()
	open := map[string]bool{}
	for _, l := range m.app.Lives() {
		open[l.Session.ID()] = true
	}
	var items []wm.Item
	for _, info := range m.sessions.Rows() {
		id := info.ID
		title := info.Title
		if title == "" {
			title = id
		}
		keys := info.Updated.Local().Format("01-02 15:04")
		if open[id] {
			keys = "open"
		}
		items = append(items, wm.Item{ID: id, Title: title, Detail: fmt.Sprintf("%s · %d msgs", info.Model, info.Messages), Keys: keys, Run: func() tea.Cmd { return m.openStored(id) }})
	}
	if len(items) == 0 {
		return m.wm.Toast("nothing stored yet", wm.Info, 0)
	}
	return m.pick("open session", items)
}

func (m *Model) pickTurn() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	entries := d.live.Session.Entries()
	var items []wm.Item
	turn := 0
	for i, e := range entries {
		if e.Role != protocol.RoleUser {
			continue
		}
		turn++
		n := i
		text := render.Summary(protocol.Response{Content: e.Content}.Text(), 60)
		items = append(items, wm.Item{ID: fmt.Sprint(n), Title: fmt.Sprintf("before turn %d", turn), Detail: text, Keys: e.Time.Local().Format("15:04"), Run: func() tea.Cmd { return m.forkAt(d, d.live, n) }})
	}
	all := len(entries)
	items = append(items, wm.Item{ID: "end", Title: "at the end", Detail: fmt.Sprintf("%d entries", all), Run: func() tea.Cmd { return m.forkAt(d, d.live, all) }})
	return m.pick("fork at", items)
}

func (m *Model) pickMission() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	var items []wm.Item
	for _, ms := range d.live.Missions.List() {
		id := ms.ID
		items = append(items, wm.Item{ID: id, Title: render.Summary(ms.Objective, 50), Detail: ms.Status, Keys: render.Percent(ms.Progress()), Run: func() tea.Cmd {
			card, ok := m.missionOf(d, id)
			if !ok {
				return nil
			}
			return m.wm.Open(d.ws, card)
		}})
	}
	return m.pick("open mission", items)
}

// Child runs of the session of a desk, newest first
func (m *Model) childRuns(d *desk) []*harness.Run {
	var out []*harness.Run
	for _, r := range m.app.Control.Runs() {
		if r.Parent != "" && m.app.LiveOfRun(r.ID) == d.live {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

func (m *Model) pickAgent() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	var items []wm.Item
	for _, r := range m.childRuns(d) {
		id := r.ID
		state := "done"
		switch {
		case r.Running():
			state = "running"
		case r.Err() != nil:
			state = "failed"
		}
		items = append(items, wm.Item{ID: id, Title: r.Name, Detail: render.Truncate(r.Description, 50), Keys: state, Run: func() tea.Cmd { return m.openAgent(id) }})
	}
	return m.pick("open agent", items)
}

func (m *Model) promptMission() tea.Cmd {
	return m.prompt("mission objective", "", func(text string) tea.Cmd { return m.startMission(m.current(), text) })
}

// Asks for the session label, an empty answer handing the label back to the first prompt
func (m *Model) promptRename() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	return m.prompt("session label, empty to derive it from the first prompt", d.live.Session.Title(), func(text string) tea.Cmd {
		if strings.TrimSpace(text) != "" {
			return m.command(d, "rename", text)
		}
		d.live.Session.Rename("")
		d.live.Session.SetTitle(firstPrompt(d.live))
		m.wm.Repaint()
		return m.wm.Toast("label derived from the first prompt", wm.Success, 0)
	})
}

// Label the first prompt of a session derives, empty before any
func firstPrompt(l *harness.Live) string {
	for _, e := range l.Session.Entries() {
		text := protocol.Response{Content: e.Content}.Text()
		if e.Role == protocol.RoleUser && strings.TrimSpace(text) != "" {
			return render.Summary(text, 60)
		}
	}
	return ""
}

func (m *Model) promptExport() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	return m.prompt("export path", m.exportPath(d), func(text string) tea.Cmd { return m.exportTranscript(d, strings.TrimSpace(text)) })
}

// Panels a workspace can open, by slash command name, each opened by the keymap action open.<name>
var panels = []struct{ name, title, detail string }{
	{"agents", "Agents", "every run across the open sessions"},
	{"metrics", "Metrics", "tokens, cost, latency and the context gauge"},
	{"files", "Files", "files the agents read and changed"},
	{"jobs", "Jobs", "background shell jobs and their output"},
	{"findings", "Findings", "facts the agent recorded"},
	{"sessions", "Sessions", "stored sessions to resume, fork or delete"},
	{"config", "Config", "profiles, models, knobs, rules, hooks, servers and keybinds"},
}

func (m *Model) pickPanel() tea.Cmd {
	d := m.current()
	items := make([]wm.Item, 0, len(panels))
	for _, p := range panels {
		name := p.name
		var marks []string
		if k := m.keys.Display("open." + name); k != "" {
			marks = append(marks, k)
		}
		open := m.panel(name) != nil && m.wm.Has(name)
		if name == "findings" && d != nil {
			open = m.wm.Has("findings:" + d.live.Session.ID())
		}
		if open {
			marks = append(marks, "open")
		}
		items = append(items, wm.Item{ID: name, Title: p.title, Detail: p.detail, Keys: strings.Join(marks, " · "), Run: func() tea.Cmd { return m.command(d, name, "") }})
	}
	return m.pick("open a panel", items)
}
