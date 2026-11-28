// The page of Skills: the categories of skills with how many of each are on, the page of each category with a switch per skill, a search of
// every skill, the page of each skill, finding and installing skills, bundles, sources, trusted projects, the skills you changed, and the
// options of the area
package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	hubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/skills/skills/config"
	"github.com/nickheyer/icebath/internal/profile"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	skillsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/skills/v1/skillsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the skills
const Screen = "skills"

// Keymap context of the rows of skills
const context = "skills"

// App whose own turned off skills this app turns on and off
const surface = "tui"

// Headings of the page: the categories, then the options
const (
	categoriesGroup = "Categories"
	optionsGroup    = "Options"
)

// Sections under Skills whose settings are a page each, in the order they are listed
var sections = []string{skillsconfig.Variables, skillsconfig.Values, hubconfig.Hub}

// What the argument of the screen opens over the page, besides the words of /skills
const (
	categoryOf = "category:"
	skillOf    = "skill:"
	bundleOf   = "bundle:"
	sectionOf  = "section:"
	changedArg = "changed"
)

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
		Usage: "[@profile] [list|hub|bundles|sources|trusted|browse [page]|search <words>|inspect <name>|install <name or address>|<words to find>]",
		Area:  skillsconfig.Key,
		Where: map[string]string{context: "skills page", review: "skill hub operation"},
		Actions: []keys.Action{
			{ID: "skills.turn", Context: context, Label: "turn on/off", Doc: "Turn the skill in hand on or off", Default: []string{"t"}},
			{ID: "skills.search", Context: context, Label: "search", Doc: "Search the registries of the skill hub", Default: []string{"ctrl+f"}},
			{ID: "skills.go", Context: review, Label: "continue", Doc: "Continue what is waiting, such as installing a scanned skill", Default: []string{"enter"}},
		},
		Pages: func(kit *tui.Kit, name string) []tui.Page { return skillPages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			s := &screen{kit: kit, service: service, settings: settings, profile: name, waiting: map[string]func(){}}
			return s.open(rest)
		},
	}}
}

type screen struct {
	kit      *tui.Kit
	service  skillsv1connect.SkillsServiceClient
	settings configv1connect.ConfigServiceClient
	// Profile the page was opened on, the one in use when empty
	profile string
	hub     *cards.Hub
	root    *cards.List
	options *configapp.Rows
	// Search, registry and page the skill hub shows
	query  string
	source string
	page   int
	// Installs staged by this page that wait for the person's yes, each with what its page does once the staged copy is dropped
	waiting map[string]func()
	// Skills as last read, with the bundles and sources the rows that open them count
	state   *skillsv1.ListResponse
	bundled []*skillsv1.Bundle
	taps    []*skillsv1.SkillTap
	loaded  bool
	err     error
	rows    []row
	// Lists of the pages opened over the page, read again with it
	views cards.Watched
	// Puts what was just made in hand once the skills are read again, nil for nothing
	after func()
	// Updates of skills from the hub that ran on this page, which the page of updates checks them again after
	updated int
}

// Row of the page: a category, a skill a search found, something the page opens or does, an option, a section of options or the row of the
// advanced options
type row struct {
	category *skillsv1.Category
	skill    *skillsv1.Skill
	act      *act
	setting  *configv1.Setting
	section  string
	advanced bool
}

// Something a row does: its name, what it holds or says, what it does, the word on its button, whether it opens a page, and the work
type act struct {
	title, value, about, verb string
	opens                     bool
	run                       func(l *cards.List) tea.Cmd
}

// Scope of every call: the profile of the page and the folder of the session in view
func (s *screen) scope() *skillsv1.Scope {
	return &skillsv1.Scope{Profile: s.profile, Directory: s.kit.Env.WorkDir()}
}

// Shows the page, then reads the skills and the options, then opens what the argument names over the page
func (s *screen) open(argument string) tea.Cmd {
	t := s.kit.Env.Theme
	s.root = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   "Skills",
		Glyph:   t.Glyphs.Ext,
		Key:     Screen,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search skills",
		Intro:   s.intro,
		Columns: configapp.Columns,
		Rows: func() [][]string {
			s.rows = s.order(s.root.Query() != "")
			cells := make([][]string, len(s.rows))
			for i, r := range s.rows {
				cells[i] = s.cells(r)
			}
			return cells
		},
		Group: func(i int) string { return s.group(s.rows[i]) },
		Words: func(i int) string {
			if skill := s.rows[i].skill; skill != nil {
				return skill.GetDescription() + " " + skill.GetCategoryTitle()
			}
			return ""
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.rows) {
				return nil
			}
			return s.detail(s.rows[i], s.root.Cut(i, 2))
		},
		Empty: func() string {
			if q := s.root.Query(); q != "" {
				return "No skill matches “" + q + "”."
			}
			return tui.Waiting(s.loaded, s.err, "")
		},
		OnFocus: func(*cards.List) tea.Cmd { return s.reload() },
		Free:    map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return s.hints(i) },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(l *cards.List, i int) tea.Cmd { return s.use(l, s.rows[i]) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return s.removeRow(l, s.rows[i]) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return s.findSkills() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.readAgain() },
		},
	})
	s.root.ToggleDetail()
	hub, cmd := s.kit.Show(Screen, s.root)
	s.hub = hub
	hub.OnClose(s.discardWaiting)
	s.options = configapp.NewRows(s.kit, hub, s.settings, s.profile)
	s.options.Watch(s.root)
	// What the argument opens may be a section of the options, so they are read first
	return tea.Batch(cmd, s.options.Load(func() tea.Cmd {
		return s.read(func() tea.Cmd { return s.slash(strings.TrimSpace(argument)) })
	}))
}

// What /skills opens over the page: a view, a verb of the skill hub, a page a search of Settings found, or the skills found by the words
func (s *screen) slash(argument string) tea.Cmd {
	word, rest := verb(argument)
	switch {
	case word == "" || word == "list":
		return nil
	case word == "hub":
		return s.findSkills()
	case word == "bundles":
		return s.bundles()
	case word == "sources":
		return s.sources()
	case word == "trusted":
		return s.trusted()
	case word == changedArg:
		return s.changed()
	case word == "browse":
		n, ok := pageNumber(rest)
		if !ok {
			return tea.Batch(s.findSkills(), s.hub.Fail("The page is a number from 1 up, such as /skills browse 2."))
		}
		s.page = n
		return s.findSkills()
	case word == "search":
		if rest == "" {
			return tea.Batch(s.findSkills(), s.hub.Fail("Say what to search for, such as /skills search pdf."))
		}
		s.query = rest
		return s.findSkills()
	case word == "install":
		identifier, name, category := installArgs(rest)
		if identifier == "" {
			return tea.Batch(s.findSkills(), s.hub.Fail("Name the skill to install, such as /skills install pdf-tools."))
		}
		return tea.Batch(s.findSkills(), s.install(identifier, name, category, s.reload))
	case word == "inspect":
		if rest == "" {
			return s.hub.Fail("Name the skill to read, such as /skills inspect pdf-tools.")
		}
		return s.inspect(rest)
	case strings.HasPrefix(word, categoryOf):
		return s.categoryPage(strings.TrimPrefix(argument, categoryOf))
	case strings.HasPrefix(word, skillOf):
		name := strings.TrimPrefix(argument, skillOf)
		if s.skill(name) == nil {
			return s.hub.Fail("There is no skill " + name + " now.")
		}
		return s.skillPage(name)
	case strings.HasPrefix(word, bundleOf):
		return tea.Batch(s.bundles(), s.bundlePage(strings.TrimPrefix(argument, bundleOf)))
	case strings.HasPrefix(word, sectionOf):
		return s.sectionPage(strings.TrimPrefix(argument, sectionOf))
	}
	s.root.Filter(argument)
	return nil
}

// First line of the page: what the area is
func (s *screen) intro() []string {
	lines := []string{"Instructions the agent loads for certain kinds of work."}
	// Where a skill the agent writes, or one you add by hand, goes
	if folder := s.state.GetCreateDir(); folder != "" {
		lines = append(lines, render.Dim("New skills are made in "+folder+".", s.kit.Env.Theme))
	}
	return lines
}

// Reads the skills, the bundles and the sources again
func (s *screen) reload() tea.Cmd { return s.read(nil) }

// Reads the skills, the bundles and the sources again, then runs then
func (s *screen) read(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		ctx := s.kit.Env.Ctx
		response, err := s.service.List(ctx, &skillsv1.ListRequest{Scope: s.scope(), Surface: surface})
		var bundled []*skillsv1.Bundle
		var taps []*skillsv1.SkillTap
		if err == nil {
			if listed, failed := s.service.Bundles(ctx, &skillsv1.BundlesRequest{Scope: s.scope()}); failed == nil {
				bundled = listed.GetBundles()
			}
			if listed, failed := s.service.Taps(ctx, &skillsv1.TapsRequest{Scope: s.scope()}); failed == nil {
				taps = listed.GetTaps()
			}
		}
		return func() tea.Cmd {
			s.err, s.loaded = err, true
			if err == nil {
				s.state, s.bundled, s.taps = response, bundled, taps
			}
			s.refresh()
			if then != nil {
				return then()
			}
			return nil
		}
	})
}

// Shows the skills as last read on the page and on every page opened over it
func (s *screen) refresh() {
	s.root.Refresh()
	s.views.Refresh()
	if s.after != nil {
		s.after()
		s.after = nil
	}
}

// Scans the skills folders again and says which slash commands came and went
func (s *screen) readAgain() tea.Cmd {
	s.hub.Busy("Reading the skills again…")
	return tui.Call(s.hub, func() (*skillsv1.ReloadResponse, error) {
		return s.service.Reload(s.kit.Env.Ctx, &skillsv1.ReloadRequest{Scope: s.scope()})
	}, func(response *skillsv1.ReloadResponse) tea.Cmd {
		return tea.Batch(s.hub.Done(reloaded(response)), s.reload(), s.options.Load(nil))
	})
}

// Skill of the name as last read, nil for none
func (s *screen) skill(name string) *skillsv1.Skill {
	for _, skill := range s.state.GetSkills() {
		if skill.GetName() == name {
			return skill
		}
	}
	return nil
}

// Category of the name as last read, nil for none
func (s *screen) category(name string) *skillsv1.Category {
	for _, category := range s.state.GetCategories() {
		if category.GetName() == name {
			return category
		}
	}
	return nil
}

// Skills of a category by name
func (s *screen) skillsOf(category string) []*skillsv1.Skill {
	var out []*skillsv1.Skill
	for _, skill := range s.state.GetSkills() {
		if skill.GetCategory() == category {
			out = append(out, skill)
		}
	}
	slices.SortFunc(out, func(a, b *skillsv1.Skill) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out
}

// Rows of the page once the skills are read: the categories, what the page opens, the options and the sections of options and the row of
// the advanced ones; while searched, every skill, each with its state
func (s *screen) order(searching bool) []row {
	if !s.loaded || s.err != nil {
		return nil
	}
	var rows []row
	if searching {
		for _, category := range s.state.GetCategories() {
			for _, skill := range s.skillsOf(category.GetName()) {
				rows = append(rows, row{skill: skill})
			}
		}
		return rows
	}
	for _, category := range s.state.GetCategories() {
		rows = append(rows, row{category: category})
	}
	for _, a := range s.acts() {
		rows = append(rows, row{act: a})
	}
	everyday, _ := s.options.Section(skillsconfig.Key)
	for _, setting := range everyday {
		rows = append(rows, row{setting: setting})
	}
	for _, key := range sections {
		if s.holds(key) {
			rows = append(rows, row{section: key})
		}
	}
	if _, _, ok := s.options.Advanced(skillsconfig.Key); ok {
		rows = append(rows, row{advanced: true})
	}
	return rows
}

// Whether a section of options holds a setting to change
func (s *screen) holds(key string) bool {
	if s.options.Info(key) == nil {
		return false
	}
	everyday, advanced := s.options.Own(key)
	return len(everyday)+len(advanced) > 0
}

// What the page opens and does besides the categories: finding skills, the bundles, the sources, the trusted projects, trusting the project
// of the session while it is not, and the skills you changed
func (s *screen) acts() []*act {
	out := []*act{
		{title: "Find skills", about: "Browse, search and install skills from the registries of the skill hub.", verb: "open", opens: true,
			run: func(*cards.List) tea.Cmd { return s.findSkills() }},
		{title: "Bundles", value: counted(len(s.bundled), "bundle"), about: "Several skills loaded at once with one slash command.", verb: "open",
			opens: true, run: func(*cards.List) tea.Cmd { return s.bundles() }},
		{title: "Sources", value: counted(len(s.taps), "source"), about: "GitHub repositories the skill hub searches besides its registries.",
			verb: "open", opens: true, run: func(*cards.List) tea.Cmd { return s.sources() }},
		{title: "Trusted projects", value: counted(len(s.state.GetTrusted()), "folder"), about: "Project folders whose own skills load.",
			verb: "open", opens: true, run: func(*cards.List) tea.Cmd { return s.trusted() }},
	}
	if a := s.trustThis(); a != nil {
		out = append(out, a)
	}
	return append(out, &act{title: "Skills you changed", value: counted(len(s.yours()), "skill"),
		about: "Built-in skills you edited, and skills you made yourself.", verb: "open", opens: true,
		run: func(*cards.List) tea.Cmd { return s.changed() }})
}

// A count of things in words, None for none
func counted(n int, noun string) string {
	if n == 0 {
		return "None"
	}
	return render.Count(n, noun)
}

// Trusting the project the session works in, nil while it is trusted or there is none
func (s *screen) trustThis() *act {
	project := s.state.GetProject()
	if project == "" || s.state.GetProjectTrusted() {
		return nil
	}
	about := "Lets the skills " + project + " keeps in its own folder load. It keeps none yet."
	if n := int(s.state.GetProjectSkills()); n > 0 {
		about = "Lets the " + render.Count(n, "skill") + " " + project + " keeps in its own folder load."
	}
	return &act{title: "Trust this project", about: about, verb: "trust", run: func(l *cards.List) tea.Cmd {
		l.Confirm(cards.Question{Text: "Trust " + project + "? Its own skills then load for sessions working in it, and they can tell the agent what to do.",
			Verb: "Trust"}, func() tea.Cmd { return s.trust(project, false, nil) })
		return nil
	}}
}

// Skills you changed or made: built-in ones you edited, then your own
func (s *screen) yours() []*skillsv1.Skill {
	var edited, made []*skillsv1.Skill
	for _, skill := range s.state.GetSkills() {
		switch {
		case skill.GetSource() == "builtin" && skill.GetModified():
			edited = append(edited, skill)
		case skill.GetSource() == "local" || skill.GetSource() == "create_dir":
			made = append(made, skill)
		}
	}
	return append(edited, made...)
}

// Heading a row is listed under: the categories theirs, what the page opens none so it sits with them, the options theirs, a skill found
// none
func (s *screen) group(r row) string {
	switch {
	case r.category != nil:
		return categoriesGroup
	case r.setting != nil, r.section != "", r.advanced:
		return optionsGroup
	}
	return ""
}

// Cells of a row: its name, the mark of a changed value or of a row that opens, and its state or value
func (s *screen) cells(r row) []string {
	t := s.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.category != nil:
		return []string{r.category.GetTitle(), "", r.category.GetState()}
	case r.skill != nil:
		sep := " " + t.Glyphs.Forward + " "
		return []string{render.Dim(r.skill.GetCategoryTitle()+sep, t) + r.skill.GetName(), "", skillState(r.skill.GetShown(), t)}
	case r.act != nil && r.act.opens:
		return []string{r.act.title, opens, render.Dim(r.act.value, t)}
	case r.act != nil:
		return []string{r.act.title, "", ""}
	case r.setting != nil:
		return s.options.Cells(r.setting)
	case r.section != "":
		return []string{s.options.Info(r.section).GetTitle(), opens, render.Dim(s.inside(r.section), t)}
	}
	cells, _, _ := s.options.Advanced(skillsconfig.Key)
	return cells
}

// State of a skill in its words, in the colour of what it means
func skillState(state string, t theme.Theme) string {
	switch state {
	case "Off":
		return render.Dim(state, t)
	case "Off in this app":
		return render.Warm(state, t)
	}
	return state
}

// How many settings a row that opens a section holds, and how many were changed
func (s *screen) inside(key string) string {
	everyday, advanced := s.options.Own(key)
	all := append(everyday, advanced...)
	changed := 0
	for _, setting := range all {
		if setting.GetSource() == configv1.Source_SOURCE_FILE {
			changed++
		}
	}
	said := render.Count(len(all), "setting")
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// Lines under the list about the row in hand, never what the row or the first line says
func (s *screen) detail(r row, cut bool) []string {
	switch {
	case r.category != nil:
		var names []string
		for _, skill := range s.skillsOf(r.category.GetName()) {
			names = append(names, skill.GetName())
		}
		if about := r.category.GetAbout(); about != "" {
			return []string{described(about), render.Dim(strs.And(names)+".", s.kit.Env.Theme)}
		}
		return []string{strs.And(names) + "."}
	case r.skill != nil:
		return []string{described(r.skill.GetDescription())}
	case r.act != nil:
		return []string{r.act.about}
	case r.setting != nil:
		return s.options.Detail(r.setting, cut)
	case r.section != "":
		return []string{described(s.options.Info(r.section).GetDescription())}
	}
	return []string{"Settings that few people change."}
}

// Buttons for the row in hand: what enter does, uninstall on a skill from the hub, reset on a changed option, then find skills
func (s *screen) hints(i int) []wm.Hint {
	find := wm.Hint{Action: "hub.add", Label: "find skills"}
	if i < 0 || i >= len(s.rows) {
		return []wm.Hint{find}
	}
	switch r := s.rows[i]; {
	case r.skill != nil:
		hints := []wm.Hint{{Action: "hub.use", Label: "open"}}
		if r.skill.GetSource() == "hub" {
			hints = append(hints, wm.Hint{Action: "hub.remove", Label: "uninstall"})
		}
		return append(hints, find)
	case r.act != nil:
		return []wm.Hint{{Action: "hub.use", Label: r.act.verb}, find}
	case r.setting != nil:
		return append(s.options.Hints(r.setting), find)
	}
	return []wm.Hint{{Action: "hub.use", Label: "open"}, find}
}

// What enter does on a row
func (s *screen) use(l *cards.List, r row) tea.Cmd {
	switch {
	case r.category != nil:
		return s.categoryPage(r.category.GetName())
	case r.skill != nil:
		return s.skillPage(r.skill.GetName())
	case r.act != nil:
		return r.act.run(l)
	case r.setting != nil:
		return s.options.Change(r.setting)
	case r.section != "":
		return s.sectionPage(r.section)
	}
	_, open, _ := s.options.Advanced(skillsconfig.Key)
	return open()
}

// What ctrl+d does on a row: uninstalls a skill from the hub after a question, or puts a changed option back to its default
func (s *screen) removeRow(l *cards.List, r row) tea.Cmd {
	switch {
	case r.skill != nil && r.skill.GetSource() == "hub":
		return s.uninstall(l, r.skill)
	case r.setting != nil:
		return s.options.Reset(l, r.setting)
	}
	return nil
}

// What a reload changed, in a sentence or two
func reloaded(response *skillsv1.ReloadResponse) string {
	if len(response.GetAdded())+len(response.GetRemoved()) == 0 {
		return "Read the skills again. No slash command came or went."
	}
	var parts []string
	for _, group := range []struct {
		label    string
		commands []*skillsv1.Command
	}{{"New", response.GetAdded()}, {"Gone", response.GetRemoved()}} {
		if len(group.commands) == 0 {
			continue
		}
		names := make([]string, len(group.commands))
		for i, c := range group.commands {
			names[i] = "/" + c.GetName()
		}
		parts = append(parts, group.label+": "+strings.Join(names, ", ")+".")
	}
	return "Read the skills again. " + strings.Join(parts, " ")
}

// The verb of /skills and the rest of its line
func verb(argument string) (string, string) {
	word, rest, _ := strings.Cut(strings.TrimSpace(argument), " ")
	return word, strings.TrimSpace(rest)
}

// Skill, name and category of the line of /skills install
func installArgs(rest string) (string, string, string) {
	fields := strings.Fields(rest)
	var identifier, name, category string
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "--name", "--category":
			if i+1 < len(fields) {
				if fields[i] == "--name" {
					name = fields[i+1]
				} else {
					category = fields[i+1]
				}
				i++
			}
		default:
			if identifier == "" {
				identifier = fields[i]
			}
		}
	}
	return identifier, name, category
}

// Page number of /skills browse, false when it is no positive number
func pageNumber(text string) (int, bool) {
	if text == "" {
		return 1, true
	}
	n, err := strconv.Atoi(text)
	return n, err == nil && n >= 1
}

// Text an author wrote, such as a skill's description, as one line that ends as a sentence
func described(text string) string {
	return strs.Full(strings.Join(strings.Fields(text), " "))
}
