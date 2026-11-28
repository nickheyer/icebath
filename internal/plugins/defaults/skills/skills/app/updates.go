package app

import (
	"cmp"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Words for how a skill from the hub stands against its source
var checkWords = map[string]string{"up_to_date": "Up to date", "update_available": "Update available", "unavailable": "Can't be checked",
	"orphaned": "Gone from its source", "invalid_install": "Install is broken"}

// Opens the skills you installed from the hub, each with how it stands against its source as soon as that is checked, enter opening its
// page; then updating them all, over your changes or not, and scanning them again
func (s *screen) updates() tea.Cmd {
	t := s.kit.Env.Theme
	var (
		checked map[string]string
		loaded  bool
		failed  error
		list    *cards.List
	)
	installed := func() []*skillsv1.Skill {
		var out []*skillsv1.Skill
		for _, skill := range s.state.GetSkills() {
			if skill.GetSource() == "hub" {
				out = append(out, skill)
			}
		}
		return out
	}
	// How the skills stand against their sources, asked again each time the page comes back in view after an update ran, as one may have
	// from the page of a skill opened here
	seen := s.updated
	check := func() tea.Cmd {
		seen = s.updated
		return tui.Async(func() func() tea.Cmd {
			response, err := s.service.Check(s.kit.Env.Ctx, &skillsv1.CheckRequest{Scope: s.scope()})
			return func() tea.Cmd {
				loaded, failed = true, err
				checked = map[string]string{}
				for _, c := range response.GetSkills() {
					checked[c.GetName()] = c.GetStatus()
				}
				list.Refresh()
				return nil
			}
		})
	}
	// An update leaves the skills other than they were checked, so they are read and checked again
	again := func() tea.Cmd {
		loaded = false
		return tea.Batch(s.reload(), check())
	}
	acts := []*act{
		{title: "Update every skill", about: "Gets the newest version of each skill, keeping those you changed as they are.", verb: "update",
			run: func(*cards.List) tea.Cmd { return s.update("", false, again) }},
		{title: "Update over your changes", about: "Gets the newest version of each skill, replacing changes you made.", verb: "update",
			run: func(*cards.List) tea.Cmd { return s.update("", true, again) }},
		{title: "Scan again", about: "Looks for danger in each of these skills again.", verb: "scan",
			run: func(*cards.List) tea.Cmd { return s.audit() }},
	}
	var shown []*skillsv1.Skill
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Updates",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":updates",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"The skills you installed from the skill hub."}
			switch {
			case len(installed()) == 0:
				lines = append(lines, "None is installed now.")
			case failed != nil:
				lines = append(lines, render.Warm("They could not be checked. "+tui.Reason(failed), t))
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = installed()
			rows := make([][]string, 0, len(shown)+len(acts))
			for _, skill := range shown {
				state := render.Dim("Checking…", t)
				if loaded {
					state = s.checkState(checked[skill.GetName()])
				}
				rows = append(rows, []string{skill.GetName(), "", state})
			}
			if len(shown) == 0 {
				return rows
			}
			for _, a := range acts {
				rows = append(rows, []string{a.title, "", ""})
			}
			return rows
		},
		Detail: func(i int) []string {
			switch {
			case i < 0:
				return nil
			case i < len(shown):
				return []string{described(shown[i].GetDescription())}
			}
			return []string{acts[i-len(shown)].about}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case i < len(shown):
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: acts[i-len(shown)].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i < len(shown) {
					return s.skillPage(shown[i].GetName())
				}
				return acts[i-len(shown)].run(l)
			},
		},
		OnFocus: func(*cards.List) tea.Cmd {
			if seen == s.updated {
				return nil
			}
			loaded = false
			list.Refresh()
			return check()
		},
	})
	list.ToggleDetail()
	s.views.Add(list)
	return tea.Batch(s.hub.Push(list), check())
}

// How a skill from the hub stands against its source, in its words and the colour of what it means
func (s *screen) checkState(status string) string {
	t := s.kit.Env.Theme
	said := cmp.Or(checkWords[status], "Can't be checked")
	switch status {
	case "update_available":
		return render.Warm(said, t)
	case "up_to_date":
		return render.Good(said, t)
	case "":
		return render.Dim(said, t)
	}
	return render.Bad(said, t)
}
