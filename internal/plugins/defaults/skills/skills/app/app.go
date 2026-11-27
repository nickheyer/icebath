package app

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the skills
const Screen = "skills"

// Keymap context of the skills screen
const context = "skills"

// Surface of this app, whose own turned off skills ctrl+t sets
const surface = "tui"

// Column of a list of skills that holds what each one does, shown whole or not at all
const aboutColumn = 2

// Headings of the skills in no category, and of what the screen offers besides the skills
const (
	otherGroup = "Other"
	moreGroup  = "More"
)

// Views of the screen /skills opens on by name, besides the skills
var views = []string{"hub", "bundles", "sources", "trusted"}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := skillsv1connect.NewSkillsServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Group: tui.GroupAbilities,
		Order: 20,
		Title: "Skills",
		Short: "Turn skills on or off and install new ones",
		Slash: []string{"skills"},
		Usage: "[list|hub|bundles|sources|trusted|browse [page]|search <words>|inspect <name>|install <name or address>|<words to filter by>]",
		Where: map[string]string{context: "skills screen", review: "skill hub operation"},
		Actions: []keys.Action{
			{ID: "skills.surface", Context: context, Label: "toggle in this app", Doc: "Turn the highlighted skill on or off in this app only", Default: []string{"ctrl+t"}},
			{ID: "skills.search", Context: context, Label: "search", Doc: "Search the skill registries", Default: []string{"ctrl+f"}},
			{ID: "skills.go", Context: review, Label: "continue", Doc: "Continue what is waiting, such as installing a scanned skill", Default: []string{"enter"}},
		},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			s := &screen{kit: kit, service: service, settings: settings, waiting: map[string]bool{}}
			return s.slash(argument)
		},
	}}
}

type screen struct {
	kit      *tui.Kit
	service  skillsv1connect.SkillsServiceClient
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	root     *cards.List
	// Search, registry and page the skill hub opens at
	query  string
	source string
	page   int
	// Installs staged by this screen that wait for the user's yes
	waiting map[string]bool
	// Skills as last read
	state  *skillsv1.ListResponse
	loaded bool
	err    error
	rows   []row
}

// Row of the skills: a skill, or something else the screen offers
type row struct {
	skill *skillsv1.Skill
	act   *step
}

// Something a row does: its name, what it does and the word on its button
type step struct {
	title, about, verb string
	run                func(l *cards.List) tea.Cmd
}

// What /skills opens: a view, a verb of the hub, or the skills filtered by the text
func (s *screen) slash(argument string) tea.Cmd {
	word, rest := verb(argument)
	switch word {
	case "", "list":
		return s.open("", "")
	case "hub", "bundles", "sources", "trusted":
		return s.open(word, "")
	case "browse":
		n, ok := pageNumber(rest)
		if !ok {
			return tea.Batch(s.open("hub", ""), s.hub.Fail("The page must be a number from 1 up, such as /skills browse 2."))
		}
		s.page = n
		return s.open("hub", "")
	case "search":
		if rest == "" {
			return tea.Batch(s.open("hub", ""), s.hub.Fail("Say what to search for, such as /skills search pdf."))
		}
		s.query = rest
		return s.open("hub", "")
	case "install":
		identifier, name, category := installArgs(rest)
		if identifier == "" {
			return tea.Batch(s.open("hub", ""), s.hub.Fail("Name the skill to install, such as /skills install pdf-tools."))
		}
		return tea.Batch(s.open("hub", ""), s.install(identifier, name, category, func() tea.Cmd { return nil }))
	case "inspect":
		if rest == "" {
			return tea.Batch(s.open("", ""), s.hub.Fail("Name the skill to read, such as /skills inspect pdf-tools."))
		}
		return tea.Batch(s.open("", ""), s.inspect(rest))
	}
	return s.open("", strings.TrimSpace(argument))
}

// Scope of the screen: the profile in use, the folder of the session in view
func (s *screen) scope() *skillsv1.Scope {
	return &skillsv1.Scope{Directory: s.kit.Env.WorkDir()}
}

// Opens the skills, filtered by the search when given, with a view of the screen over them when one is named
func (s *screen) open(view, search string) tea.Cmd {
	s.root = s.skillsList()
	s.hub = s.kit.Hub(Screen, s.root)
	s.hub.OnClose(s.discardWaiting)
	if search != "" {
		s.root.Filter(search)
	}
	cmds := []tea.Cmd{s.kit.WM.ShowOverlay(s.hub)}
	if slices.Contains(views, view) {
		cmds = append(cmds, s.hub.Push(s.view(view)))
	}
	return tea.Batch(cmds...)
}

// A view of the screen besides the skills, as a list over them
func (s *screen) view(name string) *cards.List {
	var spec cards.ListSpec
	switch name {
	case "hub":
		spec = s.hubPart()
	case "bundles":
		spec = s.bundles()
	case "sources":
		spec = s.sources()
	default:
		spec = s.trusted()
	}
	spec.Glyph, spec.Key = s.kit.Env.Theme.Glyphs.Ext, Screen+":"+name
	if spec.Context == "" {
		spec.Context = cards.HubContext
	}
	spec.Primary = "hub.use"
	if spec.Free == nil {
		spec.Free = map[string]bool{}
	}
	spec.Free["hub.refresh"] = true
	list := cards.NewList(s.kit.Env, spec)
	list.ToggleDetail()
	return list
}

// Reads the skills again
func (s *screen) reload() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.List(s.kit.Env.Ctx, &skillsv1.ListRequest{Scope: s.scope()})
		return func() tea.Cmd {
			if err == nil {
				s.state = response
			}
			s.err, s.loaded = err, true
			if s.root != nil {
				s.root.Refresh()
			}
			return nil
		}
	})
}

// The installed skills under their categories, turned on or off with enter, then what else the screen offers
func (s *screen) skillsList() *cards.List {
	t := s.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Skills",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen,
		Context: context,
		// The row keys stay in force on every row, those the footer has no room for too
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search skills",
		Intro: func() []string {
			lines := []string{"Skills are instructions the agent loads for certain kinds of work. Turn off the ones you don't want it to use."}
			if project := s.state.GetUntrustedProject(); project != "" {
				lines = append(lines, project+" has "+render.Count(int(s.state.GetUntrustedCount()), "skill")+" of its own that load once you trust it.")
			}
			return lines
		},
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			s.rows = s.order()
			cells := make([][]string, len(s.rows))
			for i, r := range s.rows {
				if r.act != nil {
					cells[i] = []string{r.act.title, "", render.Dim(r.act.about, t)}
					continue
				}
				cells[i] = []string{r.skill.GetName(), s.stateOf(r.skill), render.Dim(r.skill.GetDescription(), t)}
			}
			return cells
		},
		Group: func(i int) string {
			switch r := s.rows[i]; {
			case r.act != nil:
				return moreGroup
			case r.skill.GetCategory() == "":
				return otherGroup
			}
			// A category is the name of a folder: said in words
			return strings.NewReplacer("-", " ", "_", " ").Replace(s.rows[i].skill.GetCategory())
		},
		Words: func(i int) string {
			if skill := s.rows[i].skill; skill != nil {
				return skill.GetCategory() + " " + skill.GetDescription()
			}
			return s.rows[i].act.about
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.rows) {
				return nil
			}
			if act := s.rows[i].act; act != nil {
				return []string{act.about + "."}
			}
			var lines []string
			if !list.Shows(aboutColumn) {
				lines = append(lines, s.rows[i].skill.GetDescription())
			}
			return append(lines, s.facts(s.rows[i].skill)...)
		},
		Empty: func() string { return tui.Waiting(s.loaded, s.err, "No skills are installed.") },
		Free:  map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			find := wm.Hint{Action: "hub.add", Label: "find skills"}
			if i < 0 || i >= len(s.rows) {
				return []wm.Hint{find}
			}
			if act := s.rows[i].act; act != nil {
				return []wm.Hint{{Action: "hub.use", Label: act.verb}, find}
			}
			skill := s.rows[i].skill
			var hints []wm.Hint
			switch {
			case skill.GetEssential():
				hints = append(hints, wm.Hint{Label: "always on"})
			case skill.GetEnabled():
				hints = append(hints, wm.Hint{Action: "hub.use", Label: "turn off"})
			default:
				hints = append(hints, wm.Hint{Action: "hub.use", Label: "turn on"})
			}
			if skill.GetEnabled() && !skill.GetEssential() {
				here := "off in this app"
				if slices.Contains(skill.GetDisabledOn(), surface) {
					here = "on in this app"
				}
				hints = append(hints, wm.Hint{Action: "skills.surface", Label: here})
			}
			hints = append(hints, wm.Hint{Action: "hub.edit", Label: "open"})
			if skill.GetSource() == "hub" {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "uninstall"})
			}
			return append(hints, find)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				if act := s.rows[i].act; act != nil {
					return act.run(l)
				}
				return s.toggle(s.rows[i].skill, "")
			},
			"skills.surface": s.withSkill(func(_ *cards.List, skill *skillsv1.Skill) tea.Cmd { return s.toggle(skill, surface) }),
			"hub.edit":       s.withSkill(func(_ *cards.List, skill *skillsv1.Skill) tea.Cmd { return s.skillView(skill) }),
			"hub.remove":     s.withSkill(func(l *cards.List, skill *skillsv1.Skill) tea.Cmd { return s.uninstall(l, skill) }),
			"hub.add":        func(*cards.List, int) tea.Cmd { return s.hub.Push(s.view("hub")) },
			"hub.refresh": func(*cards.List, int) tea.Cmd {
				s.hub.Busy("Reading the skills again…")
				return tui.Call(s.hub, func() (*skillsv1.ReloadResponse, error) {
					return s.service.Reload(s.kit.Env.Ctx, &skillsv1.ReloadRequest{Scope: s.scope()})
				}, func(response *skillsv1.ReloadResponse) tea.Cmd {
					return tea.Batch(s.hub.Done(reloaded(response)), s.reload())
				})
			},
		},
		OnFocus: func(*cards.List) tea.Cmd { return s.reload() },
	})
	list.ToggleDetail()
	return list
}

// Action on the skill of the row under the cursor, nothing on any other row
func (s *screen) withSkill(act func(l *cards.List, skill *skillsv1.Skill) tea.Cmd) func(*cards.List, int) tea.Cmd {
	return func(l *cards.List, i int) tea.Cmd {
		if i < 0 || i >= len(s.rows) || s.rows[i].skill == nil {
			return nil
		}
		return act(l, s.rows[i].skill)
	}
}

// Rows once the skills are read: the skills by category and name, those in none last, then what else the screen offers
func (s *screen) order() []row {
	if s.state == nil {
		return nil
	}
	skills := slices.Clone(s.state.GetSkills())
	slices.SortStableFunc(skills, func(a, b *skillsv1.Skill) int {
		switch ac, bc := a.GetCategory(), b.GetCategory(); {
		case ac == bc:
			return strings.Compare(a.GetName(), b.GetName())
		case ac == "":
			return 1
		case bc == "":
			return -1
		default:
			return strings.Compare(ac, bc)
		}
	})
	rows := make([]row, 0, len(skills)+6)
	for _, skill := range skills {
		rows = append(rows, row{skill: skill})
	}
	open := func(view string) func(*cards.List) tea.Cmd {
		return func(*cards.List) tea.Cmd { return s.hub.Push(s.view(view)) }
	}
	rows = append(rows, row{act: &step{"Find skills", "Browse and install skills from the skill hub", "open", open("hub")}},
		row{act: &step{"Bundles", "Several skills loaded at once under one slash command", "open", open("bundles")}},
		row{act: &step{"Sources", "GitHub repositories the skill hub searches", "open", open("sources")}},
		row{act: &step{"Trusted projects", "Project folders whose own skills load", "open", open("trusted")}})
	if project := s.state.GetUntrustedProject(); project != "" {
		rows = append(rows, row{act: &step{"Trust this project", "Load the " + render.Count(int(s.state.GetUntrustedCount()), "skill") + " of " + project,
			"trust", func(l *cards.List) tea.Cmd {
				l.Confirm("Trust "+project+"? Its skills then load for sessions working in it.", func() tea.Cmd {
					return s.trust(project, false, s.reload)
				})
				return nil
			}}})
	}
	return append(rows, row{act: &step{"Manage every skill", "Update, check and scan the skills you installed, and more", "open",
		func(*cards.List) tea.Cmd { return s.every() }}})
}

// Whether a skill is on, in words
func (s *screen) stateOf(skill *skillsv1.Skill) string {
	t := s.kit.Env.Theme
	switch {
	case skill.GetEssential():
		return "Always on"
	case !skill.GetEnabled():
		return render.Dim("Off", t)
	case slices.Contains(skill.GetDisabledOn(), surface):
		return render.Warm("Off in this app", t)
	}
	return "On"
}

// Where a skill comes from, in a sentence
func (s *screen) origin(skill *skillsv1.Skill) string {
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
		return "From an extra folder in settings."
	case "plugin":
		return "Comes with a plugin."
	}
	return "From " + skill.GetSource() + "."
}

// How far the skill hub trusts a skill, in words
var trustWords = map[string]string{"builtin": "Official", "trusted": "Trusted", "community": "Community"}

// Titles of the apps a skill can be turned off in alone, by name
func (s *screen) surfaces(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = name
		for _, known := range s.state.GetSurfaces() {
			if known.GetName() == name {
				out[i] = known.GetTitle()
			}
		}
	}
	return out
}

// Facts about a skill: where it comes from, whether it is on, its slash command, how it was used, what holds it and its file
func (s *screen) facts(skill *skillsv1.Skill) []string {
	t := s.kit.Env.Theme
	lines := []string{s.origin(skill)}
	switch {
	case skill.GetEssential():
		lines = append(lines, render.Good("Always on, because the agent's instructions point at it.", t))
	case !skill.GetEnabled():
		lines = append(lines, render.Dim("Off for the agent everywhere.", t))
	case len(skill.GetDisabledOn()) > 0:
		lines = append(lines, render.Warm("Off in "+strings.Join(s.surfaces(skill.GetDisabledOn()), ", ")+".", t))
	}
	if command := skill.GetCommand(); command != "" {
		lines = append(lines, "Slash command: /"+command)
	} else if note := skill.GetCommandNote(); note != "" {
		lines = append(lines, render.Warm(note, t))
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
		lines = append(lines, "Pinned, so it is never put away as unused.")
	}
	if skill.GetLocked() {
		lines = append(lines, "Locked, so the agent can't change it.")
	}
	switch skill.GetState() {
	case "stale":
		lines = append(lines, render.Warm("Not used for a long time.", t))
	case "archived":
		lines = append(lines, render.Warm("Put away as unused.", t))
	}
	return append(lines, render.Dim("File: "+skill.GetPath(), t))
}

// Turns a skill on or off for the agent everywhere, or in this app alone
func (s *screen) toggle(skill *skillsv1.Skill, where string) tea.Cmd {
	if skill.GetEssential() {
		return s.hub.Fail(skill.GetName() + " stays on, because the agent's instructions point at it.")
	}
	on, place := !skill.GetEnabled(), ""
	if where != "" {
		on, place = slices.Contains(skill.GetDisabledOn(), where), " in this app"
	}
	s.hub.Busy("Saving…")
	return tui.Call(s.hub, func() (*skillsv1.ToggleResponse, error) {
		return s.service.Toggle(s.kit.Env.Ctx, &skillsv1.ToggleRequest{Scope: s.scope(), Names: []string{skill.GetName()}, Enabled: on, Surface: where})
	}, func(*skillsv1.ToggleResponse) tea.Cmd {
		state := "off"
		if on {
			state = "on"
		}
		return tea.Batch(s.hub.Done(skill.GetName()+" is now "+state+place+"."), s.reload())
	})
}

// Skill as last read, the one given when it is gone
func (s *screen) current(skill *skillsv1.Skill) *skillsv1.Skill {
	for _, now := range s.state.GetSkills() {
		if now.GetName() == skill.GetName() {
			return now
		}
	}
	return skill
}

// One skill over the list: what it is and how it stands, then what can be done to it by where it comes from
func (s *screen) skillView(skill *skillsv1.Skill) tea.Cmd {
	t := s.kit.Env.Theme
	steps := func() []step {
		now := s.current(skill)
		out := []step{{"Read", "What it tells the agent, and its files", "read", func(*cards.List) tea.Cmd { return s.read(now.GetName()) }}}
		if !now.GetEssential() {
			toggle := step{"Turn off", "For the agent everywhere", "turn off", func(*cards.List) tea.Cmd { return s.toggle(now, "") }}
			if !now.GetEnabled() {
				toggle = step{"Turn on", "For the agent everywhere", "turn on", func(*cards.List) tea.Cmd { return s.toggle(now, "") }}
			}
			out = append(out, toggle)
			if now.GetEnabled() {
				here := step{"Turn off in this app", "Only here, while other apps keep it", "turn off", func(*cards.List) tea.Cmd { return s.toggle(now, surface) }}
				if slices.Contains(now.GetDisabledOn(), surface) {
					here = step{"Turn on in this app", "It is off here alone", "turn on", func(*cards.List) tea.Cmd { return s.toggle(now, surface) }}
				}
				out = append(out, here)
			}
		}
		switch now.GetSource() {
		case "local", "create_dir":
			out = append(out, step{"Publish", "Share it as a pull request to a repository of skills", "publish", func(*cards.List) tea.Cmd { return s.publish(now) }})
		case "hub":
			out = append(out, step{"Update", "Get its newest version", "update", func(*cards.List) tea.Cmd { return s.update(now.GetName(), false, s.reload) }},
				step{"Update over your changes", "Get its newest version, replacing changes you made to it", "update",
					func(*cards.List) tea.Cmd { return s.update(now.GetName(), true, s.reload) }})
			if now.GetTrust() == "builtin" {
				out = append(out, step{"Restore", "Put the official copy back, keeping yours aside", "restore", func(*cards.List) tea.Cmd { return s.restore(now) }})
			}
			out = append(out, step{"Uninstall", "Delete it", "uninstall", func(l *cards.List) tea.Cmd { return s.uninstall(l, now) }})
		case "builtin":
			out = append(out, step{"See changes", "How your copy differs from the one icebath ships", "open", func(*cards.List) tea.Cmd { return s.diff(now.GetName()) }},
				step{"Reset", "Take updates again, or put the copy icebath ships back", "reset", func(*cards.List) tea.Cmd { return s.reset(now) }})
		}
		return out
	}
	var shown []step
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   skill.GetName(),
		Glyph:   t.Glyphs.Ext,
		Key:     Screen + ":skill:" + skill.GetName(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			now := s.current(skill)
			return append([]string{now.GetDescription()}, s.facts(now)...)
		},
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			shown = steps()
			cells := make([][]string, len(shown))
			for i, st := range shown {
				cells[i] = []string{st.title, render.Dim(st.about, t)}
			}
			return cells
		},
		Detail: func(i int) []string {
			if list.Shows(1) || i < 0 || i >= len(shown) {
				return nil
			}
			return []string{shown[i].about + "."}
		},
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
	list.ToggleDetail()
	return s.hub.Push(list)
}

// A page of the skill as the agent loads it: its content, what it still needs, its files and how it is written
func (s *screen) read(name string) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.ShowResponse, error) {
		return s.service.Show(s.kit.Env.Ctx, &skillsv1.ShowRequest{Scope: s.scope(), Name: name})
	}, func(response *skillsv1.ShowResponse) tea.Cmd { return s.readPage(name, response) })
}

// The page of a skill as the agent loads it
func (s *screen) readPage(name string, response *skillsv1.ShowResponse) tea.Cmd {
	t := s.kit.Env.Theme
	page := cards.NewPage(s.kit.Env, cards.PageSpec{Title: name, Key: Screen + ":read", Lines: func(int) []string {
		var lines []string
		if note := response.GetSetupNote(); note != "" {
			lines = append(lines, render.Warm(note, t), "")
		}
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

// Lets a built in skill take updates again, or puts the shipped copy back
func (s *screen) reset(skill *skillsv1.Skill) tea.Cmd {
	choices := []tui.Choice{
		{ID: "follow", Title: "Take updates again", Detail: "Your copy stays until the next update of " + skill.GetName() + " replaces it"},
		{ID: "restore", Title: "Put the shipped copy back", Detail: "Deletes your copy now and copies the one icebath ships"},
	}
	return s.kit.Pick(s.hub, "Reset "+skill.GetName(), "reset", choices, "", func(choice tui.Choice) tea.Cmd {
		s.hub.Busy("Resetting " + skill.GetName() + "…")
		return tui.Call(s.hub, func() (*skillsv1.ResetResponse, error) {
			return s.service.Reset(s.kit.Env.Ctx, &skillsv1.ResetRequest{Scope: s.scope(), Name: skill.GetName(), Restore: choice.ID == "restore"})
		}, func(response *skillsv1.ResetResponse) tea.Cmd {
			return tea.Batch(s.hub.Done(response.GetMessage()), s.reload())
		})
	})
}

// Puts the official copy of a skill from the hub back, the user's copy kept aside
func (s *screen) restore(skill *skillsv1.Skill) tea.Cmd {
	s.hub.Busy("Restoring " + skill.GetName() + "…")
	return tui.Call(s.hub, func() (*skillsv1.RepairResponse, error) {
		return s.service.Repair(s.kit.Env.Ctx, &skillsv1.RepairRequest{Scope: s.scope(), Name: skill.GetName(), Restore: true})
	}, func(response *skillsv1.RepairResponse) tea.Cmd {
		note := "Put the official copy of " + skill.GetName() + " back."
		if backed := response.GetBackedUp(); len(backed) > 0 {
			note += " Your copy of " + strings.Join(backed, ", ") + " is kept in " + response.GetBackupDir() + "."
		}
		return tea.Batch(s.hub.Done(note), s.reload())
	})
}

// What a reload changed, in a sentence or two
func reloaded(response *skillsv1.ReloadResponse) string {
	if len(response.GetAdded())+len(response.GetRemoved()) == 0 {
		return "Read the skills again. No slash command came or went."
	}
	var parts []string
	if added := response.GetAdded(); len(added) > 0 {
		names := make([]string, len(added))
		for i, c := range added {
			names[i] = "/" + c.GetName()
		}
		parts = append(parts, "New: "+strings.Join(names, ", ")+".")
	}
	if removed := response.GetRemoved(); len(removed) > 0 {
		names := make([]string, len(removed))
		for i, c := range removed {
			names[i] = "/" + c.GetName()
		}
		parts = append(parts, "Gone: "+strings.Join(names, ", ")+".")
	}
	return "Read the skills again. " + strings.Join(parts, " ")
}

// Bundles: several skills loaded under one slash command, made, changed and removed here
func (s *screen) bundles() cards.ListSpec {
	var (
		state  *skillsv1.BundlesResponse
		loaded bool
		failed error
		list   *cards.List
	)
	rows := func() []*skillsv1.Bundle { return state.GetBundles() }
	reload := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := s.service.Bundles(s.kit.Env.Ctx, &skillsv1.BundlesRequest{Scope: s.scope()})
			return func() tea.Cmd {
				if err == nil {
					state = response
				}
				failed, loaded = err, true
				if list != nil {
					list.Refresh()
				}
				return nil
			}
		})
	}
	t := s.kit.Env.Theme
	return cards.ListSpec{
		Title:   "Bundles",
		Search:  "Search bundles",
		Intro:   func() []string { return []string{"A bundle loads several skills at once under one slash command."} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			out := make([][]string, len(rows()))
			for i, b := range rows() {
				out[i] = []string{"/" + b.GetSlug(), render.Count(len(b.GetSkills()), "skill"), render.Dim(b.GetDescription(), t)}
			}
			return out
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(rows()) {
				return nil
			}
			b := rows()[i]
			var lines []string
			if !list.Shows(aboutColumn) && b.GetDescription() != "" {
				lines = append(lines, b.GetDescription())
			}
			lines = append(lines, "Loads "+strings.Join(b.GetSkills(), ", ")+".")
			if b.GetInstruction() != "" {
				lines = append(lines, "Tells the agent: "+b.GetInstruction())
			}
			return append(lines, render.Dim("File: "+b.GetPath(), t))
		},
		Empty: func() string {
			return tui.Waiting(loaded, failed, "No bundles yet. "+s.kit.Env.Keys.Display("hub.add")+" makes one.")
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			add := wm.Hint{Action: "hub.add", Label: "add"}
			if i < 0 {
				return []wm.Hint{add}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, add, {Action: "hub.remove", Label: "remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd { return s.editBundle(nil, reload) },
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return s.editBundle(rows()[i], reload) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				b := rows()[i]
				l.Confirm("Remove /"+b.GetSlug()+"? The slash command goes away, and the skills stay.", func() tea.Cmd {
					return tui.Call(s.hub, func() (*skillsv1.DeleteBundleResponse, error) {
						return s.service.DeleteBundle(s.kit.Env.Ctx, &skillsv1.DeleteBundleRequest{Scope: s.scope(), Name: b.GetName()})
					}, func(*skillsv1.DeleteBundleResponse) tea.Cmd {
						return tea.Batch(s.hub.Done("Removed /"+b.GetSlug()+"."), reload())
					})
				})
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			return reload()
		},
	}
}

// Form that makes a bundle, or changes the one given
func (s *screen) editBundle(b *skillsv1.Bundle, reload func() tea.Cmd) tea.Cmd {
	title, submit := "New bundle", "Add"
	if b != nil {
		title, submit = "/"+b.GetSlug(), "Save"
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: title,
		Intro: []string{"A bundle loads several skills at once under one slash command."},
		Fields: []cards.Field{
			{ID: "name", Label: "Name", Value: b.GetName(), Required: true, Help: "Its slash command is the name in lower case, with hyphens for spaces."},
			{ID: "skills", Label: "Skills", Value: strings.Join(b.GetSkills(), ", "), Required: true, Help: "The skills it loads, separated by commas."},
			{ID: "description", Label: "Description", Value: b.GetDescription(), Help: "What it is for, shown beside its slash command."},
			{ID: "instruction", Label: "Instruction", Value: b.GetInstruction(), Help: "What the agent is told along with the skills."},
		},
		Submit: submit,
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			var names []string
			for _, name := range strings.Split(values["skills"], ",") {
				if name = strings.TrimSpace(name); name != "" {
					names = append(names, name)
				}
			}
			s.hub.Busy("Saving…")
			return tui.Call(s.hub, func() (*skillsv1.SaveBundleResponse, error) {
				if b != nil && !strings.EqualFold(strings.TrimSpace(values["name"]), b.GetName()) {
					if _, err := s.service.DeleteBundle(s.kit.Env.Ctx, &skillsv1.DeleteBundleRequest{Scope: s.scope(), Name: b.GetName()}); err != nil {
						return nil, err
					}
				}
				return s.service.SaveBundle(s.kit.Env.Ctx, &skillsv1.SaveBundleRequest{
					Scope: s.scope(), Name: values["name"], Skills: names, Description: strings.TrimSpace(values["description"]),
					Instruction: strings.TrimSpace(values["instruction"]), Overwrite: b != nil,
				})
			}, func(response *skillsv1.SaveBundleResponse) tea.Cmd {
				told := "Added /" + response.GetBundle().GetSlug() + "."
				if b != nil {
					told = "Saved /" + response.GetBundle().GetSlug() + "."
				}
				return tea.Batch(s.hub.Close(form), s.hub.Done(told), reload())
			})
		},
	})
	return s.hub.Push(form)
}

// Trusted projects: folders whose own skills load, the session's folder offered first
func (s *screen) trusted() cards.ListSpec {
	var (
		trusted []string
		loaded  bool
		failed  error
		list    *cards.List
	)
	reload := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			var folders []string
			response, err := s.settings.Get(s.kit.Env.Ctx, &configv1.GetRequest{Key: skillsconfig.TrustedProjectDirs})
			if err == nil {
				err = yaml.Unmarshal([]byte(response.GetSetting().GetValue()), &folders)
			}
			return func() tea.Cmd {
				trusted, failed, loaded = folders, err, true
				if list != nil {
					list.Refresh()
				}
				return nil
			}
		})
	}
	return cards.ListSpec{
		Title: "Trusted projects",
		Intro: func() []string {
			return []string{"The skills of a trusted project load for sessions working in it. They sit in its .icebath/skills or .agents/skills folder."}
		},
		Columns: []render.Column{{Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(trusted))
			for i, folder := range trusted {
				out[i] = []string{folder}
			}
			return out
		},
		Empty: func() string {
			return tui.Waiting(loaded, failed, "No trusted projects yet. "+s.kit.Env.Keys.Display("hub.add")+" trusts one.")
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			add := wm.Hint{Action: "hub.add", Label: "trust"}
			if i < 0 {
				return []wm.Hint{add}
			}
			return []wm.Hint{{Action: "hub.remove", Label: "stop trusting"}, add}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				return s.kit.Ask(s.hub, "Trust a project", cards.Field{Label: "Folder", Value: s.kit.Env.WorkDir(),
					Help: "The folder of the project. Its skills load for sessions working in it."}, "Trust", func(folder string) tea.Cmd {
					return s.trust(folder, false, reload)
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				folder := trusted[i]
				l.Confirm("Stop trusting "+folder+"? Its skills no longer load.", func() tea.Cmd { return s.trust(folder, true, reload) })
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			return reload()
		},
	}
}
