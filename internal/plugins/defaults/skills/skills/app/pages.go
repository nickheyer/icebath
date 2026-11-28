package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pages inside Skills that a search of Settings finds: finding skills, the bundles, the sources, the trusted projects, the skills you changed
// and the settings of the skills hub at once; each category, each skill under its category, each bundle and the sections of options a
// skill asks for from the last reading of the skills, read again behind, as reading every skill takes time
func skillPages(kit *tui.Kit, service skillsv1connect.SkillsServiceClient, name string) []tui.Page {
	pages := []tui.Page{
		{Title: "Find skills", About: "Browse, search and install skills from the skill hub", Words: "hub install browse", Argument: "hub"},
		{Title: "Bundles", About: "Several skills loaded at once with one slash command", Words: "bundle", Argument: "bundles"},
		{Title: "Sources", About: "GitHub repositories the skill hub searches", Words: "tap repository", Argument: "sources"},
		{Title: "Trusted projects", About: "Project folders whose own skills load", Words: "trust project folder", Argument: "trusted"},
		{Title: "Skills you changed", About: "Built-in skills you edited, and skills you made yourself", Words: "edited publish export",
			Argument: changedArg},
		{Title: sectionTitles[sections[2]], About: sectionAbouts[sections[2]], Argument: sectionOf + sections[2], Keys: []string{sections[2] + "."}},
	}
	return append(pages, tui.Remembered("skills:"+name, func() []tui.Page { return readPages(kit, service, name) })...)
}

// Pages of the skills as they are now: each category, each skill under its category, each bundle, and the sections of secrets and options
// while a skill asks for one; nil when the skills can't be read
func readPages(kit *tui.Kit, service skillsv1connect.SkillsServiceClient, name string) []tui.Page {
	scope := &skillsv1.Scope{Profile: name, Directory: kit.Env.WorkDir()}
	response, err := service.List(kit.Env.Ctx, &skillsv1.ListRequest{Scope: scope, Surface: surface})
	if err != nil {
		return nil
	}
	pages := []tui.Page{}
	for _, category := range response.GetCategories() {
		about := strings.TrimSuffix(category.GetAbout(), ".")
		if about == "" {
			about = "Skills of " + category.GetTitle()
		}
		pages = append(pages, tui.Page{Title: category.GetTitle(), About: about, Words: "skills category", Argument: categoryOf + category.GetName()})
	}
	for _, skill := range response.GetSkills() {
		pages = append(pages, tui.Page{Title: skill.GetName(), Trail: []string{skill.GetCategoryTitle()}, About: oneLine(skill.GetDescription()),
			Words: "skill", Argument: skillOf + skill.GetName()})
	}
	if bundled, err := service.Bundles(kit.Env.Ctx, &skillsv1.BundlesRequest{Scope: scope}); err == nil {
		for _, b := range bundled.GetBundles() {
			pages = append(pages, tui.Page{Title: "/" + b.GetSlug(), Trail: []string{"Bundles"}, About: "A bundle of " + strs.And(b.GetSkills()),
				Words: "bundle " + b.GetName(), Argument: bundleOf + b.GetName()})
		}
	}
	held := map[string]bool{sections[0]: response.GetSecrets() > 0, sections[1]: response.GetValues() > 0}
	for _, key := range sections[:2] {
		if held[key] {
			pages = append(pages, tui.Page{Title: sectionTitles[key], About: sectionAbouts[key], Argument: sectionOf + key, Keys: []string{key + "."}})
		}
	}
	return pages
}

// Titles and descriptions of the sections of options, as the pages a search finds say them before the settings are read
var (
	sectionTitles = map[string]string{sections[0]: "Skill secrets", sections[1]: "Skill options", sections[2]: "Skills hub"}
	sectionAbouts = map[string]string{sections[0]: "API keys and other secrets that your skills need", sections[1]: "Values that your installed skills read",
		sections[2]: "How the skills hub signs in to GitHub"}
)

// Opens a section of the options of the area over its page: its everyday settings, and its advanced ones in place while few, else behind
// a row of their own
func (s *screen) sectionPage(key string) tea.Cmd {
	t := s.kit.Env.Theme
	info := s.options.Info(key)
	if info == nil {
		return s.hub.Fail("The Skills page has no section " + key + ".")
	}
	var shown []*configv1.Setting
	var advanced bool
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   info.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     Screen + ":" + key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{described(info.GetDescription())}
			if len(shown) == 0 && !advanced {
				lines = append(lines, "None of your skills asks for one yet.")
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			everyday, rare := s.options.Own(key)
			shown = everyday
			_, _, advanced = s.options.OwnAdvanced(key)
			if !advanced {
				shown = append(shown, rare...)
			}
			cells := make([][]string, 0, len(shown)+1)
			for _, setting := range shown {
				cells = append(cells, s.options.Cells(setting))
			}
			if advanced {
				row, _, _ := s.options.OwnAdvanced(key)
				cells = append(cells, row)
			}
			return cells
		},
		Detail: func(i int) []string {
			if i >= len(shown) {
				return []string{"Settings that few people change."}
			}
			return s.options.Detail(shown[i], view.Cut(i, 2))
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return nil
			case i >= len(shown):
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			}
			return s.options.Hints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					_, open, _ := s.options.OwnAdvanced(key)
					return open()
				}
				return s.options.Change(shown[i])
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					return nil
				}
				return s.options.Reset(l, shown[i])
			},
		},
	})
	view.ToggleDetail()
	s.options.Watch(view)
	return s.hub.Push(view)
}
