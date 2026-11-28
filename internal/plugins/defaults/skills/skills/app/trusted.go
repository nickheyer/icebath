package app

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Opens the trusted projects: the folders whose own skills load, each no longer trusted with enter after the question, the row that adds one
// asked as a folder, and trusting the project the session works in while it is not
func (s *screen) trusted() tea.Cmd {
	t := s.kit.Env.Theme
	folders := func() []string { return s.state.GetTrusted() }
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Trusted projects",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":trusted",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"Project folders whose own skills load for sessions working in them."}
			if len(folders()) == 0 {
				lines = append(lines, "No trusted projects yet.")
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			rows := make([][]string, 0, len(folders())+2)
			for _, folder := range folders() {
				rows = append(rows, []string{folder, "", ""})
			}
			rows = append(rows, []string{"Add a folder", render.Dim(t.Glyphs.Forward, t), ""})
			if a := s.trustThis(); a != nil {
				rows = append(rows, []string{a.title, "", ""})
			}
			return rows
		},
		Detail: func(i int) []string {
			switch n := len(folders()); {
			case i < n:
				return []string{"Its skills load from its .icebath/skills and .agents/skills folders."}
			case i == n:
				return []string{"A project folder of yours, such as ~/code/project."}
			}
			if a := s.trustThis(); a != nil {
				return []string{a.about}
			}
			return nil
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch n := len(folders()); {
			case i < 0:
				return []wm.Hint{{Action: "hub.add", Label: "add"}}
			case i < n:
				return []wm.Hint{{Action: "hub.use", Label: "stop trusting"}, {Action: "hub.add", Label: "add"}}
			case i == n:
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "trust"}, {Action: "hub.add", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch n := len(folders()); {
				case i < n:
					return s.untrust(l, folders()[i])
				case i == n:
					return s.addFolder(l)
				}
				if a := s.trustThis(); a != nil {
					return a.run(l)
				}
				return nil
			},
			"hub.add": func(l *cards.List, _ int) tea.Cmd { return s.addFolder(l) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < len(folders()) {
					return s.untrust(l, folders()[i])
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	s.views.Add(list)
	return s.hub.Push(list)
}

// Asks the folder of a project to trust, tab completing it, then trusts it, in hand on the list. The server finds the folder, on its
// computer and by its own home folder, so the row put in hand is the one the server added, whatever path that came to
func (s *screen) addFolder(list *cards.List) tea.Cmd {
	field := cards.Field{Label: "Folder", Path: cards.PathFolder, Check: func(typed string) string {
		if typed != "" && typed[0] != '/' && typed[0] != '~' {
			return "Write the whole path of the folder, starting with / or ~, such as ~/code/project."
		}
		return ""
	}, Help: "The folder of the project, such as ~/code/project. Tab completes it."}
	return s.kit.Ask(s.hub, "Add a folder", field, "Trust", func(folder string) tea.Cmd {
		before := slices.Clone(s.state.GetTrusted())
		return s.trust(folder, false, func() {
			list.Select(slices.IndexFunc(s.state.GetTrusted(), func(now string) bool { return !slices.Contains(before, now) }))
		})
	})
}

// Stops trusting a folder after the question
func (s *screen) untrust(l *cards.List, folder string) tea.Cmd {
	l.Confirm(cards.Question{Text: "Stop trusting " + folder + "? Its own skills no longer load.", Verb: "Stop trusting"},
		func() tea.Cmd { return s.trust(folder, true, nil) })
	return nil
}
