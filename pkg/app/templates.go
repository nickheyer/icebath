package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Templates file written in the configuration directory, shared by every project
const templatesFile = "templates.json"

// Mark a session takes in a template in place of its id, numbered in the order the sessions are met
const sessionMark = "@"

// Every template kept with the one new projects start from
type templates struct {
	Default   string     `json:"default,omitempty"`
	Templates []template `json:"templates,omitempty"`
}

// Arrangement of workspaces kept by name, each session card standing for a fresh session on what the session ran on
//
// Session cards and the cards grown from them carry a mark such as @1 in place of the session id, the mark naming
// the profile, model and kind to open the fresh session with. Cards of a run, a mission or an approval are left
// out since they belong to one moment of one session
type template struct {
	Name  string    `json:"name"`
	Saved time.Time `json:"saved"`
	// Index of the workspace shown once the template is applied
	Active     int                        `json:"active,omitempty"`
	Workspaces []wm.WorkspaceLayout       `json:"workspaces,omitempty"`
	Sessions   map[string]templateSession `json:"sessions,omitempty"`
}

// What a session of a template is opened on, each by name and taken as far as the app still knows it
type templateSession struct {
	Profile string `json:"profile,omitempty"`
	Model   string `json:"model,omitempty"`
	Kind    string `json:"kind,omitempty"`
}

// Template with the name, false when none has it
func (ts templates) find(name string) (template, bool) {
	for _, t := range ts.Templates {
		if t.Name == name {
			return t, true
		}
	}
	return template{}, false
}

// Template new projects start from, false when none is set or the set one is gone
func (ts templates) defaultTemplate() (template, bool) {
	if ts.Default == "" {
		return template{}, false
	}
	return ts.find(ts.Default)
}

// Templates file of the user
func (m *Model) templatesPath() string {
	return filepath.Join(m.configDir(), templatesFile)
}

// Templates as kept on disk, none when the file is missing or unreadable
func (m *Model) loadTemplates() templates {
	var ts templates
	data, err := os.ReadFile(m.templatesPath())
	if err != nil || json.Unmarshal(data, &ts) != nil {
		return templates{}
	}
	return ts
}

func (m *Model) saveTemplates(ts templates) error {
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	path := m.templatesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Template of the layout as it stands, sessions replaced by marks naming what they ran on
func (m *Model) templateOf(name string) template {
	l := m.wm.Layout()
	spec := func(id string) (templateSession, bool) {
		d := m.deskOfSession(id)
		if d == nil {
			return templateSession{}, false
		}
		return templateSession{Profile: d.live.Endpoint.Profile.Name, Model: d.live.Endpoint.Model, Kind: d.live.Kind()}, true
	}
	return templateFrom(name, l, spec)
}

// Template of a saved layout, sessions replaced by marks and described through spec, those it does not know left out
func templateFrom(name string, l wm.Layout, spec func(id string) (templateSession, bool)) template {
	t := template{Name: name, Saved: time.Now().UTC(), Active: l.Active, Sessions: map[string]templateSession{}}
	marks := map[string]string{}
	rename := func(key string) string {
		kind, rest, _ := strings.Cut(key, ":")
		switch kind {
		case "session", "findings":
			mark, ok := marks[rest]
			if !ok {
				s, known := spec(rest)
				if !known {
					return ""
				}
				mark = sessionMark + strconv.Itoa(len(marks)+1)
				marks[rest] = mark
				t.Sessions[mark] = s
			}
			return kind + ":" + mark
		case "mission", "agent", "approval":
			return ""
		}
		return key
	}
	for i, wl := range l.Workspaces {
		t.Workspaces = append(t.Workspaces, wl.Rekeyed("w"+strconv.Itoa(i+1), rename))
	}
	if t.Active < 0 || t.Active >= len(t.Workspaces) {
		t.Active = 0
	}
	return t
}

// Writes the layout as it stands as a template under the name, replacing one with the same name
func (m *Model) saveTemplate(name string) tea.Cmd {
	name = strings.TrimSpace(name)
	if name == "" {
		return m.wm.Toast("no template named", wm.Warn, 0)
	}
	ts := m.loadTemplates()
	t := m.templateOf(name)
	if i := slices.IndexFunc(ts.Templates, func(have template) bool { return have.Name == name }); i >= 0 {
		ts.Templates[i] = t
	} else {
		ts.Templates = append(ts.Templates, t)
	}
	if err := m.saveTemplates(ts); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	return m.wm.Toast(fmt.Sprintf("template %s saved, %d workspaces", name, len(t.Workspaces)), wm.Success, 0)
}

// Makes the template the one new projects start from, an empty name unsetting it
func (m *Model) setDefaultTemplate(name string) tea.Cmd {
	name = strings.TrimSpace(name)
	ts := m.loadTemplates()
	if name != "" {
		if _, ok := ts.find(name); !ok {
			return m.wm.Toast("no template "+name, wm.Warn, 0)
		}
	}
	ts.Default = name
	if err := m.saveTemplates(ts); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	if name == "" {
		return m.wm.Toast("default template unset", wm.Success, 0)
	}
	return m.wm.Toast("default template "+name, wm.Success, 0)
}

func (m *Model) deleteTemplate(name string) tea.Cmd {
	name = strings.TrimSpace(name)
	ts := m.loadTemplates()
	i := slices.IndexFunc(ts.Templates, func(have template) bool { return have.Name == name })
	if i < 0 {
		return m.wm.Toast("no template "+name, wm.Warn, 0)
	}
	ts.Templates = slices.Delete(ts.Templates, i, i+1)
	if ts.Default == name {
		ts.Default = ""
	}
	if err := m.saveTemplates(ts); err != nil {
		return m.wm.Toast(err.Error(), wm.Error, 0)
	}
	return m.wm.Toast("deleted template "+name, wm.Success, 0)
}

// Template with the endpoints of its sessions opened, ready to be placed on the event loop
type templateMsg struct {
	t   template
	eps map[string]harness.Endpoint
}

// Opens the endpoint of every session of the template, the default standing in for any that will not open
func (m *Model) resolveTemplate(t template) map[string]harness.Endpoint {
	eps := map[string]harness.Endpoint{}
	for mark, s := range t.Sessions {
		ep, err := m.app.EndpointNamed(m.ctx, s.Profile, s.Model)
		if err != nil {
			ep = m.app.Primary
		}
		eps[mark] = ep
	}
	return eps
}

// Opens the endpoints of the template off the event loop, then adds its workspaces beside the open ones
func (m *Model) applyTemplate(name string) tea.Cmd {
	t, ok := m.loadTemplates().find(strings.TrimSpace(name))
	if !ok {
		return m.wm.Toast("no template "+name, wm.Warn, 0)
	}
	return tea.Batch(m.wm.Toast("applying template "+t.Name, wm.Info, 0), func() tea.Msg {
		return templateMsg{t: t, eps: m.resolveTemplate(t)}
	})
}

// Adds the workspaces of the template after the open ones and shows the one it marks active
func (m *Model) placeTemplate(t template, eps map[string]harness.Endpoint) []tea.Cmd {
	cmds, placed, active := m.placeWorkspaces(t, eps, nil, false)
	if placed == 0 {
		return append(cmds, m.wm.Toast("template "+t.Name+" placed nothing", wm.Warn, 0))
	}
	cmds = append(cmds, m.wm.Switch(active))
	ws := m.wm.Workspaces()[active]
	if d := m.deskOfCard(ws.Focused()); d != nil {
		m.app.Select(d.live)
	} else if seated := m.desksIn(ws); len(seated) > 0 {
		m.app.Select(seated[0].live)
	}
	return append(cmds, m.wm.Toast(fmt.Sprintf("template %s, %d workspaces", t.Name, placed), wm.Success, 0))
}

// Adds the workspaces of the template after the open ones, returning how many it placed and the index of the active one
//
// A fresh session is opened for every mark on the endpoint resolved for it. The session spare when handed in takes the
// first mark met when claim is set, else the first mark on its own endpoint. Cards no session or panel answers to are
// left out, as is a workspace nothing is left in
func (m *Model) placeWorkspaces(t template, eps map[string]harness.Endpoint, spare *harness.Live, claim bool) ([]tea.Cmd, int, int) {
	var cmds []tea.Cmd
	base := len(m.wm.Workspaces())
	made := map[string]string{}
	open := func(mark string) string {
		if id, ok := made[mark]; ok {
			return id
		}
		spec := t.Sessions[mark]
		ep, ok := eps[mark]
		if !ok {
			ep = m.app.Primary
		}
		var live *harness.Live
		if spare != nil && m.deskOfLive(spare) == nil && (claim || ep.Profile.Name == spare.Endpoint.Profile.Name && ep.Model == spare.Endpoint.Model) {
			live = spare
			if !claim && spec.Kind != "" && live.Kind() == "" {
				_ = m.app.SetKindOn(live, spec.Kind)
			}
		} else {
			var err error
			if live, err = m.app.CreateOn(ep, spec.Kind); err != nil {
				made[mark] = ""
				cmds = append(cmds, m.wm.Toast(err.Error(), wm.Error, 0))
				return ""
			}
		}
		m.addDesk(live)
		made[mark] = live.Session.ID()
		return made[mark]
	}
	terms := map[string]string{}
	rename := func(key string) string {
		kind, rest, _ := strings.Cut(key, ":")
		switch kind {
		case "session", "findings":
			if id := open(rest); id != "" {
				return kind + ":" + id
			}
			return ""
		case "mission", "agent", "approval":
			return ""
		case "terminal":
			if fresh, ok := terms[rest]; ok {
				return fresh
			}
			terms[rest] = m.terminalKey()
			return terms[rest]
		}
		if m.panel(key) == nil || m.wm.Has(key) {
			return ""
		}
		return key
	}
	placed, active := 0, base
	for i, wl := range t.Workspaces {
		wl = wl.Rekeyed("", rename)
		if len(wl.Tiles) == 0 && len(wl.Floats) == 0 {
			continue
		}
		wl.ID = m.workspaceID()
		ws, cmd := m.wm.AddWorkspace(wl.ID)
		cmds = append(cmds, cmd, m.wm.RestoreWorkspace(ws, wl, m.cardFor))
		placed++
		if i <= t.Active {
			active = base + placed - 1
		}
	}
	return cmds, placed, active
}

// Lists the templates for a picker, each running choose with its name
func (m *Model) pickTemplate(placeholder string, mark func(t template) string, choose func(name string) tea.Cmd) tea.Cmd {
	ts := m.loadTemplates()
	if len(ts.Templates) == 0 {
		return m.wm.Toast("no templates, save one first", wm.Info, 0)
	}
	items := make([]wm.Item, 0, len(ts.Templates)+1)
	for _, t := range ts.Templates {
		name := t.Name
		detail := fmt.Sprintf("%d workspaces %s %d sessions %s %s", len(t.Workspaces), m.theme.Glyphs.Sep, len(t.Sessions), m.theme.Glyphs.Sep, t.Saved.Local().Format("01-02 15:04"))
		items = append(items, wm.Item{ID: name, Title: name, Detail: detail, Keys: mark(t), Run: func() tea.Cmd { return choose(name) }})
	}
	return m.pick(placeholder, items)
}

// Asks for a name, prefilled with the default template when there is one, and saves the layout under it
func (m *Model) promptTemplate() tea.Cmd {
	return m.prompt("template name", m.loadTemplates().Default, m.saveTemplate)
}

func (m *Model) pickApplyTemplate() tea.Cmd {
	def := m.loadTemplates().Default
	return m.pickTemplate("apply template", func(t template) string { return m.mark(t.Name == def) }, m.applyTemplate)
}

// Picks the default template, with a row that unsets it when one is set
func (m *Model) pickDefaultTemplate() tea.Cmd {
	ts := m.loadTemplates()
	if len(ts.Templates) == 0 {
		return m.wm.Toast("no templates, save one first", wm.Info, 0)
	}
	items := make([]wm.Item, 0, len(ts.Templates)+1)
	for _, t := range ts.Templates {
		name := t.Name
		items = append(items, wm.Item{ID: name, Title: name, Detail: fmt.Sprintf("%d workspaces", len(t.Workspaces)), Keys: m.mark(name == ts.Default), Run: func() tea.Cmd { return m.setDefaultTemplate(name) }})
	}
	if ts.Default != "" {
		items = append(items, wm.Item{ID: "", Title: "none", Detail: "new projects start with one fresh session", Run: func() tea.Cmd { return m.setDefaultTemplate("") }})
	}
	return m.pick("default template", items)
}

func (m *Model) pickDeleteTemplate() tea.Cmd {
	def := m.loadTemplates().Default
	return m.pickTemplate("delete template", func(t template) string { return m.mark(t.Name == def) }, m.deleteTemplate)
}

// Carries out the template slash command, save, apply, default or delete with a name after it
func (m *Model) templateCommand(args string) tea.Cmd {
	verb, name, _ := strings.Cut(strings.TrimSpace(args), " ")
	name = strings.TrimSpace(name)
	switch verb {
	case "":
		return m.pickApplyTemplate()
	case "save":
		if name == "" {
			return m.promptTemplate()
		}
		return m.saveTemplate(name)
	case "apply":
		if name == "" {
			return m.pickApplyTemplate()
		}
		return m.applyTemplate(name)
	case "default":
		if name == "" {
			return m.pickDefaultTemplate()
		}
		return m.setDefaultTemplate(name)
	case "delete":
		if name == "" {
			return m.pickDeleteTemplate()
		}
		return m.deleteTemplate(name)
	}
	return m.wm.Toast("template save|apply|default|delete [name]", wm.Warn, 0)
}

// Completions of the template slash command, the verbs then the template names after one
func (m *Model) templateSuggest(prefix string) []string {
	verbs := []string{"save", "apply", "default", "delete"}
	verb, _, typed := strings.Cut(prefix, " ")
	if !typed {
		return verbs
	}
	if !slices.Contains(verbs, verb) {
		return nil
	}
	var out []string
	for _, t := range m.loadTemplates().Templates {
		out = append(out, verb+" "+t.Name)
	}
	return out
}
