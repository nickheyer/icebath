package app

import (
	"fmt"
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

// Opens the page of a category over the page of the area: how many of its skills are on as its first line, then one switch per skill,
// enter opening the skill's page and t turning it on or off
func (s *screen) categoryPage(name string) tea.Cmd {
	t := s.kit.Env.Theme
	title := name
	if category := s.category(name); category != nil {
		title = category.GetTitle()
	}
	var shown []*skillsv1.Skill
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":category:" + name,
		Context: context,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Intro: func() []string {
			if category := s.category(name); category != nil {
				return []string{categorySentence(category)}
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = s.skillsOf(name)
			cells := make([][]string, len(shown))
			for i, skill := range shown {
				cells[i] = []string{skill.GetName(), "", skillState(skill.GetShown(), t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []string{described(shown[i].GetDescription())}
		},
		Empty: func() string { return "No skill is in " + title + " now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return s.switchHints(shown[i])
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return s.skillPage(shown[i].GetName()) },
			"skills.turn": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(shown) {
					return nil
				}
				return s.turn(shown[i])
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(shown) || shown[i].GetSource() != "hub" {
					return nil
				}
				return s.uninstall(l, shown[i])
			},
		},
	})
	view.ToggleDetail()
	s.views.Add(view)
	return s.hub.Push(view)
}

// How many skills of a category are on, as a sentence
func categorySentence(category *skillsv1.Category) string {
	switch on, total := category.GetOn(), category.GetTotal(); {
	case total == 1 && on == 1:
		return "Its one skill is on."
	case total == 1:
		return "Its one skill is off."
	case on == total:
		return fmt.Sprintf("All %d skills are on.", total)
	case on == 0:
		return fmt.Sprintf("All %d skills are off.", total)
	default:
		return fmt.Sprintf("%d of %d skills are on.", on, total)
	}
}

// Buttons of a skill's row: open, then its switch, or what keeps it on, then uninstall for one from the hub
func (s *screen) switchHints(skill *skillsv1.Skill) []wm.Hint {
	hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
	switch {
	case skill.GetEssential():
		hints = append(hints, wm.Hint{Label: "always on"})
	case skill.GetEnabled() && !slices.Contains(skill.GetDisabledOn(), surface):
		hints = append(hints, wm.Hint{Action: "skills.turn", Label: "turn off"})
	default:
		hints = append(hints, wm.Hint{Action: "skills.turn", Label: "turn on"})
	}
	if skill.GetSource() == "hub" {
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "uninstall"})
	}
	return hints
}

// The switch of a skill's row, which does what the state the row shows asks for: a skill that is off in this app alone comes back on here,
// and any other goes on or off in every app
func (s *screen) turn(skill *skillsv1.Skill) tea.Cmd {
	switch {
	case skill.GetEssential():
		return s.hub.Fail(skill.GetName() + " is always on, because the agent's instructions point at it.")
	case skill.GetEnabled() && slices.Contains(skill.GetDisabledOn(), surface):
		return s.toggle(skill, surface)
	}
	return s.toggle(skill, "")
}

// Turns a skill on or off for every app, or for this app alone, through the one function that does it, saying what it says
func (s *screen) toggle(skill *skillsv1.Skill, where string) tea.Cmd {
	on := !skill.GetEnabled()
	if where != "" {
		on = slices.Contains(skill.GetDisabledOn(), where)
	}
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*skillsv1.ToggleResponse, error) {
		return s.service.Toggle(s.kit.Env.Ctx, &skillsv1.ToggleRequest{Scope: s.scope(), Names: []string{skill.GetName()}, Enabled: on, Surface: where})
	}, func(response *skillsv1.ToggleResponse) tea.Cmd {
		return tea.Batch(s.hub.Done(response.GetSaid()), s.reload(), s.options.Load(nil))
	})
}

// Row of a skill's page: something done to it
type deed struct {
	title, about, verb string
	opens              bool
	run                func(l *cards.List) tea.Cmd
}

// Opens the page of a skill: its state as a sentence and where it comes from, then turning it on or off, reading it, turning it off in this
// app alone, and what its source lets be done to it: seeing and resetting your changes, updating, restoring, publishing and uninstalling
func (s *screen) skillPage(name string) tea.Cmd {
	t := s.kit.Env.Theme
	var shown []deed
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   name,
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":skill:" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if skill := s.skill(name); skill != nil {
				return s.skillState(skill)
			}
			return nil
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = s.deeds(name)
			cells := make([][]string, len(shown))
			for i, d := range shown {
				mark := ""
				if d.opens {
					mark = render.Dim(t.Glyphs.Forward, t)
				}
				cells[i] = []string{d.title, mark, ""}
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []string{shown[i].about}
		},
		Empty: func() string { return "There is no skill " + name + " now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: shown[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd { return shown[i].run(l) },
		},
	})
	view.ToggleDetail()
	s.views.Add(view)
	return s.hub.Push(view)
}

// The state of a skill as a sentence, then where it comes from, its slash command, how it was used and what holds it
func (s *screen) skillState(skill *skillsv1.Skill) []string {
	var said string
	switch skill.GetShown() {
	case "Always on":
		said = "Always on, because the agent's instructions point at it."
	case "Off":
		said = "Off, so the agent is never offered it."
	case "Off in this app":
		said = "Off in this app. Other apps still offer it to the agent."
	default:
		said = "On."
		if away := s.offIn(skill); away != "" {
			said = "On here, and off in the " + away + "."
		}
	}
	return append([]string{said}, s.facts(skill)...)
}

// The apps besides this one a skill is turned off in alone, by what a person calls them
func (s *screen) offIn(skill *skillsv1.Skill) string {
	var titles []string
	for _, known := range s.state.GetSurfaces() {
		if known.GetName() != surface && slices.Contains(skill.GetDisabledOn(), known.GetName()) {
			titles = append(titles, strings.ToLower(known.GetTitle()))
		}
	}
	return strs.And(titles)
}

// How far the skill hub trusts a skill, in words
var trustWords = map[string]string{"builtin": "Official", "trusted": "Trusted", "community": "Community"}

// Where a skill comes from, in a sentence
func origin(skill *skillsv1.Skill) string {
	switch skill.GetSource() {
	case "builtin":
		if skill.GetModified() {
			return "Comes with icebath. You changed it, so updates leave it alone."
		}
		return "Comes with icebath."
	case "hub":
		if trust := trustWords[skill.GetTrust()]; trust != "" {
			return "Installed from the skill hub. " + trust + "."
		}
		return "Installed from the skill hub."
	case "local":
		return "Your own skill."
	case "project":
		return "Comes with this project."
	case "create_dir":
		return "Made in your folder of new skills."
	case "external":
		return "From one of your extra folders."
	case "plugin":
		return "Comes with a plugin."
	}
	return "From " + skill.GetSource() + "."
}

// Facts of a skill under its state: where it comes from, its slash command, how it was used, what holds it, and its file
func (s *screen) facts(skill *skillsv1.Skill) []string {
	t := s.kit.Env.Theme
	lines := []string{render.Dim(origin(skill), t)}
	if command := skill.GetCommand(); command != "" {
		lines = append(lines, render.Dim("Its slash command is /"+command+".", t))
	} else if note := skill.GetCommandNote(); note != "" {
		lines = append(lines, render.Warm(note+".", t))
	}
	if skill.GetUseCount()+skill.GetViewCount()+skill.GetPatchCount() > 0 {
		usage := fmt.Sprintf("Used %s, read %s, changed %s.", render.Count(int(skill.GetUseCount()), "time"), render.Count(int(skill.GetViewCount()), "time"),
			render.Count(int(skill.GetPatchCount()), "time"))
		if last := skill.GetLastActivity(); last != "" {
			usage += " Last on " + last + "."
		}
		lines = append(lines, render.Dim(usage, t))
	}
	if skill.GetPinned() {
		lines = append(lines, render.Dim("Pinned, so it is never put away as unused.", t))
	}
	if skill.GetLocked() {
		lines = append(lines, render.Dim("Locked, so the agent can't change it.", t))
	}
	switch skill.GetState() {
	case "stale":
		lines = append(lines, render.Warm("Not used for a long time.", t))
	case "archived":
		lines = append(lines, render.Warm("Put away as unused.", t))
	}
	return append(lines, render.Dim("Its file is "+skill.GetPath()+".", t))
}

// What can be done to a skill as it stands now
func (s *screen) deeds(name string) []deed {
	skill := s.skill(name)
	if skill == nil {
		return nil
	}
	var out []deed
	if !skill.GetEssential() {
		turn := deed{"Turn off", "The agent is never offered it, in any app.", "turn off", false, func(*cards.List) tea.Cmd { return s.toggle(s.skill(name), "") }}
		if !skill.GetEnabled() {
			turn = deed{"Turn on", "The agent is offered it again.", "turn on", false, func(*cards.List) tea.Cmd { return s.toggle(s.skill(name), "") }}
		}
		out = append(out, turn)
	}
	out = append(out, deed{"Read it", "What it tells the agent, and its files.", "read", true, func(*cards.List) tea.Cmd { return s.readSkill(name) }})
	if skill.GetEnabled() && !skill.GetEssential() {
		here := deed{"Turn off in this app", "Only this app stops offering it. Other apps keep it.", "turn off", false,
			func(*cards.List) tea.Cmd { return s.toggle(s.skill(name), surface) }}
		if slices.Contains(skill.GetDisabledOn(), surface) {
			here = deed{"Turn on in this app", "This app offers it again.", "turn on", false, func(*cards.List) tea.Cmd { return s.toggle(s.skill(name), surface) }}
		}
		out = append(out, here)
	}
	switch skill.GetSource() {
	case "builtin":
		if skill.GetModified() {
			out = append(out, deed{"See changes", "How your copy differs from the one icebath comes with.", "open", true,
				func(*cards.List) tea.Cmd { return s.diff(name) }})
		}
		out = append(out, deed{"Reset", "Take updates again, or put back the copy icebath comes with.", "reset", true,
			func(l *cards.List) tea.Cmd { return s.reset(l, name) }})
	case "local", "create_dir":
		out = append(out, deed{"Publish", "Share it with others, on GitHub or ClawHub.", "publish", true,
			func(*cards.List) tea.Cmd { return s.publish(s.skill(name)) }})
	case "hub":
		out = append(out, deed{"Update", "Gets its newest version, keeping it as it is if you changed it.", "update", false,
			func(*cards.List) tea.Cmd { return s.update(name, false, s.reload) }},
			deed{"Update over your changes", "Gets its newest version, replacing changes you made to it.", "update", false,
				func(*cards.List) tea.Cmd { return s.update(name, true, s.reload) }})
		if skill.GetTrust() == "builtin" {
			out = append(out, deed{"Restore", "Puts the official copy back, keeping yours aside.", "restore", false,
				func(*cards.List) tea.Cmd { return s.restore(name) }})
		}
		out = append(out, deed{"Uninstall", "Deletes its files.", "uninstall", false, func(l *cards.List) tea.Cmd { return s.uninstall(l, s.skill(name)) }})
	}
	return out
}

// A page of the skill as the agent loads it: what it is for, what it still needs, its tags, files and task, how it is written, and its
// SKILL.md
func (s *screen) readSkill(name string) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.ShowResponse, error) {
		return s.service.Show(s.kit.Env.Ctx, &skillsv1.ShowRequest{Scope: s.scope(), Name: name})
	}, func(response *skillsv1.ShowResponse) tea.Cmd { return s.readPage(name, response) })
}

// The page of a skill as the agent loads it
func (s *screen) readPage(name string, response *skillsv1.ShowResponse) tea.Cmd {
	t := s.kit.Env.Theme
	page := cards.NewPage(s.kit.Env, cards.PageSpec{Title: "Read " + name, Key: Screen + ":read", Lines: func(int) []string {
		lines := []string{described(response.GetSkill().GetDescription())}
		if note := response.GetSetupNote(); note != "" {
			lines = append(lines, render.Warm(note, t))
		}
		// One that can't work here says so, which no setup changes
		if response.GetReadiness() == skills.Unsupported {
			lines = append(lines, render.Warm("This skill does not work on this computer, so the agent can't load it here.", t))
		}
		lines = append(lines, "")
		if tags := response.GetTags(); len(tags) > 0 {
			lines = append(lines, render.Dim("Tags: "+strings.Join(tags, ", "), t))
		}
		if related := response.GetRelated(); len(related) > 0 {
			lines = append(lines, render.Dim("Related skills: "+strings.Join(related, ", "), t))
		}
		if files := response.GetFiles(); len(files) > 0 {
			lines = append(lines, render.Dim("Files: "+strings.Join(files, ", "), t))
		}
		if b := response.GetBlueprint(); b != nil {
			line := "Sets up a task that runs on the schedule " + b.GetSchedule()
			if b.GetDeliver() != "origin" {
				line += " and sends its result to " + b.GetDeliver()
			}
			if b.GetNoAgent() {
				line += ", without the agent"
			}
			lines = append(lines, render.Cool(line+".", t))
		}
		if problem := response.GetBlueprintError(); problem != "" {
			lines = append(lines, render.Bad("The task it sets up can't be read: "+problem, t))
		}
		if findings := response.GetFindings(); len(findings) > 0 {
			lines = append(lines, "", render.Heading("How it is written", t))
			for _, f := range findings {
				lines = append(lines, render.Dim(f.GetSeverity()+" · "+f.GetRule()+": ", t)+f.GetMessage())
			}
		}
		lines = append(lines, "", render.Heading("SKILL.md", t))
		return append(lines, strings.Split(response.GetContent(), "\n")...)
	}})
	return s.hub.Push(page)
}

// Lets a built-in skill take updates again, or puts the copy icebath comes with back after the question, picked from the two on the skill's
// page l
func (s *screen) reset(l *cards.List, name string) tea.Cmd {
	choices := []tui.Choice{
		{ID: "follow", Title: "Take updates again", Detail: "Your copy stays until the next update of " + name + " replaces it", Verb: "Reset"},
		{ID: "restore", Title: "Put the copy icebath comes with back", Detail: "Deletes your copy, after a question", Verb: "Put back"},
	}
	run := func(restore bool) tea.Cmd {
		s.hub.Busy("Resetting " + name + "…")
		return tui.Call(s.hub, func() (*skillsv1.ResetResponse, error) {
			return s.service.Reset(s.kit.Env.Ctx, &skillsv1.ResetRequest{Scope: s.scope(), Name: name, Restore: restore})
		}, func(response *skillsv1.ResetResponse) tea.Cmd {
			return tea.Batch(s.hub.Done(response.GetMessage()), s.reload())
		})
	}
	return s.kit.PickFrom(s.hub, "Reset "+name, []string{"What to do with your copy of " + name + "."}, "reset", choices, "", func(choice tui.Choice) tea.Cmd {
		if choice.ID != "restore" {
			return run(false)
		}
		l.Confirm(cards.Question{Text: "Put back the copy of " + name + " icebath comes with? Your copy and the changes in it are deleted.",
			Verb: "Put back", Deletes: true}, func() tea.Cmd { return run(true) })
		return nil
	})
}

// Puts the official copy of a skill from the hub back, the person's copy kept aside
func (s *screen) restore(name string) tea.Cmd {
	s.hub.Busy("Restoring " + name + "…")
	return tui.Call(s.hub, func() (*skillsv1.RepairResponse, error) {
		return s.service.Repair(s.kit.Env.Ctx, &skillsv1.RepairRequest{Scope: s.scope(), Name: name, Restore: true})
	}, func(response *skillsv1.RepairResponse) tea.Cmd {
		note := "Put the official copy of " + name + " back."
		if backed := response.GetBackedUp(); len(backed) > 0 {
			note += " Your copy is kept in " + response.GetBackupDir() + "."
		}
		return tea.Batch(s.hub.Done(note), s.reload())
	})
}

// Uninstalls a skill the hub installed, after the question
func (s *screen) uninstall(l *cards.List, skill *skillsv1.Skill) tea.Cmd {
	if skill == nil {
		return nil
	}
	l.Confirm(cards.Question{Text: "Uninstall " + skill.GetName() + "? Its files are deleted, and the agent is no longer offered it.", Verb: "Uninstall",
		Deletes: true}, func() tea.Cmd {
		s.hub.Busy("Uninstalling " + skill.GetName() + "…")
		return tui.Call(s.hub, func() (*skillsv1.UninstallResponse, error) {
			return s.service.Uninstall(s.kit.Env.Ctx, &skillsv1.UninstallRequest{Scope: s.scope(), Name: skill.GetName()})
		}, func(response *skillsv1.UninstallResponse) tea.Cmd {
			if !response.GetOk() {
				return s.hub.Fail(response.GetMessage())
			}
			// The page of a skill that is gone goes with it; a list it was uninstalled from stays
			var back tea.Cmd
			if strings.HasPrefix(l.Key(), Screen+":skill:") && s.hub.Holds(l) {
				back = s.hub.Close(l)
			}
			return tea.Batch(back, s.hub.Done(response.GetMessage()), s.reload())
		})
	})
	return nil
}
