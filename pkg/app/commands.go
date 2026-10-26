package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
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
	more := m.theme.Glyphs.Ellipsis
	return []wm.Command{
		{ID: "session.new", Title: "Session: new", Run: m.newSession},
		{ID: "session.close", Title: "Session: close", When: idle, Run: func() tea.Cmd { return m.closeSession(m.current()) }},
		{ID: "workspace.new", Title: "Workspace: new", Run: m.newWorkspace},
		{ID: "workspace.close", Title: "Workspace: close", When: func() bool { return len(m.wm.Workspaces()) > 1 }, Run: m.closeWorkspace},
		{ID: "open", Title: "Open panel" + more, Run: m.pickPanel},
		{ID: "keys.edit", Title: "Keybinds", Run: m.editKeys},
		{ID: "session.open", Title: "Session: resume" + more, Run: m.pickSession},
		{ID: "session.fork", Title: "Session: fork", When: idle, Run: run("fork", "")},
		{ID: "session.fork.at", Title: "Session: fork at turn" + more, When: idle, Run: m.pickTurn},
		{ID: "session.rename", Title: "Session: rename" + more, When: has, Run: m.promptRename},
		{ID: "session.compact", Title: "Session: compact", When: idle, Run: run("compact", "")},
		{ID: "session.clear", Title: "Session: clear", When: idle, Run: run("clear", "")},
		{ID: "session.export", Title: "Session: export" + more, When: has, Run: m.promptExport},
		{ID: "run.cancel", Title: "Run: cancel", When: busy, Run: run("cancel", "")},
		{ID: "run.interrupt", Title: "Run: stop after tool", When: busy, Run: run("interrupt", "")},
		{ID: "mission.start", Title: "Mission: start" + more, When: idle, Run: m.promptMission},
		{ID: "mission.open", Title: "Mission: open" + more, When: func() bool { d := m.current(); return d != nil && d.live.Missions.Len() > 0 }, Run: m.pickMission},
		{ID: "attention.next", Title: "Attention: next", When: func() bool { return len(m.queue(m.deskStates())) > 0 }, Run: m.dispatch},
		{ID: "answer.allow", Title: "Approval: allow", When: pending, Run: m.answer(policy.Yes)},
		{ID: "answer.deny", Title: "Approval: deny", When: pending, Run: m.answer(policy.No)},
		{ID: "answer.always", Title: "Approval: always", When: pending, Run: m.answer(policy.Always)},
		{ID: "answer.never", Title: "Approval: never", When: pending, Run: m.answer(policy.Never)},
		{ID: "model.switch", Title: "Model" + more, When: idle, Run: m.fetchModels},
		{ID: "profile.switch", Title: "Profile" + more, When: idle, Run: m.pickProfile},
		{ID: "mode.set", Title: "Mode" + more, Run: m.pickMode},
		{ID: "theme.set", Title: "Theme" + more, Run: m.pickTheme},
		{ID: "motion.set", Title: "Motion" + more, Run: m.pickMotion},
		{ID: "cascade.toggle", Title: "Cascade: toggle", Run: func() tea.Cmd { return m.setSetting("cascade", strconv.FormatBool(!m.wm.Cascade()), false) }},
		{ID: "open.agents", Title: "Open: agents", Run: run("agents", "")},
		{ID: "open.metrics", Title: "Open: metrics", Run: run("metrics", "")},
		{ID: "open.jobs", Title: "Open: jobs", Run: run("jobs", "")},
		{ID: "open.sessions", Title: "Open: sessions", Run: run("sessions", "")},
		{ID: "open.config", Title: "Open: config", Run: run("config", "")},
		{ID: "open.findings", Title: "Open: findings", When: has, Run: run("findings", "")},
		{ID: "agent.open", Title: "Agent: open" + more, When: func() bool { d := m.current(); return d != nil && len(m.childRuns(d)) > 0 }, Run: m.pickAgent},
		{ID: "interrupt", Title: "Interrupt", Run: m.interrupt},
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
		for _, md := range policy.Modes() {
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
		{Name: "model", Desc: "switch model", Arg: "id", Suggest: models, Action: "model.switch"},
		{Name: "mode", Desc: "permission mode", Arg: "name", Suggest: modes, Action: "mode.cycle"},
		{Name: "profile", Desc: "switch profile", Arg: "name", Suggest: profileNames, Action: "profile.switch"},
		{Name: "attach", Desc: "attach an image", Arg: "path"},
		{Name: "mission", Desc: "start a mission", Arg: "objective", Action: "mission.start"},
		{Name: "rename", Desc: "rename the session", Arg: "title", Action: "session.rename"},
		{Name: "compact", Desc: "compact the transcript", Action: "session.compact"},
		{Name: "clear", Desc: "fresh session here", Action: "session.clear"},
		{Name: "fork", Desc: "fork the session beside it", Action: "session.fork"},
		{Name: "new", Desc: "new session here", Action: "session.new"},
		{Name: "close", Desc: "close this session", Action: "session.close"},
		{Name: "workspace", Desc: "new workspace", Action: "workspace.new"},
		{Name: "sessions", Desc: "stored sessions", Action: "open.sessions"},
		{Name: "agents", Desc: "agents", Action: "open.agents"},
		{Name: "findings", Desc: "findings", Action: "open.findings"},
		{Name: "config", Desc: "config", Action: "open.config"},
		{Name: "keys", Desc: "keybinds", Action: "keys.edit"},
		{Name: "metrics", Desc: "metrics", Action: "open.metrics"},
		{Name: "jobs", Desc: "shell jobs", Action: "open.jobs"},
		{Name: "theme", Desc: "theme", Arg: "name", Suggest: themes, Action: "theme.set"},
		{Name: "cancel", Desc: "cancel the agent", Action: "run.cancel"},
		{Name: "interrupt", Desc: "stop after this tool", Action: "run.interrupt"},
		{Name: "export", Desc: "export markdown", Arg: "path", Action: "session.export"},
		{Name: "palette", Desc: "command palette", Action: "palette"},
		{Name: "quit", Desc: "quit", Action: "quit"},
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
			return m.setSetting("mode", string(m.app.Mode.Mode().Next()), false)
		}
		return m.setSetting("mode", args, false)
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
	case "clear":
		return m.clearDesk(d)
	case "fork":
		if d == nil {
			return nil
		}
		return m.forkAt(d, d.live, d.live.Session.Len())
	case "new":
		return m.newSession()
	case "close":
		return m.closeSession(d)
	case "workspace":
		return m.newWorkspace()
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
	case "jobs":
		return open(m.jobs)
	case "theme":
		if args == "" {
			return m.pickTheme()
		}
		return m.setSetting("theme", args, false)
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
			return m.wm.Toast("nothing running", wm.Info, 0)
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
			return m.wm.Toast("nothing running", wm.Info, 0)
		}
		return m.wm.Toast("stopping after this tool", wm.Warn, 0)
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

// Lists the models of the profile on the stage off the event loop
func (m *Model) listModels(quiet bool) tea.Cmd {
	ctx := m.ctx
	p := m.endpoint().Profile
	m.listed = p.Name
	return func() tea.Msg {
		list, err := m.env.Models(ctx, p)
		return modelsMsg{list: list, err: err, quiet: quiet}
	}
}

// Mark a picker row carries when it is the value in force
func (m *Model) mark(on bool) string {
	if on {
		return m.theme.Glyphs.Live
	}
	return ""
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
		items = append(items, wm.Item{ID: id, Title: id, Detail: detail, Keys: m.mark(id == m.endpoint().Model), Run: func() tea.Cmd { return m.setModel(id) }})
	}
	if len(items) == 0 {
		return m.wm.Toast("no models", wm.Warn, 0)
	}
	return m.pick("model", items)
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
		items = append(items, wm.Item{ID: name, Title: name, Detail: c.detail, Keys: m.mark(name == m.endpoint().Profile.Name), Run: func() tea.Cmd { return m.switchProfile(name, "") }})
	}
	return m.pick("profile", items)
}

func (m *Model) pickMode() tea.Cmd {
	var items []wm.Item
	for _, md := range policy.Modes() {
		md := md
		items = append(items, wm.Item{ID: string(md), Title: md.Label(), Keys: m.mark(md == m.app.Mode.Mode()), Run: func() tea.Cmd {
			cmd, _ := m.cardMsg(cards.SetModeMsg{Mode: md})
			return cmd
		}})
	}
	return m.pick("mode", items)
}

func (m *Model) pickTheme() tea.Cmd {
	var items []wm.Item
	for _, name := range theme.Names() {
		name := name
		items = append(items, wm.Item{ID: name, Title: name, Keys: m.mark(name == m.theme.Name), Run: func() tea.Cmd { return m.setSetting("theme", name, false) }})
	}
	return m.pick("theme", items)
}

func (m *Model) pickMotion() tea.Cmd {
	levels := []struct {
		label string
		value string
	}{{"full", "1"}, {"half", "0.5"}, {"off", "0"}}
	var items []wm.Item
	for _, l := range levels {
		value := l.value
		items = append(items, wm.Item{ID: value, Title: l.label, Detail: value, Keys: m.mark(fmt.Sprintf("%.2g", m.wm.Motion()) == value), Run: func() tea.Cmd { return m.setSetting("motion", value, false) }})
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
		items = append(items, wm.Item{ID: id, Title: title, Detail: fmt.Sprintf("%s %s %d msgs", info.Model, m.theme.Glyphs.Sep, info.Messages), Keys: keys, Run: func() tea.Cmd { return m.openStored(id) }})
	}
	if len(items) == 0 {
		return m.wm.Toast("no stored sessions", wm.Info, 0)
	}
	return m.pick("session", items)
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
		text := render.Summary(protocol.Response{Content: e.Content}.Text(), 60, m.theme)
		items = append(items, wm.Item{ID: fmt.Sprint(n), Title: fmt.Sprintf("turn %d", turn), Detail: text, Keys: e.Time.Local().Format("15:04"), Run: func() tea.Cmd { return m.forkAt(d, d.live, n) }})
	}
	all := len(entries)
	items = append(items, wm.Item{ID: "end", Title: "end", Detail: fmt.Sprintf("%d entries", all), Run: func() tea.Cmd { return m.forkAt(d, d.live, all) }})
	return m.pick("fork before", items)
}

func (m *Model) pickMission() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	var items []wm.Item
	for _, ms := range d.live.Missions.List() {
		id := ms.ID
		items = append(items, wm.Item{ID: id, Title: render.Summary(ms.Objective, 50, m.theme), Detail: ms.Status, Keys: render.Percent(ms.Progress()), Run: func() tea.Cmd {
			card, ok := m.missionOf(d, id)
			if !ok {
				return nil
			}
			return m.openFrom(d, card)
		}})
	}
	return m.pick("mission", items)
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
		items = append(items, wm.Item{ID: id, Title: r.Name, Detail: render.Truncate(r.Description, 50, m.theme), Keys: state, Run: func() tea.Cmd { return m.openAgent(id) }})
	}
	return m.pick("agent", items)
}

func (m *Model) promptMission() tea.Cmd {
	return m.prompt("objective", "", func(text string) tea.Cmd { return m.startMission(m.current(), text) })
}

// Asks for the session title, an empty answer deriving it from the first prompt again
func (m *Model) promptRename() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	return m.prompt("title, empty derives it from the first prompt", d.live.Session.Title(), func(text string) tea.Cmd {
		if strings.TrimSpace(text) != "" {
			return m.command(d, "rename", text)
		}
		d.live.Session.Rename("")
		d.live.Session.SetTitle(firstPrompt(d.live, m.theme))
		m.wm.Repaint()
		return m.wm.Toast("renamed to "+d.live.Session.Title(), wm.Success, 0)
	})
}

// Label the first prompt of a session derives, empty before any
func firstPrompt(l *harness.Live, t theme.Theme) string {
	for _, e := range l.Session.Entries() {
		text := protocol.Response{Content: e.Content}.Text()
		if e.Role == protocol.RoleUser && strings.TrimSpace(text) != "" {
			return render.Summary(text, 60, t)
		}
	}
	return ""
}

func (m *Model) promptExport() tea.Cmd {
	d := m.current()
	if d == nil {
		return nil
	}
	return m.prompt("path", m.exportPath(d), func(text string) tea.Cmd { return m.exportTranscript(d, strings.TrimSpace(text)) })
}

// Windows a workspace can open, by slash command name, each opened by the keymap action named
var panels = []struct{ name, title, detail, action string }{
	{"new", "Session", "fresh session", "session.new"},
	{"agents", "Agents", "runs", "open.agents"},
	{"metrics", "Metrics", "tokens, cost, latency", "open.metrics"},
	{"jobs", "Jobs", "shell jobs", "open.jobs"},
	{"findings", "Findings", "recorded facts", "open.findings"},
	{"sessions", "Sessions", "stored sessions", "open.sessions"},
	{"config", "Config", "profiles, models, rules, hooks, mcp, tools, keys", "open.config"},
}

func (m *Model) pickPanel() tea.Cmd {
	d := m.current()
	items := make([]wm.Item, 0, len(panels))
	for _, p := range panels {
		name := p.name
		var marks []string
		if k := m.keys.Display(p.action); k != "" {
			marks = append(marks, k)
		}
		open := m.panel(name) != nil && m.wm.Has(name)
		if name == "findings" && d != nil {
			open = m.wm.Has("findings:" + d.live.Session.ID())
		}
		if open {
			marks = append(marks, "open")
		}
		items = append(items, wm.Item{ID: name, Title: p.title, Detail: p.detail, Keys: strings.Join(marks, " "+m.theme.Glyphs.Sep+" "), Run: func() tea.Cmd { return m.command(d, name, "") }})
	}
	return m.pick("panel", items)
}
