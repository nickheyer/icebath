package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Opens the sources: GitHub repositories the skill hub searches besides its registries, each removed with enter after the question, and the
// row that adds one
func (s *screen) sources() tea.Cmd {
	t := s.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Sources",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":sources",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"GitHub repositories the skill hub searches besides its registries."}
			if len(s.taps) == 0 {
				lines = append(lines, "No sources yet.")
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			rows := make([][]string, 0, len(s.taps)+1)
			for _, tap := range s.taps {
				rows = append(rows, []string{tap.GetRepo(), "", ""})
			}
			return append(rows, []string{"Add a source", render.Dim(t.Glyphs.Forward, t), ""})
		},
		Detail: func(i int) []string {
			if i >= len(s.taps) {
				return []string{"A GitHub repository that holds skills."}
			}
			return []string{"Its skills are in its " + cmp.Or(strings.TrimSuffix(s.taps[i].GetPath(), "/"), "skills") + " folder."}
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add", Label: "add"}}
			case i >= len(s.taps):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "remove"}, {Action: "hub.add", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i >= len(s.taps) {
					return s.addSource(l)
				}
				return s.removeSource(l, s.taps[i].GetRepo())
			},
			"hub.add": func(l *cards.List, _ int) tea.Cmd { return s.addSource(l) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(s.taps) {
					return nil
				}
				return s.removeSource(l, s.taps[i].GetRepo())
			},
		},
	})
	list.ToggleDetail()
	s.views.Add(list)
	return s.hub.Push(list)
}

// Asks the repository of a new source, then adds it, in hand on the list
func (s *screen) addSource(list *cards.List) tea.Cmd {
	field := cards.Field{Label: "Repository", Check: func(typed string) string {
		if owner, name, ok := strings.Cut(strings.TrimSpace(typed), "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return "Write the repository as owner/name, such as openai/skills."
		}
		return ""
	}, Help: "A GitHub repository of skills, written owner/name, such as openai/skills."}
	return s.kit.Ask(s.hub, "Add a source", field, "Add", func(repo string) tea.Cmd {
		return s.tap(repo, false, func() {
			list.Select(slices.IndexFunc(s.taps, func(tap *skillsv1.SkillTap) bool { return tap.GetRepo() == repo }))
		})
	})
}

// Removes a source after the question
func (s *screen) removeSource(l *cards.List, repo string) tea.Cmd {
	l.Confirm(cards.Question{Text: "Remove " + repo + "? The skill hub stops searching it, and skills installed from it stay.", Verb: "Remove"},
		func() tea.Cmd { return s.tap(repo, true, nil) })
	return nil
}

// Adds a source or removes it, the note saying what happened; once it was added and the sources are read again, after puts it in hand
func (s *screen) tap(repo string, remove bool, after func()) tea.Cmd {
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*skillsv1.TapResponse, error) {
		return s.service.Tap(s.kit.Env.Ctx, &skillsv1.TapRequest{Scope: s.scope(), Repo: repo, Remove: remove})
	}, func(response *skillsv1.TapResponse) tea.Cmd {
		switch {
		case remove && response.GetChanged():
			return tea.Batch(s.hub.Done("Removed "+repo+"."), s.reload())
		case remove:
			return s.hub.Fail(repo + " is not one of your sources.")
		case response.GetChanged():
			s.after = after
			return tea.Batch(s.hub.Done("Added "+repo+". The skill hub searches it now."), s.reload())
		}
		return s.hub.Done(repo + " is one of your sources already.")
	})
}
