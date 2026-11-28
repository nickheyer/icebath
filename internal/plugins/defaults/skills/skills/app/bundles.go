package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/skills"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Bundle of the name as last read, nil for none
func (s *screen) bundle(name string) *skillsv1.Bundle {
	for _, b := range s.bundled {
		if b.GetName() == name {
			return b
		}
	}
	return nil
}

// Opens the bundles: each by its slash command with how many skills it loads, enter opening its page, and the row that adds one
func (s *screen) bundles() tea.Cmd {
	t := s.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Bundles",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":bundles",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			lines := []string{"Several skills loaded at once with one slash command."}
			if len(s.bundled) == 0 {
				lines = append(lines, "No bundles yet.")
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			rows := make([][]string, 0, len(s.bundled)+1)
			for _, b := range s.bundled {
				rows = append(rows, []string{"/" + b.GetSlug(), "", render.Count(len(b.GetSkills()), "skill")})
			}
			return append(rows, []string{"Add a bundle", render.Dim(t.Glyphs.Forward, t), ""})
		},
		Detail: func(i int) []string {
			if i >= len(s.bundled) {
				return []string{"A slash command that loads the skills you tick."}
			}
			b := s.bundled[i]
			lines := []string{"Loads " + strs.And(b.GetSkills()) + "."}
			if b.GetDescription() != "" {
				lines = append(lines, described(b.GetDescription()))
			}
			return lines
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add", Label: "add"}}
			case i >= len(s.bundled):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i >= len(s.bundled) {
					return s.addBundle(l)
				}
				return s.bundlePage(s.bundled[i].GetName())
			},
			"hub.add": func(l *cards.List, _ int) tea.Cmd { return s.addBundle(l) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(s.bundled) {
					return nil
				}
				return s.removeBundle(l, s.bundled[i])
			},
		},
	})
	s.views.Add(list)
	list.ToggleDetail()
	return s.hub.Push(list)
}

// Adds a bundle in two steps: its name, then the skills it loads, ticked from your skills under their categories. Nothing is kept until the
// last step succeeds; the note then says what was made and the new bundle is in hand on the list
func (s *screen) addBundle(list *cards.List) tea.Cmd {
	f := s.kit.Flow(s.hub, "Add a bundle")
	name := cards.Field{ID: "name", Label: "Bundle name", Check: func(typed string) string { return s.bundleName(typed, nil) },
		Help: "Its slash command is the name in lower case with dashes, such as web-research for /web-research."}
	ticked := map[string]bool{}
	return f.Ask("", []cards.Field{name}, "Next", func(values map[string]string) tea.Cmd {
		named := strings.TrimSpace(values["name"])
		return f.Show(s.skillTicks("Add a bundle", "The skills /"+slug(named)+" loads.", "Add /"+slug(named), ticked, func(names []string) tea.Cmd {
			if len(names) == 0 {
				return s.hub.Fail("Tick at least one skill for the bundle to load.")
			}
			return s.saveBundle(&skillsv1.SaveBundleRequest{Name: named, Skills: names}, "Added /"+slug(named)+".", func() tea.Cmd { return f.End() },
				func() {
					list.Select(slices.IndexFunc(s.bundled, func(b *skillsv1.Bundle) bool { return b.GetName() == named }))
				})
		}))
	})
}

// What is wrong with a name typed for a bundle, empty when it can be kept: none, one that makes no slash command, or one whose slash command
// another bundle than the one renamed has
func (s *screen) bundleName(typed string, renamed *skillsv1.Bundle) string {
	typed = strings.TrimSpace(typed)
	switch {
	case typed == "":
		return "Give the bundle a name."
	case slug(typed) == "":
		return "A name needs a letter or a digit, since its slash command is made of them."
	}
	for _, b := range s.bundled {
		if b.GetSlug() == slug(typed) && (renamed == nil || b.GetName() != renamed.GetName()) {
			return "The bundle " + b.GetName() + " has the slash command /" + b.GetSlug() + " already."
		}
	}
	return ""
}

// Slash command of a bundle name, as the skills make it
func slug(name string) string {
	return skills.Slug(strings.TrimSpace(name))
}

// Saves a bundle whole, then says what was done and runs then; once the bundles are read again, after puts what was made in hand
func (s *screen) saveBundle(request *skillsv1.SaveBundleRequest, told string, then func() tea.Cmd, after func()) tea.Cmd {
	request.Scope = s.scope()
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*skillsv1.SaveBundleResponse, error) {
		return s.service.SaveBundle(s.kit.Env.Ctx, request)
	}, func(*skillsv1.SaveBundleResponse) tea.Cmd {
		s.after = after
		cmds := []tea.Cmd{s.hub.Done(told), s.reload()}
		if then != nil {
			cmds = append(cmds, then())
		}
		return tea.Batch(cmds...)
	})
}

// A list that ticks skills under their categories, enter ticking the one in hand, its last row doing the act on what is ticked, as alt+n does
// from any row; on a page each tick may be kept at once instead, with no last row
func (s *screen) skillTicks(title, intro, act string, ticked map[string]bool, done func(names []string) tea.Cmd) *cards.List {
	t := s.kit.Env.Theme
	var shown []*skillsv1.Skill
	chosen := func() []string {
		var out []string
		for _, skill := range shown {
			if ticked[skill.GetName()] {
				out = append(out, skill.GetName())
			}
		}
		return out
	}
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Key:     Screen + ":ticks",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search skills",
		Intro:   func() []string { return []string{intro} },
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			shown = shown[:0]
			for _, category := range s.state.GetCategories() {
				shown = append(shown, s.skillsOf(category.GetName())...)
			}
			rows := make([][]string, 0, len(shown)+1)
			for _, skill := range shown {
				box := "[ ]"
				if ticked[skill.GetName()] {
					box = "[x]"
				}
				rows = append(rows, []string{box, skill.GetName(), ""})
			}
			if act == "" {
				return rows
			}
			return append(rows, []string{"", act, render.Dim(render.Count(len(chosen()), "skill")+" ticked", t)})
		},
		Group: func(i int) string {
			if i < len(shown) {
				return shown[i].GetCategoryTitle()
			}
			return ""
		},
		Words: func(i int) string {
			if i < len(shown) {
				return shown[i].GetDescription()
			}
			return ""
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []string{described(shown[i].GetDescription())}
		},
		Free: map[string]bool{"hub.add": act != ""},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			add := wm.Hint{Action: "hub.add", Label: "add"}
			switch {
			case i < 0:
				return nil
			case i >= len(shown):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			case act == "" && ticked[shown[i].GetName()]:
				return []wm.Hint{{Action: "hub.use", Label: "untick"}}
			case act == "":
				return []wm.Hint{{Action: "hub.use", Label: "tick"}}
			case ticked[shown[i].GetName()]:
				return []wm.Hint{{Action: "hub.use", Label: "untick"}, add}
			}
			return []wm.Hint{{Action: "hub.use", Label: "tick"}, add}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					return done(chosen())
				}
				ticked[shown[i].GetName()] = !ticked[shown[i].GetName()]
				l.Refresh()
				if act == "" {
					return done(chosen())
				}
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				if act == "" {
					return nil
				}
				return done(chosen())
			},
		},
	})
	list.ToggleDetail()
	return list
}

// Row of a bundle's page: one of its fields, or something done to it
type part struct {
	field string
	act   *act
}

// Opens the page of a bundle: what it loads as its first line, then its skills, description and instruction, renaming and removing it
func (s *screen) bundlePage(name string) tea.Cmd {
	return s.hub.Push(s.bundleView(name))
}

// The page of a bundle, to show over the bundles
func (s *screen) bundleView(name string) *cards.List {
	t := s.kit.Env.Theme
	at := &name
	title := name
	if b := s.bundle(name); b != nil {
		title = "/" + b.GetSlug()
	}
	parts := []part{{field: "skills"}, {field: "description"}, {field: "instruction"},
		{act: &act{title: "Rename", about: "Gives it another name, and so another slash command.", verb: "rename",
			run: func(l *cards.List) tea.Cmd { return s.renameBundle(l, at) }}},
		{act: &act{title: "Remove", about: "Takes the bundle and its slash command away. Its skills stay.", verb: "remove",
			run: func(l *cards.List) tea.Cmd { return s.removeBundle(l, s.bundle(*at)) }}}}
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":bundle:" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if b := s.bundle(*at); b != nil {
				return []string{"Type /" + b.GetSlug() + " in a chat to load its skills at once."}
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			b := s.bundle(*at)
			if b == nil {
				return nil
			}
			rows := make([][]string, len(parts))
			for i, p := range parts {
				if p.act != nil {
					rows[i] = []string{p.act.title, "", ""}
					continue
				}
				rows[i] = s.bundleCells(b, p.field)
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(parts) {
				return nil
			}
			if p := parts[i]; p.act != nil {
				return []string{p.act.about}
			}
			return []string{bundleAbout[parts[i].field]}
		},
		Empty: func() string { return "There is no bundle " + *at + " now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(parts) {
				return nil
			}
			if p := parts[i]; p.act != nil {
				return []wm.Hint{{Action: "hub.use", Label: p.act.verb}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if p := parts[i]; p.act != nil {
					return p.act.run(l)
				}
				return s.changeBundle(*at, parts[i].field)
			},
		},
	})
	view.ToggleDetail()
	s.views.Add(view)
	return view
}

// What each field of a bundle holds, under the rows
var bundleAbout = map[string]string{
	"skills":      "The skills its slash command loads.",
	"description": "What it is for, shown beside its slash command.",
	"instruction": "What the agent is told along with the skills.",
}

// Cells of a field of a bundle: its name and its value
func (s *screen) bundleCells(b *skillsv1.Bundle, field string) []string {
	t := s.kit.Env.Theme
	switch field {
	case "skills":
		return []string{"Skills", render.Dim(t.Glyphs.Forward, t), strs.And(b.GetSkills())}
	case "description":
		return []string{"Description", "", nonEmpty(b.GetDescription())}
	}
	return []string{"Instruction", "", nonEmpty(b.GetInstruction())}
}

// Text a row shows, Not set for none
func nonEmpty(text string) string {
	if strings.TrimSpace(text) == "" {
		return "Not set"
	}
	return oneLine(text)
}

// Changes a field of a bundle: its skills ticked, each tick kept at once; its description or instruction typed
func (s *screen) changeBundle(name, field string) tea.Cmd {
	b := s.bundle(name)
	if b == nil {
		return nil
	}
	keep := func(change func(r *skillsv1.SaveBundleRequest), told string, then func() tea.Cmd) tea.Cmd {
		now := s.bundle(name)
		request := &skillsv1.SaveBundleRequest{Name: now.GetName(), Skills: now.GetSkills(), Description: now.GetDescription(),
			Instruction: now.GetInstruction(), Overwrite: true}
		change(request)
		return s.saveBundle(request, told, then, nil)
	}
	switch field {
	case "skills":
		ticked := map[string]bool{}
		for _, skill := range b.GetSkills() {
			ticked[skill] = true
		}
		return s.hub.Push(s.skillTicks("Skills", "The skills /"+b.GetSlug()+" loads.", "", ticked, func(names []string) tea.Cmd {
			if len(names) == 0 {
				return s.hub.Fail("A bundle loads one skill at least. Remove it instead.")
			}
			return keep(func(r *skillsv1.SaveBundleRequest) { r.Skills = names }, "/"+b.GetSlug()+" now loads "+render.Count(len(names), "skill")+".", nil)
		}))
	case "description":
		input := cards.Field{ID: "text", Label: "Description", Value: b.GetDescription(), Select: true,
			Help: "What it is for, shown beside its slash command, such as Look things up on the web.\nLeave empty for none."}
		return s.typedField("Description", input, func(text string) tea.Cmd {
			return keep(func(r *skillsv1.SaveBundleRequest) { r.Description = text }, "Saved the description of /"+b.GetSlug()+".", nil)
		})
	}
	input := cards.Field{ID: "text", Label: "Instruction", Value: b.GetInstruction(), Select: true,
		Help: "What the agent is told along with the skills, such as Cite every source you use.\nLeave empty for none."}
	return s.typedField("Instruction", input, func(text string) tea.Cmd {
		return keep(func(r *skillsv1.SaveBundleRequest) { r.Instruction = text }, "Saved the instruction of /"+b.GetSlug()+".", nil)
	})
}

// Form of one typed field that may be left empty, closed once its answer is handed on
func (s *screen) typedField(title string, field cards.Field, done func(text string) tea.Cmd) tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{Title: title, Fields: []cards.Field{field}, Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tea.Batch(s.hub.Close(form), done(strings.TrimSpace(values[field.ID])))
		}})
	return s.hub.Push(form)
}

// Gives a bundle another name from its page: kept under the new name first, then the old one taken away, a name of the same slash command
// written over the bundle in its own file. The page is then made again under the new name, which its title is
func (s *screen) renameBundle(page *cards.List, at *string) tea.Cmd {
	b := s.bundle(*at)
	if b == nil {
		return nil
	}
	field := cards.Field{Label: "Bundle name", Value: b.GetName(), Select: true, Check: func(typed string) string { return s.bundleName(typed, b) },
		Help: "Its slash command is the name in lower case with dashes, such as web-research for /web-research."}
	return s.kit.Ask(s.hub, "Rename /"+b.GetSlug(), field, "Rename", func(name string) tea.Cmd {
		if name == b.GetName() {
			return nil
		}
		// A name of the same slash command is the same file, so it is written over and nothing is taken away
		same := slug(name) == b.GetSlug()
		s.hub.Busy("Saving…")
		return tui.Call(s.hub, func() (*skillsv1.SaveBundleResponse, error) {
			saved, err := s.service.SaveBundle(s.kit.Env.Ctx, &skillsv1.SaveBundleRequest{Scope: s.scope(), Name: name, Skills: b.GetSkills(),
				Description: b.GetDescription(), Instruction: b.GetInstruction(), Overwrite: same})
			if err != nil || same {
				return saved, err
			}
			_, err = s.service.DeleteBundle(s.kit.Env.Ctx, &skillsv1.DeleteBundleRequest{Scope: s.scope(), Name: b.GetName()})
			return saved, err
		}, func(saved *skillsv1.SaveBundleResponse) tea.Cmd {
			*at = saved.GetBundle().GetName()
			told := "/" + b.GetSlug() + " is now /" + saved.GetBundle().GetSlug() + "."
			if same {
				told = "/" + b.GetSlug() + " is now called " + saved.GetBundle().GetName() + "."
			}
			// The bundles are read again first, so the page made under the new name finds its bundle
			return tea.Batch(s.hub.Done(told), s.read(func() tea.Cmd {
				if s.hub.Top() != wm.Card(page) {
					return s.hub.Close(page)
				}
				return s.hub.Swap(s.bundleView(*at))
			}))
		})
	})
}

// Removes a bundle after the question, back on the bundles
func (s *screen) removeBundle(l *cards.List, b *skillsv1.Bundle) tea.Cmd {
	if b == nil {
		return nil
	}
	l.Confirm(cards.Question{Text: "Remove /" + b.GetSlug() + "? Its slash command goes away, and its skills stay.", Verb: "Remove", Deletes: true},
		func() tea.Cmd {
			return tui.Call(s.hub, func() (*skillsv1.DeleteBundleResponse, error) {
				return s.service.DeleteBundle(s.kit.Env.Ctx, &skillsv1.DeleteBundleRequest{Scope: s.scope(), Name: b.GetName()})
			}, func(*skillsv1.DeleteBundleResponse) tea.Cmd {
				cmds := []tea.Cmd{s.hub.Done("Removed /" + b.GetSlug() + "."), s.reload()}
				if top, ok := s.hub.Top().(interface{ Key() string }); ok && strings.HasPrefix(top.Key(), Screen+":bundle:") {
					cmds = append(cmds, s.hub.Pop())
				}
				return tea.Batch(cmds...)
			})
		})
	return nil
}
