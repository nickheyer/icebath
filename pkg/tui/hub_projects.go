package tui

import (
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the projects screen
const projectsContext = "tui.projects"

// Keys of the projects screen, the shared hub keys and none of its own
func registerProjectsKeys(km *keys.Map) {
	if keys.ContextDocs[projectsContext] == "" {
		if err := keys.AddContext(projectsContext, "projects screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "projects", order: 34, title: txt.HubsTitleProjects, short: txt.HubsShortProjects, slash: "projects",
		open: func(a *app, _ string) tea.Cmd { return a.openProjects() }, keys: registerProjectsKeys})
}

// State of the open projects screen: the rows as last read and the hub they show in
type projectsHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	rows    []tuigateway.ProjectRow
	loading bool
}

// Projects screen: every project with its folders and what it is for, the one new sessions work in marked
func (a *app) openProjects() tea.Cmd {
	p := &projectsHub{a: a, loading: true}
	p.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleProjects), Glyph: a.toolkit.Glyphs.Plan, Key: "tui:projects", Context: projectsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: ""}, {Title: tr(txt.HubsColName), Max: 28}, {Title: tr(txt.HubsProjectsColFolders), Max: 44}, {Title: tr(txt.HubsProfilesColFor), Flex: 1}},
		Yield:   []int{3, 2},
		Rows:    p.cells,
		Detail:  p.detail,
		Status: func() string {
			if len(p.rows) == 0 {
				return ""
			}
			return tr(txt.HubsProjectsLegend)
		},
		Empty: func() string {
			if p.loading {
				return tr(txt.HubsLoading)
			}
			return tr(txt.HubsProjectsEmpty)
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if row, ok := p.at(i); ok {
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)})
				if !row.Archived || row.Active {
					out = append(out, wm.Hint{Action: "hub.default", Label: strings.ToLower(p.useLabel(row))})
				}
				out = append(out, wm.Hint{Action: "hub.rename"}, wm.Hint{Action: "hub.edit", Label: strings.ToLower(tr(txt.HubsProfilesDescribe))},
					wm.Hint{Action: "hub.toggle", Label: strings.ToLower(p.archiveLabel(row))})
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.default": func(_ *cards.List, i int) tea.Cmd { return p.use(i) },
			"hub.edit":    func(_ *cards.List, i int) tea.Cmd { return p.describe(i) },
			"hub.rename":  func(_ *cards.List, i int) tea.Cmd { return p.rename(i) },
			"hub.toggle":  func(_ *cards.List, i int) tea.Cmd { return p.archive(i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return p.load() },
	})
	p.h = a.newHub("tui:projects", p.list)
	return a.wm.ShowOverlay(p.h)
}

// Reads the projects again, a failure staying in the hub
func (p *projectsHub) load() tea.Cmd {
	return hubCall(p.a, p.h, "", tuigateway.ProjectsList, tuigateway.ProjectsListParams{}, func(r *tuigateway.ProjectsListResult) tea.Cmd {
		p.loading, p.rows = false, r.Projects
		p.list.Refresh()
		return nil
	})
}

// Sends one change and shows the projects it answers, back on the list, the note naming what was saved
func (p *projectsHub) change(params tuigateway.ProjectsChangeParams, name string) tea.Cmd {
	back := p.h.PopTo(p.list)
	return tea.Batch(back, hubCall(p.a, p.h, tr(txt.HubsProfilesSaving), tuigateway.ProjectsChange, params, func(r *tuigateway.ProjectsChangeResult) tea.Cmd {
		p.loading, p.rows = false, r.Projects
		p.list.Refresh()
		return p.h.Done(tr(txt.HubsCronSaved, name))
	}))
}

// Row at i, false past the rows
func (p *projectsHub) at(i int) (tuigateway.ProjectRow, bool) {
	if i < 0 || i >= len(p.rows) {
		return tuigateway.ProjectRow{}, false
	}
	return p.rows[i], true
}

// Folder path as typed made absolute: a leading tilde is the user home and a relative path is under the folder of the session
func (p *projectsHub) folderPath(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if text == "~" || strings.HasPrefix(text, "~/") {
		if userHome, err := home.UserHome(); err == nil {
			text = filepath.Join(userHome, strings.TrimPrefix(text, "~"))
		}
	}
	if !filepath.IsAbs(text) {
		text = filepath.Join(p.a.store.sessionCwd(), text)
	}
	return filepath.Clean(text)
}

// Word for the action that starts or stops working in a project
func (p *projectsHub) useLabel(row tuigateway.ProjectRow) string {
	if row.Active {
		return tr(txt.HubsProjectsClear)
	}
	return tr(txt.HubsProjectsUse)
}

// Word for the action that archives a project or brings it back
func (p *projectsHub) archiveLabel(row tuigateway.ProjectRow) string {
	if row.Archived {
		return tr(txt.HubsProjectsRestore)
	}
	return tr(txt.HubsKanbanActArchive)
}

// Cells of every project: the one new sessions work in marked, its main folder and how many more it covers
func (p *projectsHub) cells() [][]string {
	out := make([][]string, len(p.rows))
	for i, row := range p.rows {
		mark := ""
		if row.Active {
			mark = "●"
		}
		name := row.Name
		if row.Archived {
			name += " · " + tr(txt.HubsSessionsStateArchived)
		}
		folders := ""
		if len(row.Folders) > 0 {
			folders = home.Display(row.Folders[0].Path)
			for _, f := range row.Folders {
				if f.Primary {
					folders = home.Display(f.Path)
				}
			}
			if more := len(row.Folders) - 1; more > 0 {
				folders += " +" + strconv.Itoa(more)
			}
		}
		out[i] = []string{mark, name, folders, strings.ReplaceAll(row.Description, "\n", " ")}
	}
	return out
}

// Lines of a project's detail pane: what it is for, its board and every folder, the main one marked
func (p *projectsHub) detail(i int) []string {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	out := []string{row.Name + " · " + row.Slug}
	if row.Description != "" {
		out = append(out, row.Description)
	}
	if row.Active {
		out = append(out, tr(txt.HubsProjectsDetailActive))
	}
	if row.Board != "" {
		out = append(out, tr(txt.HubsProjectsDetailBoard, row.Board))
	}
	for _, f := range row.Folders {
		line := "  " + home.Display(f.Path)
		if f.Primary {
			line = "★ " + home.Display(f.Path)
		}
		if f.Label != "" {
			line += " (" + f.Label + ")"
		}
		out = append(out, line)
	}
	return out
}

// Actions screen of a project
func (p *projectsHub) open(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	return p.a.hubActions(p.h, row.Name, func() []string { return p.detail(i) }, func() []hubAction {
		var acts []hubAction
		switch {
		case row.Active:
			acts = append(acts, hubAction{key: "hub.default", label: tr(txt.HubsProjectsClear), does: tr(txt.HubsProjectsClearDoes), run: func() tea.Cmd { return p.use(i) }})
		case !row.Archived:
			acts = append(acts, hubAction{key: "hub.default", label: tr(txt.HubsProjectsUse), does: tr(txt.HubsProjectsUseDoes), run: func() tea.Cmd { return p.use(i) }})
		}
		acts = append(acts,
			hubAction{key: "hub.rename", label: tr(txt.HubsProfilesRename), does: tr(txt.HubsProjectsRenameDoes), run: func() tea.Cmd { return p.rename(i) }},
			hubAction{key: "hub.edit", label: tr(txt.HubsProfilesDescribe), does: tr(txt.HubsProjectsDescribeDoes), run: func() tea.Cmd { return p.describe(i) }},
			hubAction{label: tr(txt.HubsProjectsAddFolder), does: tr(txt.HubsProjectsAddFolderDoes), run: func() tea.Cmd { return p.addFolder(i) }})
		if len(row.Folders) > 0 {
			acts = append(acts, hubAction{label: tr(txt.HubsProjectsRemoveFolder), does: tr(txt.HubsProjectsRemoveFolderDoes), run: func() tea.Cmd { return p.removeFolder(i) }})
		}
		if len(row.Folders) > 1 {
			acts = append(acts, hubAction{label: tr(txt.HubsProjectsSetMain), does: tr(txt.HubsProjectsSetMainDoes), run: func() tea.Cmd { return p.setMain(i) }})
		}
		acts = append(acts, hubAction{label: tr(txt.HubsProjectsBindBoard), does: tr(txt.HubsProjectsBindBoardDoes), run: func() tea.Cmd { return p.bindBoard(i) }})
		if row.Archived {
			return append(acts, hubAction{key: "hub.toggle", label: tr(txt.HubsProjectsRestore), does: tr(txt.HubsProjectsRestoreDoes), run: func() tea.Cmd { return p.archive(i) }})
		}
		return append(acts, hubAction{key: "hub.toggle", label: tr(txt.HubsKanbanActArchive), does: tr(txt.HubsProjectsArchiveDoes), run: func() tea.Cmd { return p.archive(i) }})
	})
}

// Form of a new project: its name, the folders it covers, what it is for and whether new sessions work in it at once
func (p *projectsHub) add() tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsProjectsNew), Submit: tr(txt.HubsProfilesCreateSubmit), Fields: []cards.Field{
		{ID: "name", Label: tr(txt.HubsColName), Required: true},
		{ID: "folders", Label: tr(txt.HubsProjectsColFolders), Help: tr(txt.HubsProjectsFoldersHelp), Value: home.Display(p.a.store.sessionCwd())},
		{ID: "about", Label: tr(txt.HubsProfilesColFor)},
		{ID: "use", Label: tr(txt.HubsProjectsUseNow), Kind: cards.FieldBool, Value: "true"},
	}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		var folders []string
		for _, part := range strings.Split(values["folders"], ",") {
			if path := p.folderPath(part); path != "" {
				folders = append(folders, path)
			}
		}
		name := strings.TrimSpace(values["name"])
		params := tuigateway.ProjectsChangeParams{Verb: "create", Name: name, Folders: folders, Description: values["about"], Use: values["use"] == "true"}
		return p.change(params, name)
	})
}

// Starts working in a project, or stops when new sessions already work in it
func (p *projectsHub) use(i int) tea.Cmd {
	row, ok := p.at(i)
	switch {
	case !ok:
		return nil
	case row.Active:
		return p.change(tuigateway.ProjectsChangeParams{Verb: "clear"}, row.Name)
	}
	return p.change(tuigateway.ProjectsChangeParams{Verb: "use", Project: row.ID}, row.Name)
}

// Asks for a project's new name
func (p *projectsHub) rename(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	return p.a.hubAsk(p.h, tr(txt.HubsProfilesRename), tr(txt.HubsColName), row.Name, false, func(name string) tea.Cmd {
		if name == "" || name == row.Name {
			return nil
		}
		return p.change(tuigateway.ProjectsChangeParams{Verb: "rename", Project: row.ID, Name: name}, name)
	})
}

// Asks what a project is for, an empty answer clearing it
func (p *projectsHub) describe(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	spec := cards.FormSpec{Title: tr(txt.HubsProfilesDescribe), Fields: []cards.Field{{ID: "about", Label: tr(txt.HubsProfilesColFor), Value: row.Description}}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		return p.change(tuigateway.ProjectsChangeParams{Verb: "describe", Project: row.ID, Description: values["about"]}, row.Name)
	})
}

// Form of one more folder of a project
func (p *projectsHub) addFolder(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	spec := cards.FormSpec{Title: tr(txt.HubsProjectsAddFolder), Fields: []cards.Field{
		{ID: "folder", Label: tr(txt.HubsProjectsFolder), Required: true, Value: home.Display(p.a.store.sessionCwd())},
		{ID: "label", Label: tr(txt.HubsProjectsFolderLabel)},
		{ID: "main", Label: tr(txt.HubsProjectsMainFolder), Kind: cards.FieldBool, Value: "false"},
	}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		params := tuigateway.ProjectsChangeParams{Verb: "add-folder", Project: row.ID, Folder: p.folderPath(values["folder"]), Label: values["label"], Primary: values["main"] == "true"}
		return p.change(params, row.Name)
	})
}

// Folders of a project as choices, the main one first as the service lists them
func projectFolderChoices(row tuigateway.ProjectRow) ([]hubChoice, string) {
	choices, current := make([]hubChoice, len(row.Folders)), ""
	for i, f := range row.Folders {
		choices[i] = hubChoice{id: f.Path, title: home.Display(f.Path), detail: f.Label}
		if f.Primary {
			current = f.Path
		}
	}
	return choices, current
}

// Picks the folder to take out of a project
func (p *projectsHub) removeFolder(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	choices, _ := projectFolderChoices(row)
	return p.a.hubPick(p.h, tr(txt.HubsProjectsRemoveFolder), choices, "", func(c hubChoice) tea.Cmd {
		return p.change(tuigateway.ProjectsChangeParams{Verb: "remove-folder", Project: row.ID, Folder: c.id}, row.Name)
	})
}

// Picks the main folder of a project
func (p *projectsHub) setMain(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	choices, current := projectFolderChoices(row)
	return p.a.hubPick(p.h, tr(txt.HubsProjectsSetMain), choices, current, func(c hubChoice) tea.Cmd {
		return p.change(tuigateway.ProjectsChangeParams{Verb: "set-primary", Project: row.ID, Folder: c.id}, row.Name)
	})
}

// Asks for the kanban board of a project, an empty answer unbinding the one bound now
func (p *projectsHub) bindBoard(i int) tea.Cmd {
	row, ok := p.at(i)
	if !ok {
		return nil
	}
	spec := cards.FormSpec{Title: tr(txt.HubsProjectsBindBoard), Fields: []cards.Field{
		{ID: "board", Label: tr(txt.HubsProjectsBoard), Help: tr(txt.HubsProjectsBoardHelp), Value: row.Board}}}
	return p.a.hubForm(p.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		return p.change(tuigateway.ProjectsChangeParams{Verb: "bind-board", Project: row.ID, Board: values["board"]}, row.Name)
	})
}

// Archives a project, or brings an archived one back
func (p *projectsHub) archive(i int) tea.Cmd {
	row, ok := p.at(i)
	switch {
	case !ok:
		return nil
	case row.Archived:
		return p.change(tuigateway.ProjectsChangeParams{Verb: "restore", Project: row.ID}, row.Name)
	}
	return p.change(tuigateway.ProjectsChangeParams{Verb: "archive", Project: row.ID}, row.Name)
}
