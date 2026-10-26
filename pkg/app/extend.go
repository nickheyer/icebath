package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Workflow named on the command line, started once the program is up
type startWorkflowMsg struct {
	name string
	arg  string
}

// Message that starts a workflow handed on the command line once the program is up
func StartWorkflow(name, arg string) tea.Msg { return startWorkflowMsg{name: name, arg: arg} }

// Interface an extension command drives
type extUI struct{ m *Model }

func (u extUI) Open(c wm.Card) tea.Cmd { return u.m.wm.OpenHere(c) }

func (u extUI) Toast(text string, level wm.Level) tea.Cmd { return u.m.wm.Toast(text, level, 0) }

func (u extUI) Prompt(placeholder, value string, submit func(string) tea.Cmd) tea.Cmd {
	return u.m.prompt(placeholder, value, submit)
}

func (u extUI) Pick(placeholder string, items []wm.Item) tea.Cmd { return u.m.pick(placeholder, items) }

func (u extUI) Submit(live *harness.Live, parts []protocol.Part) tea.Cmd {
	if live == nil {
		return u.m.wm.Toast("no session", wm.Warn, 0)
	}
	return u.m.submit(cards.SubmitMsg{Session: live.Session.ID(), Parts: parts, Text: protocol.Response{Content: parts}.Text()})
}

func (u extUI) Start(workflow, arg string) tea.Cmd { return u.m.startWorkflow(workflow, arg) }

func (u extUI) Env() *cards.Env { return u.m.env }

// Call of an extension command from the session on the stage with the arguments
func (m *Model) call(args string) *ext.Call {
	c := &ext.Call{Ctx: m.ctx, App: m.app, Args: args, UI: extUI{m}}
	if d := m.current(); d != nil {
		c.Live = d.live
	}
	return c
}

// Runs an extension command, asking for its argument first when it takes one and none came
func (m *Model) runExtension(c ext.Command, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if c.Arg != "" && args == "" {
		return m.prompt(c.Arg, "", func(text string) tea.Cmd {
			if strings.TrimSpace(text) == "" {
				return m.wm.Toast(c.Arg+" is required", wm.Warn, 0)
			}
			return c.Run(m.call(strings.TrimSpace(text)))
		})
	}
	return c.Run(m.call(args))
}

// Checks that no extension command, workflow or panel takes a name the app already answers to
func (m *Model) checkExtensions() error {
	taken := map[string]string{}
	for _, c := range m.builtinSlash() {
		taken[c.Name] = "built in slash command"
	}
	for _, p := range builtinPanels {
		taken[p.name] = "built in panel"
	}
	claim := func(name, what string) error {
		if have, ok := taken[name]; ok {
			return fmt.Errorf("%s %s takes the name of a %s", what, name, have)
		}
		taken[name] = what
		return nil
	}
	var errs []error
	for _, c := range ext.Commands() {
		if c.Slash != "" {
			errs = append(errs, claim(c.Slash, "extension command"))
		}
	}
	for _, w := range harness.Workflows() {
		errs = append(errs, claim(w.Name, "workflow"))
	}
	for _, p := range ext.Panels() {
		errs = append(errs, claim(p.Key, "extension panel"))
	}
	return errors.Join(errs...)
}

// Card of an extension panel, built on first use and shared by every workspace
func (m *Model) extPanel(key string) wm.Card {
	if c, ok := m.extPanels[key]; ok {
		return c
	}
	for _, p := range ext.Panels() {
		if p.Key == key {
			c := p.New(m.env)
			m.extPanels[key] = c
			return c
		}
	}
	return nil
}

// Palette commands extensions, workflows, kinds and panels add
func (m *Model) extensionCommands() []wm.Command {
	more := m.theme.Glyphs.Ellipsis
	out := []wm.Command{
		{ID: "kind.set", Title: "Kind" + more, When: func() bool { return m.current() != nil && len(m.app.Kinds()) > 0 }, Run: m.pickKind},
		{ID: "workflow.stop", Title: "Workflow: stop" + more, When: func() bool { return len(m.app.Flows.Active()) > 0 }, Run: m.pickFlow},
	}
	for _, w := range harness.Workflows() {
		name := w.Name
		title := w.Title
		if title == "" {
			title = name
		}
		if w.Arg != "" {
			title += more
		}
		out = append(out, wm.Command{ID: "workflow." + name, Title: "Workflow: " + title, Run: func() tea.Cmd { return m.startWorkflow(name, "") }})
	}
	for _, c := range ext.Commands() {
		c := c
		out = append(out, wm.Command{ID: c.ID, Title: c.Title, When: func() bool { return c.When == nil || c.When(m.call("")) }, Run: func() tea.Cmd { return m.runExtension(c, "") }})
	}
	for _, p := range ext.Panels() {
		key := p.Key
		out = append(out, wm.Command{ID: "open." + key, Title: "Open: " + strings.ToLower(p.Title), Run: func() tea.Cmd { return m.wm.OpenHere(m.extPanel(key)) }})
	}
	return out
}

// Slash commands extensions, workflows, kinds and panels add
func (m *Model) extensionSlash() []cards.Command {
	kindNames := func(string) []string {
		var out []string
		for _, k := range m.app.Kinds() {
			out = append(out, k.Name)
		}
		return append(out, harness.NoKind)
	}
	out := []cards.Command{{Name: "kind", Desc: "run the session as a kind", Arg: "name", Suggest: kindNames, Action: "kind.set"}}
	for _, w := range harness.Workflows() {
		out = append(out, cards.Command{Name: w.Name, Desc: w.Description, Arg: w.Arg, Action: "workflow." + w.Name})
	}
	for _, c := range ext.Commands() {
		if c.Slash != "" {
			out = append(out, cards.Command{Name: c.Slash, Desc: c.Desc, Arg: c.Arg, Action: c.ID})
		}
	}
	for _, p := range ext.Panels() {
		out = append(out, cards.Command{Name: p.Key, Desc: p.Detail, Action: "open." + p.Key})
	}
	return out
}

// Carries out a slash command an extension, workflow, kind or panel answers, false for one none of them knows
func (m *Model) extensionCommand(d *desk, name, args string) (tea.Cmd, bool) {
	if name == "kind" {
		if strings.TrimSpace(args) == "" {
			return m.pickKind(), true
		}
		return m.setKind(d, strings.TrimSpace(args)), true
	}
	if _, ok := harness.LookupWorkflow(name); ok {
		return m.startWorkflow(name, args), true
	}
	for _, c := range ext.Commands() {
		if c.Slash == name {
			return m.runExtension(c, args), true
		}
	}
	if c := m.extPanel(name); c != nil {
		return m.wm.OpenHere(c), true
	}
	return nil, false
}

// Makes the session of the desk run as the kind from its next run, none returning it to the plain agent
func (m *Model) setKind(d *desk, name string) tea.Cmd {
	if d == nil {
		return m.wm.Toast("no session", wm.Warn, 0)
	}
	if name == harness.NoKind {
		name = ""
	}
	if err := m.app.SetKindOn(d.live, name); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	m.wm.Repaint()
	if name == "" {
		return m.wm.Toast("kind cleared", wm.Success, 0)
	}
	return m.wm.Toast("kind "+name, wm.Success, 0)
}

func (m *Model) pickKind() tea.Cmd {
	d := m.current()
	if d == nil {
		return m.wm.Toast("no session", wm.Warn, 0)
	}
	current := d.live.Kind()
	items := []wm.Item{{ID: harness.NoKind, Title: harness.NoKind, Detail: "the plain agent", Keys: m.mark(current == ""), Run: func() tea.Cmd { return m.setKind(d, "") }}}
	for _, k := range m.app.Kinds() {
		name := k.Name
		detail := k.Description
		if k.Source != "" {
			detail = strings.TrimSpace(detail + " " + m.theme.Glyphs.Sep + " " + render.Truncate(k.Source, 40, m.theme))
		}
		items = append(items, wm.Item{ID: name, Title: name, Detail: detail, Keys: m.mark(current == name), Run: func() tea.Cmd { return m.setKind(d, name) }})
	}
	return m.pick("kind", items)
}

// Starts a workflow from the session on the stage, asking for its argument when it takes one and none came
func (m *Model) startWorkflow(name, arg string) tea.Cmd {
	w, ok := harness.LookupWorkflow(name)
	if !ok {
		return m.wm.Toast("no workflow "+name, wm.Warn, 0)
	}
	arg = strings.TrimSpace(arg)
	if w.Arg != "" && arg == "" {
		return m.prompt(w.Arg, "", func(text string) tea.Cmd { return m.startWorkflow(name, text) })
	}
	var live *harness.Live
	if d := m.current(); d != nil {
		live = d.live
	}
	f, err := m.app.Flows.Start(m.ctx, w, live, arg)
	if err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	return m.wm.Toast("workflow "+f.ID+" started", wm.Info, 0)
}

func (m *Model) pickFlow() tea.Cmd {
	var items []wm.Item
	for _, f := range m.app.Flows.Active() {
		id := f.ID
		detail := f.Status()
		if detail == "" {
			detail = render.Truncate(f.Arg, 50, m.theme)
		}
		items = append(items, wm.Item{ID: id, Title: id, Detail: detail, Keys: render.Elapsed(f.Elapsed()), Run: func() tea.Cmd {
			if err := m.app.Flows.Cancel(id); err != nil {
				return m.wm.Toast(err.Error(), wm.Error, 0)
			}
			return m.wm.Toast("stopping "+id, wm.Warn, 0)
		}})
	}
	if len(items) == 0 {
		return m.wm.Toast("no workflow running", wm.Info, 0)
	}
	return m.pick("stop workflow", items)
}

// Toast level of a harness notice
func toastLevel(l harness.Level) wm.Level {
	switch l {
	case harness.Success:
		return wm.Success
	case harness.Warn:
		return wm.Warn
	case harness.Error:
		return wm.Error
	}
	return wm.Info
}

// Routes the messages workflows and extensions raise and reports whether the message was one
func (m *Model) extensionMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case harness.NoticeMsg:
		return m.wm.Toast(render.Truncate(msg.Text, 100, m.theme), toastLevel(msg.Level), 0), true
	case harness.OpenedMsg:
		return m.openDesk(nil, msg.Live), true
	case harness.FlowDoneMsg:
		switch {
		case msg.Err == nil:
			return m.wm.Toast("workflow "+msg.ID+" done in "+render.Elapsed(msg.Duration), wm.Success, 0), true
		case !failed(msg.Err):
			return m.wm.Toast("workflow "+msg.ID+" stopped", wm.Warn, 0), true
		}
		return m.wm.Toast("workflow "+msg.ID+": "+render.Truncate(msg.Err.Error(), 80, m.theme), wm.Error, 8*time.Second), true
	case startWorkflowMsg:
		_, opened := m.ensureDesk()
		return tea.Batch(opened, m.startWorkflow(msg.name, msg.arg)), true
	}
	return nil, false
}

// Extensions as the config tab lists them
func extensionRows() []cards.Extension {
	var out []cards.Extension
	for _, e := range ext.Extensions() {
		row := cards.Extension{Name: e.Name, Version: e.Version, Description: e.Description}
		for _, c := range e.Contributions {
			row.Provides = append(row.Provides, [3]string{c.Point, c.Name, c.Detail})
		}
		out = append(out, row)
	}
	return out
}

// Setting values extensions registered, the defaults for those left unset
func extensionSettings(cfg harness.Config, into map[string]string) {
	for _, s := range harness.Settings() {
		if v := cfg.Setting(s.Key); v != "" {
			into[s.Key] = v
		}
	}
}
