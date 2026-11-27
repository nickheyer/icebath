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

// Surface of this app, whose own disabled skills ctrl+t sets
const surface = "tui"

// Parts of the screen in tab order
var parts = []string{"skills", "hub", "bundles", "sources", "trusted"}

var titles = map[string]string{"skills": "Skills", "hub": "Skill hub", "bundles": "Bundles", "sources": "Sources", "trusted": "Trusted projects"}

type screens struct{}

func Register(registry *app.Registry) {
	registry.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := skillsv1connect.NewSkillsServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    Screen,
		Title: "Skills",
		Short: "Turn skills on or off, see what each does, install skills from the hub, and manage bundles, sources and trusted projects",
		Slash: []string{"skills"},
		Usage: "[list|hub|bundles|sources|trusted|browse [page]|search <query>|inspect <name>|install <name or url>|<filter>]",
		Where: map[string]string{context: "skills screen", review: "skill hub operation"},
		Actions: []keys.Action{
			{ID: "skills.next", Context: context, Label: "next part", Doc: "show the next part: skills, skill hub, bundles, sources, trusted projects", Default: []string{"tab"}},
			{ID: "skills.prev", Context: context, Label: "previous part", Doc: "show the previous part of the screen", Default: []string{"shift+tab"}, Quiet: true},
			{ID: "skills.surface", Context: context, Label: "here only", Doc: "turn the selected skill on or off in the terminal app alone", Default: []string{"ctrl+t"}},
			{ID: "skills.more", Context: context, Label: "more", Doc: "what can be done with the selected skill: read, update, uninstall, publish, see changes", Default: []string{"o"}},
			{ID: "skills.update", Context: context, Label: "all skills", Doc: "commands over every skill: update, check, audit, save or install a list; on the skill hub, the next page", Default: []string{"u"}},
			{ID: "skills.search", Context: context, Label: "search", Doc: "search the skill registries", Default: []string{"ctrl+f"}},
			{ID: "skills.go", Context: review, Label: "go on", Doc: "go on with what the operation waits for, such as installing a scanned skill", Default: []string{"enter"}},
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
	// Search and page the skill hub part opens at
	query string
	page  int
	// Installs staged by this screen that wait for the user's yes
	waiting map[string]bool
}

// What /skills opens: a part, a verb of the hub, or the skills filtered by the text
func (s *screen) slash(argument string) tea.Cmd {
	word, rest := verb(argument)
	switch word {
	case "", "list":
		return s.open("skills", "")
	case "hub", "bundles", "sources", "trusted":
		return s.open(word, "")
	case "browse":
		n, ok := pageNumber(rest)
		if !ok {
			return tea.Batch(s.open("hub", ""), s.hub.Fail("usage: /skills browse [page]  (page must be a positive number)"))
		}
		s.page = n
		return s.open("hub", "")
	case "search":
		if rest == "" {
			return tea.Batch(s.open("hub", ""), s.hub.Fail("usage: /skills search <query>"))
		}
		s.query = rest
		return s.open("hub", "")
	case "install":
		identifier, name, category := installArgs(rest)
		if identifier == "" {
			return tea.Batch(s.open("hub", ""), s.hub.Fail("usage: /skills install <name or url>"))
		}
		return tea.Batch(s.open("hub", ""), s.install(identifier, name, category, func() tea.Cmd { return nil }))
	case "inspect":
		if rest == "" {
			return tea.Batch(s.open("skills", ""), s.hub.Fail("usage: /skills inspect <name>"))
		}
		return tea.Batch(s.open("skills", ""), s.inspect(rest))
	}
	return s.open("skills", strings.TrimSpace(argument))
}

// Scope of the screen: the profile in use, the folder of the session in view
func (s *screen) scope() *skillsv1.Scope {
	return &skillsv1.Scope{Directory: s.kit.Env.WorkDir()}
}

func (s *screen) open(part, search string) tea.Cmd {
	list := s.part(part)
	s.hub = s.kit.Hub(Screen, list)
	s.hub.OnClose(s.discardWaiting)
	if search != "" {
		list.Filter(search)
	}
	return s.kit.WM.ShowOverlay(s.hub)
}

// Swaps the part in view for the one d steps away
func (s *screen) cycle(from string, d int) tea.Cmd {
	i := slices.Index(parts, from)
	return s.hub.Swap(s.part(parts[(i+d+len(parts))%len(parts)]))
}

// One part of the screen
func (s *screen) part(name string) *cards.List {
	var spec cards.ListSpec
	switch name {
	case "hub":
		spec = s.hubPart(s.query, s.page)
	case "bundles":
		spec = s.bundles()
	case "sources":
		spec = s.sources()
	case "trusted":
		spec = s.trusted()
	default:
		spec = s.skills()
	}
	spec.Title, spec.Glyph, spec.Key = "Skills › "+titles[name], s.kit.Env.Theme.Glyphs.Ext, Screen+":"+name
	spec.Context, spec.Shared, spec.Primary = context, []string{cards.HubContext}, "hub.use"
	own := spec.Status
	spec.Status = func() string {
		var out []string
		if own != nil {
			if status := own(); status != "" {
				out = append(out, status+" ")
			}
		}
		for _, p := range parts {
			if p == name {
				out = append(out, "["+titles[p]+"]")
			} else {
				out = append(out, titles[p])
			}
		}
		return strings.Join(out, " ")
	}
	if spec.Free == nil {
		spec.Free = map[string]bool{}
	}
	spec.Free["skills.next"], spec.Free["skills.prev"], spec.Free["hub.refresh"] = true, true, true
	spec.Actions["skills.next"] = func(*cards.List, int) tea.Cmd { return s.cycle(name, 1) }
	spec.Actions["skills.prev"] = func(*cards.List, int) tea.Cmd { return s.cycle(name, -1) }
	hints := spec.Hints
	spec.Hints = func(l *cards.List, i int) []wm.Hint {
		var out []wm.Hint
		if hints != nil {
			out = hints(l, i)
		}
		return append(out, wm.Hint{Action: "skills.next"})
	}
	list := cards.NewList(s.kit.Env, spec)
	list.ToggleDetail()
	return list
}

// Installed skills: on or off, where each comes from and what it does
func (s *screen) skills() cards.ListSpec {
	var (
		state  *skillsv1.ListResponse
		loaded bool
		failed error
		list   *cards.List
	)
	rows := func() []*skillsv1.Skill { return state.GetSkills() }
	reload := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := s.service.List(s.kit.Env.Ctx, &skillsv1.ListRequest{Scope: s.scope()})
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
	toggle := func(skill *skillsv1.Skill, where string) tea.Cmd {
		if skill.GetEssential() {
			return s.hub.Fail(skill.GetName() + " is essential: the system prompt points at it, so it stays on")
		}
		on := !skill.GetEnabled()
		label := "everywhere"
		if where != "" {
			on, label = slices.Contains(skill.GetDisabledOn(), where), "in the terminal app"
		}
		return tui.Call(s.hub, func() (*skillsv1.ToggleResponse, error) {
			return s.service.Toggle(s.kit.Env.Ctx, &skillsv1.ToggleRequest{Scope: s.scope(), Names: []string{skill.GetName()}, Enabled: on, Surface: where})
		}, func(*skillsv1.ToggleResponse) tea.Cmd {
			verb := "Turned off "
			if on {
				verb = "Turned on "
			}
			return tea.Batch(s.hub.Done(verb+skill.GetName()+" "+label), reload())
		})
	}
	t := s.kit.Env.Theme
	spec := cards.ListSpec{
		Search:  "Search skills",
		Columns: []render.Column{{}, {Title: "Skill", Max: 32}, {Title: "Category", Max: 22}, {Title: "Source", Max: 12}, {Title: "What it does", Flex: 1}},
		Yield:   []int{4, 2, 3},
		Rows: func() [][]string {
			out := make([][]string, len(rows()))
			for i, skill := range rows() {
				mark := render.Good(t.Glyphs.Check, t)
				switch {
				case !skill.GetEnabled():
					mark = render.Dim(t.Glyphs.Hollow, t)
				case slices.Contains(skill.GetDisabledOn(), surface):
					mark = render.Warm(t.Glyphs.Skip, t)
				}
				out[i] = []string{mark, skill.GetName(), render.Dim(skill.GetCategory(), t), sourceText(skill), skill.GetDescription()}
			}
			return out
		},
		Detail: func(i int) []string { return s.detail(rows()[i]) },
		Banner: func() string {
			if project := state.GetUntrustedProject(); project != "" {
				return render.Warm(fmt.Sprintf("%s holds %d skills that don't load until it is trusted (Trusted projects)", project, state.GetUntrustedCount()), t)
			}
			return ""
		},
		Empty: func() string { return tui.Waiting(loaded, failed, "No skills installed") },
		Free:  map[string]bool{"hub.add": true, "skills.update": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			every := []wm.Hint{{Action: "hub.add", Label: "install"}, {Action: "skills.update", Label: "all skills"}}
			if i < 0 {
				return every
			}
			label := "turn off"
			if !rows()[i].GetEnabled() {
				label = "turn on"
			}
			hints := []wm.Hint{{Action: "hub.use", Label: label}, {Action: "skills.surface"}, {Action: "hub.edit", Label: "view"}, {Action: "skills.more"}}
			switch rows()[i].GetSource() {
			case "builtin":
				hints = append(hints, wm.Hint{Action: "hub.reset", Label: "reset"})
			case "hub":
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "uninstall"})
			}
			return append(hints, every...)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":        func(_ *cards.List, i int) tea.Cmd { return toggle(rows()[i], "") },
			"skills.surface": func(_ *cards.List, i int) tea.Cmd { return toggle(rows()[i], surface) },
			"hub.edit":       func(_ *cards.List, i int) tea.Cmd { return s.view(rows()[i].GetName()) },
			"hub.reset":      func(_ *cards.List, i int) tea.Cmd { return s.reset(rows()[i], reload) },
			"hub.remove":     func(l *cards.List, i int) tea.Cmd { return s.uninstall(l, rows()[i], reload) },
			"hub.add":        func(*cards.List, int) tea.Cmd { return s.cycle("skills", 1) },
			"skills.more": func(l *cards.List, i int) tea.Cmd {
				skill := rows()[i]
				return s.more(l, skill, func() tea.Cmd { return toggle(skill, "") }, reload)
			},
			"skills.update": func(*cards.List, int) tea.Cmd { return s.every(state.GetOptedOut(), reload) },
			"hub.refresh": func(*cards.List, int) tea.Cmd {
				return tui.Call(s.hub, func() (*skillsv1.ReloadResponse, error) {
					return s.service.Reload(s.kit.Env.Ctx, &skillsv1.ReloadRequest{Scope: s.scope()})
				}, func(response *skillsv1.ReloadResponse) tea.Cmd {
					return tea.Batch(s.hub.Done(reloaded(response)), reload())
				})
			},
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			return reload()
		},
	}
	return spec
}

// Where a skill comes from in a word
func sourceText(skill *skillsv1.Skill) string {
	switch skill.GetSource() {
	case "builtin":
		if skill.GetModified() {
			return "built in, changed"
		}
		return "built in"
	case "create_dir":
		return "created"
	}
	return skill.GetSource()
}

// Lines of a skill's detail
func (s *screen) detail(skill *skillsv1.Skill) []string {
	t := s.kit.Env.Theme
	lines := []string{skill.GetDescription(), "From: " + sourceText(skill) + render.Dim("  trust "+skill.GetTrust(), t)}
	switch {
	case skill.GetEssential():
		lines = append(lines, render.Good("Essential: the system prompt points at it, so it stays on", t))
	case !skill.GetEnabled():
		lines = append(lines, render.Dim("Off for the agent everywhere", t))
	case len(skill.GetDisabledOn()) > 0:
		lines = append(lines, render.Warm("Off on: "+strings.Join(skill.GetDisabledOn(), ", "), t))
	}
	if command := skill.GetCommand(); command != "" {
		lines = append(lines, "Command: /"+command)
	} else if note := skill.GetCommandNote(); note != "" {
		lines = append(lines, render.Warm(note, t))
	}
	if skill.GetUseCount()+skill.GetViewCount()+skill.GetPatchCount() > 0 {
		usage := fmt.Sprintf("Used %d, viewed %d, changed %d times", skill.GetUseCount(), skill.GetViewCount(), skill.GetPatchCount())
		if last := skill.GetLastActivity(); last != "" {
			usage += ", last " + last
		}
		lines = append(lines, render.Dim(usage, t))
	}
	var holds []string
	if skill.GetPinned() {
		holds = append(holds, "pinned")
	}
	if skill.GetLocked() {
		holds = append(holds, "locked from the agent")
	}
	if state := skill.GetState(); state != "" && state != "active" {
		holds = append(holds, state)
	}
	if len(holds) > 0 {
		lines = append(lines, render.Warm(strings.Join(holds, ", "), t))
	}
	return append(lines, render.Dim(skill.GetPath(), t))
}

// A page of the skill as the agent loads it: its content, what it still needs, its files and how it is written
func (s *screen) view(name string) tea.Cmd {
	return tui.Call(s.hub, func() (*skillsv1.ShowResponse, error) {
		return s.service.Show(s.kit.Env.Ctx, &skillsv1.ShowRequest{Scope: s.scope(), Name: name})
	}, func(response *skillsv1.ShowResponse) tea.Cmd { return s.viewPage(name, response) })
}

// The page of a skill as the agent loads it
func (s *screen) viewPage(name string, response *skillsv1.ShowResponse) tea.Cmd {
	t := s.kit.Env.Theme
	page := cards.NewPage(s.kit.Env, cards.PageSpec{Title: "Skill › " + name, Key: Screen + ":view", Lines: func(w int) []string {
		var lines []string
		if note := response.GetSetupNote(); note != "" {
			lines = append(lines, render.Warm(render.Wrap(note, w), t), "")
		}
		if tags := response.GetTags(); len(tags) > 0 {
			lines = append(lines, render.Dim("Tags: "+strings.Join(tags, ", "), t))
		}
		if related := response.GetRelated(); len(related) > 0 {
			lines = append(lines, render.Dim("Related: "+strings.Join(related, ", "), t))
		}
		if files := response.GetFiles(); len(files) > 0 {
			lines = append(lines, render.Dim("Files: "+strings.Join(files, ", "), t))
		}
		if b := response.GetBlueprint(); b != nil {
			line := "Automation: runs on schedule " + b.GetSchedule()
			if b.GetDeliver() != "origin" {
				line += ", delivering to " + b.GetDeliver()
			}
			if b.GetNoAgent() {
				line += ", without the agent"
			}
			lines = append(lines, render.Cool(render.Wrap(line, w), t))
		}
		if problem := response.GetBlueprintError(); problem != "" {
			lines = append(lines, render.Bad(render.Wrap("Automation blueprint: "+problem, w), t))
		}
		if findings := response.GetFindings(); len(findings) > 0 {
			lines = append(lines, "", render.Heading("How it is written", t))
			for _, f := range findings {
				lines = append(lines, render.Wrap(fmt.Sprintf("%s %s: %s", f.GetSeverity(), f.GetRule(), f.GetMessage()), w))
			}
		}
		lines = append(lines, "", render.Heading("SKILL.md", t))
		for _, line := range strings.Split(response.GetContent(), "\n") {
			lines = append(lines, strings.Split(render.Wrap(line, w), "\n")...)
		}
		return lines
	}})
	return s.hub.Push(page)
}

// Lets a built in skill follow its source again, or puts the shipped copy back
func (s *screen) reset(skill *skillsv1.Skill, reload func() tea.Cmd) tea.Cmd {
	if skill.GetSource() != "builtin" {
		return s.hub.Fail(skill.GetName() + " is not a built in skill")
	}
	choices := []tui.Choice{
		{ID: "follow", Title: "Keep my copy, follow updates", Detail: "Stops tracking your changes; the next update of the built in skill replaces your copy"},
		{ID: "restore", Title: "Restore the built in copy", Detail: "Deletes your copy and copies the built in version again"},
	}
	return s.kit.Pick(s.hub, "Reset "+skill.GetName(), "reset", choices, "", func(choice tui.Choice) tea.Cmd {
		return tui.Call(s.hub, func() (*skillsv1.ResetResponse, error) {
			return s.service.Reset(s.kit.Env.Ctx, &skillsv1.ResetRequest{Scope: s.scope(), Name: skill.GetName(), Restore: choice.ID == "restore"})
		}, func(response *skillsv1.ResetResponse) tea.Cmd {
			return tea.Batch(s.hub.Done(response.GetMessage()), reload())
		})
	})
}

// What a reload changed, in a line
func reloaded(response *skillsv1.ReloadResponse) string {
	if len(response.GetAdded())+len(response.GetRemoved()) == 0 {
		return fmt.Sprintf("No new skills detected; %d skill commands", response.GetTotal())
	}
	var parts []string
	for _, c := range response.GetAdded() {
		parts = append(parts, "+/"+c.GetName())
	}
	for _, c := range response.GetRemoved() {
		parts = append(parts, "-/"+c.GetName())
	}
	return "Skills reloaded: " + strings.Join(parts, " ")
}

// Bundles: several skills loaded under one slash command, made and removed here
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
		Search:  "Search bundles",
		Columns: []render.Column{{Title: "Command", Max: 28}, {Title: "Skills"}, {Title: "What it does", Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			out := make([][]string, len(rows()))
			for i, b := range rows() {
				out[i] = []string{"/" + b.GetSlug(), fmt.Sprint(len(b.GetSkills())), b.GetDescription()}
			}
			return out
		},
		Detail: func(i int) []string {
			b := rows()[i]
			lines := []string{b.GetName(), "Skills: " + strings.Join(b.GetSkills(), ", ")}
			if b.GetInstruction() != "" {
				lines = append(lines, "Instruction: "+b.GetInstruction())
			}
			return append(lines, render.Dim(b.GetPath(), t))
		},
		Empty: func() string {
			if loaded && failed == nil {
				return "No bundles yet: " + s.kit.Env.Keys.Display("hub.add") + " makes one in " + state.GetDir()
			}
			return tui.Waiting(loaded, failed, "No bundles")
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{{Action: "hub.add", Label: "new"}}
			if i >= 0 {
				hints = append([]wm.Hint{{Action: "hub.use", Label: "change"}}, append(hints, wm.Hint{Action: "hub.remove", Label: "delete"})...)
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd { return s.editBundle(nil, reload) },
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return s.editBundle(rows()[i], reload) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				b := rows()[i]
				l.Confirm("Delete the bundle /"+b.GetSlug()+"?", func() tea.Cmd {
					return tui.Call(s.hub, func() (*skillsv1.DeleteBundleResponse, error) {
						return s.service.DeleteBundle(s.kit.Env.Ctx, &skillsv1.DeleteBundleRequest{Scope: s.scope(), Name: b.GetName()})
					}, func(*skillsv1.DeleteBundleResponse) tea.Cmd {
						return tea.Batch(s.hub.Done("Deleted /"+b.GetSlug()), reload())
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
	title := "New bundle"
	if b != nil {
		title = "Bundle /" + b.GetSlug()
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title: title,
		Fields: []cards.Field{
			{ID: "name", Label: "Name", Value: b.GetName(), Required: true, Help: "Its slash command is the name in lower case with hyphens"},
			{ID: "skills", Label: "Skills", Value: strings.Join(b.GetSkills(), ", "), Required: true, Help: "Names of the skills it loads, separated by commas"},
			{ID: "description", Label: "Description", Value: b.GetDescription(), Help: "What it is for, shown with its command"},
			{ID: "instruction", Label: "Instruction", Value: b.GetInstruction(), Help: "Told to the model along with the skills"},
		},
		Submit: "save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			var names []string
			for _, name := range strings.Split(values["skills"], ",") {
				if name = strings.TrimSpace(name); name != "" {
					names = append(names, name)
				}
			}
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
				return tea.Batch(s.hub.Close(form), s.hub.Done("Saved /"+response.GetBundle().GetSlug()), reload())
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
		Columns: []render.Column{{Title: "Folder", Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(trusted))
			for i, folder := range trusted {
				out[i] = []string{folder}
			}
			return out
		},
		Detail: func(int) []string {
			return []string{"The skills of a trusted project, in its .icebath/skills and .agents/skills, load for sessions working in it"}
		},
		Empty: func() string {
			return tui.Waiting(loaded, failed, "No trusted projects: "+s.kit.Env.Keys.Display("hub.add")+" trusts one")
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{{Action: "hub.add", Label: "trust"}}
			if i >= 0 {
				hints = append(hints, wm.Hint{Action: "hub.remove", Label: "untrust"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd {
				return s.kit.Ask(s.hub, "Trust a project", cards.Field{Label: "Folder", Value: s.kit.Env.WorkDir(),
					Help: "Skills inside a trusted project folder load for sessions working there"}, "trust", func(folder string) tea.Cmd {
					return s.trust(folder, false, reload)
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				folder := trusted[i]
				l.Confirm("Stop loading the skills of "+folder+"?", func() tea.Cmd { return s.trust(folder, true, reload) })
				return nil
			},
			"hub.use":     func(*cards.List, int) tea.Cmd { return nil },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
		},
		OnFocus: func(l *cards.List) tea.Cmd {
			list = l
			return reload()
		},
	}
}
