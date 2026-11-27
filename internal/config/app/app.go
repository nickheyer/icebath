package app

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/ansi"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Screen of every setting, opened on a section by its key or searched for any other words
	Settings = "settings"
	// Screen that changes one setting, opened with its key, or one entry of a table setting with the entry's key
	Edit = "setting"
)

// Keymap context of the field one setting is typed into
const fieldContext = "settings.field"

var actions = []keys.Action{
	{ID: "settings.default", Context: fieldContext, Label: "use default", Doc: "Put the setting back to its default", Default: []string{"ctrl+d"}},
}

// Screens the settings open: the allowed commands, where the proposals and the check of a command the settings of approvals offer are
// made, and the providers, where one is added and its accounts connected
const (
	allowedScreen   = "approvals"
	providersScreen = "providers"
)

// Group the row of the settings files is listed in, on the first screen
const filesGroup = string(config.GroupApp)

// Cells a value takes in a row before it is cut, the whole of it then shown under the list
const valueRoom = 48

// Entries of a group of values a row names before it counts the rest
const namedFields = 3

// Columns of the first screen, a name, what is set there and how many settings inside were changed, and of the rows of a section, a
// name, the mark of a changed value or of a row that opens, and the value
var (
	homeColumns = []render.Column{{}, {Flex: 8, Whole: true}, {Flex: 1, Right: true}}
	rowColumns  = []render.Column{{}, {}, {Flex: 1}}
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{
		{
			ID:      Settings,
			Group:   tui.GroupApp,
			Order:   10,
			Title:   "Settings",
			Short:   "Change how icebath and the agent work",
			Slash:   []string{"settings", "config", "preferences"},
			Usage:   "[@profile] [section or words to search for]",
			Actions: actions,
			Where:   map[string]string{fieldContext: "setting fields"},
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, target := profile.Split(argument)
				return (&settings{kit: kit, service: service, profile: name, named: name}).open(target)
			},
		},
		{
			ID:    Edit,
			Short: "Change one setting",
			Open: func(kit *tui.Kit, argument string) tea.Cmd {
				name, key := profile.Split(argument)
				return (&settings{kit: kit, service: service, profile: name, named: name}).one(key)
			},
		},
	}
}

type settings struct {
	kit      *tui.Kit
	service  configv1connect.ConfigServiceClient
	hub      *cards.Hub
	root     *cards.List
	groups   []*configv1.Group
	sections []*configv1.Section
	all      []*configv1.Setting
	// Profile whose settings are shown, besides those every profile shares: the one opened on, else the one in use once read
	profile string
	// Profile the screen was opened on, the one in use when empty
	named string
	// Lists open over the hub, read again once the settings are
	views  []*cards.List
	loaded bool
	err    error
	// Section to open or words to search for once the settings are read
	target string
	// Opened on one setting, the hub closing once it is changed
	direct bool
	// Form the value of a setting is typed into, where a refusal of that value is shown under its field
	typed *cards.Form
}

// Something a row does besides opening a section or changing a setting
type action struct {
	title, about string
	// Word on the button that does it
	verb string
	run  func() tea.Cmd
}

// Row of a list: a section to open, a setting to change, the advanced settings of a section, or something else the screen does
type row struct {
	section *configv1.Section
	setting *configv1.Setting
	// Section whose advanced settings the row opens
	advanced *configv1.Section
	act      *action
	// Titles of the sections the row is under, said before its name where rows of several sections are listed together
	trail []string
}

// First screen: the sections at the top under the headings of their groups, and every setting once words are typed into the search
func (s *settings) open(target string) tea.Cmd {
	s.target = target
	t := s.kit.Env.Theme
	var shown []row
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   s.heading(),
		Glyph:   t.Glyphs.Settings,
		Key:     "settings",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search all settings",
		Columns: homeColumns,
		// What a search finds is listed as the rows of a section are: name, mark and value
		Layout: func() []render.Column {
			if list.Query() != "" {
				return rowColumns
			}
			return homeColumns
		},
		Rows: func() [][]string {
			searching := list.Query() != ""
			shown = s.home(searching)
			cells := make([][]string, len(shown))
			for i, r := range shown {
				if searching {
					cells[i] = s.cells(r)
				} else {
					cells[i] = s.homeCells(r)
				}
			}
			return cells
		},
		Group: func(i int) string { return s.groupOf(shown[i]) },
		Words: func(i int) string { return s.wordsOf(shown[i]) },
		Detail: func(i int) []string {
			r := shown[i]
			if list.Query() != "" {
				return s.detail(r, list.Cut(i, 2))
			}
			if list.Shows(1) {
				return nil
			}
			return s.detail(r, false)
		},
		Empty:   func() string { return tui.Waiting(s.loaded, s.err, "No settings to show.") },
		Free:    map[string]bool{"hub.refresh": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return s.hints(shown, i) },
		Actions: s.rowActions(&shown),
	})
	list.ToggleDetail()
	s.root = list
	s.views = append(s.views, list)
	s.hub = s.kit.Hub(Settings, s.root)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
}

// Title of the first screen, naming the profile when it is not the one a person starts with
func (s *settings) heading() string {
	if s.named != "" {
		return "Settings · " + s.named
	}
	return "Settings"
}

// Rows of the first screen: the sections at the top in their groups with the settings files among those of the app, or everything there is
// to find once words are typed
func (s *settings) home(searching bool) []row {
	if searching {
		return append(s.found(""), row{act: s.filesAction()})
	}
	rows := s.children("")
	at := len(rows)
	for i, r := range rows {
		if r.section.GetGroup() == filesGroup {
			at = i + 1
		}
	}
	return slices.Insert(rows, at, row{act: s.filesAction()})
}

// Row that opens the files the settings are kept in
func (s *settings) filesAction() *action {
	return &action{title: "Settings files", about: "Open config.yaml and your keys file in an editor", verb: "open", run: s.files}
}

// Heading of the group a row of the first screen is listed under
func (s *settings) groupOf(r row) string {
	id := filesGroup
	if r.section != nil {
		id = r.section.GetGroup()
	}
	for _, group := range s.groups {
		if group.GetId() == id {
			return group.GetTitle()
		}
	}
	return ""
}

// Cells of a row of the first screen: its name, what is set there and how many settings inside were changed
func (s *settings) homeCells(r row) []string {
	t := s.kit.Env.Theme
	switch {
	case r.section != nil:
		badge := ""
		if n := s.changed(r.section.GetKey()); n > 0 {
			badge = render.Cool(strconv.Itoa(n)+" changed", t)
		}
		return []string{r.section.GetTitle(), render.Dim(r.section.GetDescription(), t), badge}
	case r.act != nil:
		return []string{r.act.title, render.Dim(r.act.about, t), ""}
	}
	return []string{s.name(r), "", ""}
}

// Name of a row, after the trail of the sections it is under where rows of several sections are listed together
func (s *settings) name(r row) string {
	t := s.kit.Env.Theme
	name := ""
	switch {
	case r.setting != nil:
		name = r.setting.GetTitle()
	case r.section != nil:
		name = r.section.GetTitle()
	case r.advanced != nil:
		name = "Advanced"
	case r.act != nil:
		name = r.act.title
	}
	if len(r.trail) == 0 {
		return name
	}
	sep := " " + t.Glyphs.Forward + " "
	return render.Dim(strings.Join(r.trail, sep)+sep, t) + name
}

// Words a search reads for a row besides the cells it shows: what it does, its key, the variables that set it and its value
func (s *settings) wordsOf(r row) string {
	switch {
	case r.setting != nil:
		words := []string{r.setting.GetDescription(), r.setting.GetHelp(), r.setting.GetKey(), r.setting.GetShown(), strings.Join(r.trail, " ")}
		return strings.Join(append(words, r.setting.GetEnv()...), " ")
	case r.section != nil:
		return r.section.GetDescription() + " " + r.section.GetKey()
	case r.act != nil:
		return r.act.about
	}
	return ""
}

// Mark before a value that was changed from its default
func changedMark(plain bool) string {
	if plain {
		return "*"
	}
	return "●"
}

// Opens the way to change one setting on its own: the setting with the key, or the entries of the table setting a key below it names
func (s *settings) one(key string) tea.Cmd {
	s.direct = true
	return s.read(func() tea.Cmd {
		if setting := s.find(key); setting != nil {
			return s.change(setting)
		}
		for _, setting := range s.all {
			if setting.GetKind() == configv1.Kind_KIND_TABLE && strings.HasPrefix(strings.ToLower(key), setting.GetKey()+".") {
				return s.change(setting)
			}
		}
		return s.kit.WM.Toast("There is no setting named "+key+".", wm.Warn, wm.DefaultToastTTL)
	})
}

func (s *settings) load() tea.Cmd {
	return s.read(func() tea.Cmd {
		target := s.target
		s.target = ""
		if target == "" || s.root == nil {
			return nil
		}
		if section := s.section(target); section != nil {
			var chain []*configv1.Section
			for at := section; at != nil; at = s.section(at.GetParent()) {
				chain = append([]*configv1.Section{at}, chain...)
			}
			var cmds []tea.Cmd
			for _, at := range chain {
				cmds = append(cmds, s.enter(at))
			}
			return tea.Batch(cmds...)
		}
		s.root.Filter(target)
		return nil
	})
}

// Reads every setting again, then runs then
func (s *settings) read(then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := s.service.List(s.kit.Env.Ctx, &configv1.ListRequest{Profile: s.named})
		return func() tea.Cmd {
			s.err, s.loaded = err, true
			if err == nil {
				s.groups, s.sections, s.all, s.profile = response.GetGroups(), response.GetSections(), response.GetSettings(), response.GetProfile()
			}
			for _, view := range s.views {
				view.Refresh()
			}
			if err != nil {
				return s.fail(tui.Reason(err))
			}
			return then()
		}
	})
}

// What enter, reset and reload do on the rows a list shows
func (s *settings) rowActions(shown *[]row) map[string]func(*cards.List, int) tea.Cmd {
	return map[string]func(*cards.List, int) tea.Cmd{
		"hub.use": func(_ *cards.List, i int) tea.Cmd {
			switch r := (*shown)[i]; {
			case r.section != nil:
				return s.enter(r.section)
			case r.advanced != nil:
				return s.advanced(r.advanced)
			case r.act != nil:
				return r.act.run()
			default:
				return s.change(r.setting)
			}
		},
		"hub.remove": func(l *cards.List, i int) tea.Cmd {
			setting := (*shown)[i].setting
			if setting == nil || setting.GetSource() != configv1.Source_SOURCE_FILE {
				return nil
			}
			if setting.GetLocked() != "" {
				return s.fail(locked(setting))
			}
			question := "Reset " + setting.GetTitle() + "?"
			if !setting.GetSecret() {
				question = "Reset " + setting.GetTitle() + " to " + setting.GetDefaultShown() + "?"
			}
			l.Confirm(question, func() tea.Cmd { return s.save(setting, setting.GetKey(), "null", nil, "") })
			return nil
		},
		"hub.refresh": func(*cards.List, int) tea.Cmd { return s.read(func() tea.Cmd { return nil }) },
	}
}

// Buttons for the row under the cursor: what enter does on it, then reset on a value that was changed
func (s *settings) hints(shown []row, i int) []wm.Hint {
	if i < 0 || i >= len(shown) {
		return nil
	}
	r := shown[i]
	switch {
	case r.act != nil:
		return []wm.Hint{{Action: "hub.use", Label: r.act.verb}}
	case r.setting == nil:
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	}
	if r.setting.GetLocked() != "" {
		return []wm.Hint{{Label: "locked"}}
	}
	verb := "change"
	if r.setting.GetKind() == configv1.Kind_KIND_TOGGLE {
		verb = "turn on"
		if on, _ := decode(r.setting.GetValue()).(bool); on {
			verb = "turn off"
		}
	}
	hints := []wm.Hint{{Action: "hub.use", Label: verb}}
	if r.setting.GetSource() == configv1.Source_SOURCE_FILE {
		hints = append(hints, wm.Hint{Action: "hub.remove", Label: "reset"})
	}
	return hints
}

// List of the rows of one section over the hub: what is set there, then name and value of each row, the highlighted one explained under it
func (s *settings) list(title, key, about, search string, rows func(searching bool) []row) *cards.List {
	var shown []row
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  search,
		Intro:   func() []string { return []string{about} },
		Columns: rowColumns,
		Rows: func() [][]string {
			shown = rows(list.Query() != "")
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = s.cells(r)
			}
			return cells
		},
		Words:   func(i int) string { return s.wordsOf(shown[i]) },
		Detail:  func(i int) []string { return s.detail(shown[i], list.Cut(i, 2)) },
		Empty:   func() string { return tui.Waiting(s.loaded, s.err, "No settings to show.") },
		Free:    map[string]bool{"hub.refresh": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return s.hints(shown, i) },
		Actions: s.rowActions(&shown),
	})
	list.ToggleDetail()
	s.views = append(s.views, list)
	return list
}

// Opens a section over the hub: its everyday settings, the sections under it, then its advanced settings behind one row, or in place when
// they are few or all it has. Typing searches the section and everything under it
func (s *settings) enter(section *configv1.Section) tea.Cmd {
	key := section.GetKey()
	return s.hub.Push(s.list(section.GetTitle(), "settings:"+key, section.GetDescription(), "Search in "+section.GetTitle(), func(searching bool) []row {
		if searching {
			return s.found(key)
		}
		everyday, rare := s.own(key)
		rows := everyday
		apart := len(rare) >= 3 && len(everyday) > 0
		if !apart {
			rows = append(rows, rare...)
		}
		rows = append(rows, s.children(key)...)
		if apart {
			rows = append(rows, row{advanced: section})
		}
		return append(rows, s.extras(key)...)
	}))
}

// What a section offers besides its settings, each a row that opens the screen where it is done: connecting a provider among the
// providers, and among the approvals the proposals from approval history and the check of a command
func (s *settings) extras(key string) []row {
	open := func(title, about, screen, argument string) row {
		return row{act: &action{title, about, "open", func() tea.Cmd { return s.kit.Open(screen, argument) }}}
	}
	switch key {
	case providerconfig.Key:
		return []row{open("Connect a provider", "Add a provider, sign in or enter an API key", providersScreen, profile.Join(s.named, ""))}
	case approvalsconfig.Key:
		return []row{open("Suggest commands to allow", "Propose commands from the ones you approved before", allowedScreen, "suggest"),
			open("Check a command", "See what the rules do with a command, without running it", allowedScreen, "check")}
	}
	return nil
}

// Opens the advanced settings of a section over the hub
func (s *settings) advanced(section *configv1.Section) tea.Cmd {
	key := section.GetKey()
	about := "Settings of " + section.GetTitle() + " that few people change"
	return s.hub.Push(s.list("Advanced", "settings:"+key+":advanced", about, "Search in "+section.GetTitle(), func(searching bool) []row {
		if searching {
			return s.found(key)
		}
		_, rare := s.own(key)
		return rare
	}))
}

// Settings of a section a person changes, the everyday ones and those listed as advanced
func (s *settings) own(key string) (everyday, rare []row) {
	for _, setting := range s.all {
		switch {
		case setting.GetSection() != key || !s.visible(setting):
		case setting.GetAdvanced():
			rare = append(rare, row{setting: setting})
		default:
			everyday = append(everyday, row{setting: setting})
		}
	}
	return everyday, rare
}

// Sections under the parent that hold something to change, in the order the server lists them
func (s *settings) children(parent string) []row {
	var rows []row
	for _, section := range s.sections {
		if section.GetParent() == parent && s.holds(section.GetKey()) {
			rows = append(rows, row{section: section})
		}
	}
	return rows
}

// Every section and setting under the base, the whole tree for no base, each with the trail of the sections between the base and it
func (s *settings) found(base string) []row {
	var rows []row
	within := func(key string) ([]string, bool) {
		var trail []string
		for at := s.section(key); at != nil; at = s.section(at.GetParent()) {
			if at.GetKey() == base {
				return trail, true
			}
			trail = append([]string{at.GetTitle()}, trail...)
		}
		return trail, base == ""
	}
	for _, section := range s.sections {
		if trail, ok := within(section.GetParent()); ok && section.GetKey() != base && s.holds(section.GetKey()) {
			rows = append(rows, row{section: section, trail: trail})
		}
	}
	for _, setting := range s.all {
		if trail, ok := within(setting.GetSection()); ok && s.visible(setting) {
			rows = append(rows, row{setting: setting, trail: trail})
		}
	}
	return rows
}

// Whether a setting is one a person changes here, those icebath keeps for itself left out
func (s *settings) visible(setting *configv1.Setting) bool { return !setting.GetInternal() }

// Whether the section or one under it holds a setting to change, or offers something else to do
func (s *settings) holds(key string) bool {
	total, _ := s.count(key)
	return total > 0 || len(s.extras(key)) > 0
}

// Settings of the section and those under it a person changes, and how many of them differ from their defaults
func (s *settings) count(key string) (total, changed int) {
	for _, setting := range s.all {
		if setting.GetSection() == key && s.visible(setting) {
			total++
			if setting.GetSource() != configv1.Source_SOURCE_DEFAULT {
				changed++
			}
		}
	}
	for _, section := range s.sections {
		if section.GetParent() == key {
			inside, differing := s.count(section.GetKey())
			total, changed = total+inside, changed+differing
		}
	}
	return total, changed
}

// Settings of the section and those under it that differ from their defaults
func (s *settings) changed(key string) int {
	_, changed := s.count(key)
	return changed
}

func (s *settings) section(key string) *configv1.Section {
	for _, section := range s.sections {
		if section.GetKey() == key {
			return section
		}
	}
	return nil
}

func (s *settings) find(key string) *configv1.Setting {
	for _, setting := range s.all {
		if setting.GetKey() == key {
			return setting
		}
	}
	return nil
}

// Cells of a row of a section: its name, the mark of a value that was changed or of a row that opens, and its value or what it opens
func (s *settings) cells(r row) []string {
	t := s.kit.Env.Theme
	opens := render.Dim(t.Glyphs.Forward, t)
	switch {
	case r.section != nil:
		return []string{s.name(r), opens, render.Dim(s.inside(r.section.GetKey(), false), t)}
	case r.advanced != nil:
		return []string{s.name(r), opens, render.Dim(s.inside(r.advanced.GetKey(), true), t)}
	case r.act != nil:
		return []string{s.name(r), opens, render.Dim(r.act.about, t)}
	}
	value, _ := s.value(r.setting)
	mark := ""
	if r.setting.GetSource() == configv1.Source_SOURCE_FILE {
		mark = render.Cool(changedMark(t.Plain), t)
	}
	return []string{s.name(r), mark, value}
}

// What a row that opens a section says is inside: how many settings, and how many of them were changed. For the advanced settings of a
// section only those count
func (s *settings) inside(key string, advanced bool) string {
	total, changed := s.count(key)
	if advanced {
		_, rare := s.own(key)
		total, changed = len(rare), 0
		for _, r := range rare {
			if r.setting.GetSource() != configv1.Source_SOURCE_DEFAULT {
				changed++
			}
		}
	}
	said := render.Count(total, "setting")
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// Value of a setting as a row shows it: in the words the server gives, a list cut after its last whole item that fits and any other value
// cut at the room a row has, then where it comes from when that is not this screen. Whether it was cut
func (s *settings) value(setting *configv1.Setting) (string, bool) {
	shown, cut := setting.GetShown(), false
	if items := list(setting.GetValue()); setting.GetKind() == configv1.Kind_KIND_LIST && len(items) > 0 {
		for i := range items {
			items[i] = title(setting, items[i])
		}
		shown, cut = brief(items, valueRoom)
	} else if ansi.StringWidth(shown) > valueRoom {
		shown, cut = ansi.Truncate(shown, valueRoom, s.kit.Env.Theme.Glyphs.Ellipsis), true
	}
	switch {
	case setting.GetLocked() != "" || setting.GetSource() == configv1.Source_SOURCE_MANAGED:
		shown += " (locked)"
	case setting.GetSource() == configv1.Source_SOURCE_ENVIRONMENT:
		shown += " (from " + setting.GetVariable() + ")"
	}
	return shown, cut
}

// Items joined by commas up to the last whole one that fits the room, the rest counted; whether any were left out
func brief(items []string, room int) (string, bool) {
	said := ""
	for i, item := range items {
		next := said
		if i > 0 {
			next += ", "
		}
		next += item
		if rest := len(items) - i - 1; ansi.StringWidth(next)+len(more(rest)) > room && i > 0 {
			return said + more(len(items)-i), true
		}
		said = next
	}
	return said, false
}

// Count of the items a list left out, nothing for none
func more(n int) string {
	if n <= 0 {
		return ""
	}
	return " +" + strconv.Itoa(n) + " more"
}

// Why a setting can't be changed here, as the refusal says it
func locked(setting *configv1.Setting) string {
	return setting.GetTitle() + " can't be changed here. " + setting.GetLocked()
}

// When a change of the setting takes hold, nothing for one that does at once or says nothing of it
func applies(setting *configv1.Setting) string {
	switch setting.GetApply() {
	case configv1.Apply_APPLY_NEXT_MESSAGE:
		return "Takes effect from your next message"
	case configv1.Apply_APPLY_NEW_SESSIONS:
		return "Takes effect in new sessions"
	case configv1.Apply_APPLY_RESTART:
		return "Takes effect after icebath restarts"
	}
	return ""
}

// Lines under the list about the highlighted row. A setting: what it does, then its default, bounds, when a change takes hold and the
// command that changes it too, then where its value comes from when that is not the default, whether every profile shares it, its key and
// variables, and its whole value when the row cut it. Any other row: what it opens
func (s *settings) detail(r row, cut bool) []string {
	t := s.kit.Env.Theme
	switch {
	case r.section != nil:
		return []string{r.section.GetDescription()}
	case r.advanced != nil:
		return []string{"Settings of " + r.advanced.GetTitle() + " that few people change"}
	case r.act != nil:
		return []string{r.act.about}
	}
	setting := r.setting
	lines := []string{setting.GetDescription()}
	if help := setting.GetHelp(); help != "" {
		lines = append(lines, help)
	}
	if skill := setting.GetSkill(); skill != "" {
		lines = append(lines, "The "+skill+" skill asks for this value.")
	}
	var facts []string
	if !setting.GetSecret() && setting.GetDefaultShown() != "" {
		facts = append(facts, "Default: "+setting.GetDefaultShown())
	}
	facts = append(facts, setting.GetBounds(), applies(setting))
	if slash := setting.GetSlash(); slash != "" {
		facts = append(facts, "Also /"+slash)
	}
	if facts = slices.DeleteFunc(facts, func(fact string) bool { return fact == "" }); len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	switch {
	case setting.GetLocked() != "":
		lines = append(lines, render.Warm("Locked. "+setting.GetLocked(), t))
	case setting.GetSource() == configv1.Source_SOURCE_MANAGED:
		lines = append(lines, render.Warm("Locked. Your administrator sets this.", t))
	case setting.GetSource() == configv1.Source_SOURCE_ENVIRONMENT:
		lines = append(lines, render.Warm("Set by the environment variable "+setting.GetVariable()+", which wins over this screen.", t))
	case setting.GetSource() == configv1.Source_SOURCE_FILE:
		lines = append(lines, render.Cool("You changed this. "+s.kit.Env.Keys.Display("hub.remove")+" puts the default back.", t))
	}
	if s.global(setting.GetSection()) {
		lines = append(lines, render.Dim("Shared by every profile.", t))
	}
	where := "Config key: " + setting.GetKey()
	switch variables := setting.GetEnv(); len(variables) {
	case 0:
	case 1:
		where += " · Variable: " + variables[0]
	default:
		where += " · Variables: " + strings.Join(variables, ", ")
	}
	lines = append(lines, render.Dim(where, t))
	// The value whole, where its row could not show it so
	if _, long := s.value(setting); long || cut {
		lines = append(lines, render.Dim("Value: "+setting.GetShown(), t))
	}
	return lines
}

// Whether the settings of the section are kept once for every profile
func (s *settings) global(key string) bool {
	for at := s.section(key); at != nil; at = s.section(at.GetParent()) {
		if at.GetGlobal() {
			return true
		}
	}
	return false
}

// Changes the setting the way its kind takes: a switch flips at once, a choice opens its choices, a list opens what it holds, a table opens
// its entries, and the rest open a field to type into. One the administrator, Remote Config or a package manager decides is refused with why
func (s *settings) change(setting *configv1.Setting) tea.Cmd {
	if setting.GetLocked() != "" {
		return s.fail(locked(setting))
	}
	switch setting.GetKind() {
	case configv1.Kind_KIND_TOGGLE:
		on, _ := decode(setting.GetValue()).(bool)
		return s.save(setting, setting.GetKey(), strconv.FormatBool(!on), nil, "")
	case configv1.Kind_KIND_CHOICE:
		return s.show(s.choices(setting))
	case configv1.Kind_KIND_LIST:
		switch {
		case recorded(setting):
			return s.show(s.records(setting.GetKey(), setting.GetTitle(), setting.GetDescription(), setting.GetEntry(), func() []any {
				held, _ := decode(s.current(setting).GetValue()).([]any)
				return held
			}, func(next []any, from wm.Card, told string) tea.Cmd {
				encoded, err := encode(append([]any{}, next...))
				if err != nil {
					return s.fail(err.Error())
				}
				return s.save(setting, setting.GetKey(), encoded, from, told)
			}))
		case len(setting.GetOptions()) > 0:
			return s.show(s.checks(setting))
		}
		return s.show(s.items(setting))
	case configv1.Kind_KIND_TABLE:
		return s.show(s.entries(setting, ""))
	}
	return s.show(s.field(setting))
}

// Shows a view to change a setting over the hub, or as a hub of its own when the setting was opened alone
func (s *settings) show(view wm.Card) tea.Cmd {
	if s.hub == nil {
		s.hub = s.kit.Hub(Edit, view)
		return s.kit.WM.ShowOverlay(s.hub)
	}
	return s.hub.Push(view)
}

// Setting as last read, the one given when it was not read since
func (s *settings) current(setting *configv1.Setting) *configv1.Setting {
	if now := s.find(setting.GetKey()); now != nil {
		return now
	}
	return setting
}

// Choices of a setting, the one in use marked on every row: what each means beside it, or under the list where the width can't hold every
// meaning whole
func (s *settings) choices(setting *configv1.Setting) *cards.List {
	t := s.kit.Env.Theme
	held := func() string { return text(s.current(setting).GetValue()) }
	options := setting.GetOptions()
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     len(options) > 8,
		Search:  "Search " + strings.ToLower(setting.GetTitle()),
		Intro:   func() []string { return []string{setting.GetDescription()} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(options))
			for i, option := range options {
				rows[i] = []string{s.kit.Mark(true, option.GetValue() == held()), title(setting, option.GetValue()), render.Dim(option.GetDescription(), t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if list.Shows(2) || options[i].GetDescription() == "" {
				return nil
			}
			return []string{options[i].GetDescription()}
		},
		Empty: func() string { return "Nothing to choose from." },
		Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "choose"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return s.save(setting, setting.GetKey(), strconv.Quote(options[i].GetValue()), list, "")
			},
		},
	})
	list.ToggleDetail()
	list.Refresh()
	list.Select(slices.IndexFunc(options, func(option *configv1.Option) bool { return option.GetValue() == held() }))
	return list
}

// Values a list setting may hold, each turned on or off in place and saved as it is
func (s *settings) checks(setting *configv1.Setting) *cards.List {
	t := s.kit.Env.Theme
	held := func() []string { return list(s.current(setting).GetValue()) }
	options := setting.GetOptions()
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     len(options) > 8,
		Search:  "Search " + strings.ToLower(setting.GetTitle()),
		Intro:   func() []string { return []string{setting.GetDescription()} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			on := held()
			rows := make([][]string, len(options))
			for i, option := range options {
				box := "[ ]"
				if slices.Contains(on, option.GetValue()) {
					box = "[x]"
				}
				rows[i] = []string{box, title(setting, option.GetValue()), render.Dim(option.GetDescription(), t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if view.Shows(2) || options[i].GetDescription() == "" {
				return nil
			}
			return []string{options[i].GetDescription()}
		},
		Empty: func() string { return "Nothing to choose from." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i >= 0 && slices.Contains(held(), options[i].GetValue()) {
				return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				value, name := options[i].GetValue(), title(setting, options[i].GetValue())
				if slices.Contains(held(), value) {
					return s.store(setting, slices.DeleteFunc(held(), func(have string) bool { return have == value }), name+" is now off.")
				}
				return s.store(setting, append(held(), value), name+" is now on.")
			},
		},
	})
	view.ToggleDetail()
	s.views = append(s.views, view)
	return view
}

// Items a list setting holds, each changed, removed or added by typing it
func (s *settings) items(setting *configv1.Setting) *cards.List {
	held := func() []string { return list(s.current(setting).GetValue()) }
	ask := func(heading, was, verb string, done func(item string) tea.Cmd) tea.Cmd {
		return s.kit.Ask(s.hub, heading, cards.Field{Label: "Item", Value: was, Help: setting.GetHelp()}, verb, func(item string) tea.Cmd {
			if item == "" || item == was {
				return nil
			}
			if slices.Contains(held(), item) {
				return s.hub.Say(item+" is already in the list.", cards.NoteInfo)
			}
			return done(item)
		})
	}
	var shown []string
	view := cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search " + strings.ToLower(setting.GetTitle()),
		Intro:   func() []string { return []string{setting.GetDescription()} },
		Columns: []render.Column{{Flex: 1}},
		Rows: func() [][]string {
			shown = held()
			rows := make([][]string, len(shown))
			for i, item := range shown {
				rows[i] = []string{item}
			}
			return rows
		},
		Empty: func() string { return "Nothing here yet. " + s.kit.Env.Keys.Display("hub.add") + " adds one." },
		Free:  map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				was := shown[i]
				return ask("Change item", was, "Save", func(item string) tea.Cmd {
					items := held()
					if at := slices.Index(items, was); at >= 0 {
						items[at] = item
					}
					return s.store(setting, items, "Changed "+was+" to "+item+".")
				})
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				return ask("Add to "+strings.ToLower(setting.GetTitle()), "", "Add", func(item string) tea.Cmd {
					return s.store(setting, append(held(), item), "Added "+item+".")
				})
			},
			"hub.remove": func(_ *cards.List, i int) tea.Cmd {
				item := shown[i]
				return s.store(setting, slices.DeleteFunc(held(), func(have string) bool { return have == item }), "Removed "+item+".")
			},
		},
	})
	s.views = append(s.views, view)
	return view
}

// Saves the items of a list setting, saying what changed. A setting that names its values keeps only those it names now, so an item gone
// since it was added, such as a provider removed from config.yaml, can't fail the save
func (s *settings) store(setting *configv1.Setting, items []string, told string) tea.Cmd {
	if options := setting.GetOptions(); len(options) > 0 {
		items = slices.DeleteFunc(slices.Clone(items), func(item string) bool {
			return !slices.ContainsFunc(options, func(option *configv1.Option) bool { return option.GetValue() == item })
		})
	}
	value, err := encode(append([]string{}, items...))
	if err != nil {
		return s.fail(err.Error())
	}
	return s.save(setting, setting.GetKey(), value, nil, told)
}

// Title of the choice of the setting with the value, the value itself when no choice has it
func title(setting *configv1.Setting, value string) string {
	for _, option := range setting.GetOptions() {
		if option.GetValue() == value && option.GetTitle() != "" {
			return option.GetTitle()
		}
	}
	return value
}

// What typing a value for the setting is told under its field: what its help says, what an empty or zero value means, its bounds, what
// it counts and how a length of time is written
func guide(setting *configv1.Setting) string {
	var lines []string
	if help := setting.GetHelp(); help != "" {
		lines = append(lines, help)
	}
	kind := setting.GetKind()
	switch zero := setting.GetZero(); {
	case setting.GetUnset() != "":
		lines = append(lines, "Leave empty for: "+capital(setting.GetUnset())+".")
	case zero == "":
	case kind == configv1.Kind_KIND_NUMBER || kind == configv1.Kind_KIND_DURATION:
		lines = append(lines, text(setting.GetZeroValue())+" means: "+zero+".")
	default:
		lines = append(lines, "Leave empty for: "+zero+".")
	}
	if kind == configv1.Kind_KIND_DURATION && setting.GetUnset() != "" && setting.GetZero() != "" {
		lines = append(lines, "0 means: "+setting.GetZero()+".")
	}
	if bounds := setting.GetBounds(); bounds != "" {
		lines = append(lines, bounds+".")
	}
	if starts := setting.GetStarts(); len(starts) > 0 {
		lines = append(lines, "It starts with "+strings.Join(starts, " or ")+".")
	}
	switch {
	case setting.GetSecret():
		lines = append(lines, "The value kept now is never shown.")
	case kind == configv1.Kind_KIND_DURATION:
		lines = append(lines, "A length of time such as 30s, 5m or 2h. A number alone counts "+unitWords[setting.GetUnit()]+".")
	case kind == configv1.Kind_KIND_NUMBER && config.Counting(setting.GetCounts()) != "":
		lines = append(lines, config.Counting(setting.GetCounts()))
	}
	return strings.Join(lines, "\n")
}

// Units a length of time is kept in, as the server names them: what a bare number counts, and how long one of them is
var (
	unitWords   = map[string]string{"ms": "milliseconds", "s": "seconds", "min": "minutes", "h": "hours", "days": "days"}
	unitLengths = map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "min": time.Minute, "h": time.Hour, "days": 24 * time.Hour}
)

// Words of the screen's own with a lower case first letter, as they read inside a sentence. Never for words a setting declares, which
// may start with a name
func lower(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToLower(text[:1]) + text[1:]
}

// Text with a capital first letter, as it reads after a colon
func capital(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// What is wrong with text typed for a number or a length of time before it is sent, empty when it may be: not a number, or outside the
// bounds of the setting unless it is the value its zero words speak of
func check(setting *configv1.Setting, typed string) string {
	typed = strings.TrimSpace(typed)
	var number float64
	switch setting.GetKind() {
	case configv1.Kind_KIND_NUMBER:
		n, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return "Enter a number."
		}
		number = n
	case configv1.Kind_KIND_DURATION:
		if typed == "" {
			if setting.GetUnset() == "" {
				return "Enter a length of time such as 30s or 5m."
			}
			return ""
		}
		length, ok := config.Length(typed, unitLengths[setting.GetUnit()])
		if !ok {
			return "Enter a number, or a length of time such as 30s or 5m."
		}
		number = float64(length) / float64(unitLengths[setting.GetUnit()])
	default:
		// Text that must start a certain way, such as an address
		if starts := setting.GetStarts(); len(starts) > 0 && typed != "" &&
			!slices.ContainsFunc(starts, func(start string) bool { return strings.HasPrefix(strings.ToLower(typed), strings.ToLower(start)) }) {
			return "Must start with " + strings.Join(starts, " or ") + "."
		}
		return ""
	}
	if zero, ok := decode(setting.GetZeroValue()).(int); ok && setting.GetZero() != "" && number == float64(zero) {
		return ""
	}
	least, low := bound(setting.GetLeast())
	most, high := bound(setting.GetMost())
	if (low && number < least) || (high && number > most) {
		return "Must be " + lower(setting.GetBounds()) + "."
	}
	// A number goes under 0 only where its setting says so: by a least bound, a default or a value of its own under 0
	if number < 0 && !low && !under(setting.GetDefaultValue()) && !under(setting.GetZeroValue()) {
		return "This can't be less than 0."
	}
	return ""
}

// Whether a value a setting carries is a number under 0
func under(value string) bool {
	switch n := decode(value).(type) {
	case int:
		return n < 0
	case float64:
		return n < 0
	}
	return false
}

// Length of time as a person types it, from the number of its unit a setting keeps: 1h for 3600 seconds, 1.5s, 500ms. A count of days,
// which no such spelling has, and anything that is not a number stay as they are
func length(kept, unit string) string {
	n, err := strconv.ParseFloat(kept, 64)
	if err != nil || unit == "days" || n == 0 {
		return kept
	}
	spelled := time.Duration(n * float64(unitLengths[unit])).String()
	for _, zero := range []string{"0s", "0m"} {
		if trimmed := strings.TrimSuffix(spelled, zero); trimmed != spelled && trimmed != "" && !strings.HasSuffix(trimmed, ".") &&
			(strings.HasSuffix(trimmed, "m") || strings.HasSuffix(trimmed, "h")) {
			spelled = trimmed
		}
	}
	return spelled
}

// Number of a bound the server gives as text, false for none
func bound(text string) (float64, bool) {
	if text == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(text, 64)
	return n, err == nil
}

// Field to type the value of a setting into: text, a number, a length of time or a secret, told what an empty value means, the bounds and
// how to write it, and refused under the field when it can't be right
func (s *settings) field(setting *configv1.Setting) *cards.Form {
	field := cards.Field{ID: "value", Label: setting.GetTitle(), Value: text(setting.GetValue()), Help: guide(setting), Select: true,
		Placeholder: cmp.Or(capital(setting.GetUnset()), setting.GetZero()), Check: func(typed string) string { return check(setting, typed) }}
	switch {
	case setting.GetSecret():
		field.Kind, field.Value, field.Placeholder = cards.FieldSecret, "", ""
	case setting.GetKind() == configv1.Kind_KIND_NUMBER:
		field.Kind = cards.FieldNumber
	case setting.GetKind() == configv1.Kind_KIND_DURATION:
		field.Value = length(field.Value, setting.GetUnit())
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:   setting.GetTitle(),
		Intro:   []string{setting.GetDescription()},
		Fields:  []cards.Field{field},
		Submit:  "Save",
		Context: fieldContext,
		Hints:   []wm.Hint{{Action: "settings.default"}},
		Actions: map[string]func(*cards.Form) tea.Cmd{
			"settings.default": func(*cards.Form) tea.Cmd { return s.save(setting, setting.GetKey(), "null", form, "") },
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			typed := strings.TrimSpace(values["value"])
			value := strconv.Quote(typed)
			switch {
			case setting.GetSecret() && typed == "":
				return s.hub.Close(form)
			case setting.GetKind() == configv1.Kind_KIND_DURATION && typed == "":
				value = "null"
			case setting.GetKind() == configv1.Kind_KIND_NUMBER && !setting.GetSecret():
				value = typed
			}
			return s.save(setting, setting.GetKey(), value, form, "")
		},
	})
	s.typed = form
	return form
}

// Saves a value written in YAML under the key, the setting's own or one of its entries, null putting the default back, and tells the app
// the value the setting holds now. The view it came from closes, and a setting opened alone closes its hub with it. The note says what
// changed, told when given and else the new value, and when the change takes hold. A refusal shows under the field of the form it came from
func (s *settings) save(setting *configv1.Setting, key, value string, from wm.Card, told string) tea.Cmd {
	if s.hub != nil && s.hub.Shown() {
		s.hub.Busy("Saving…")
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := s.kit.Env.Ctx
		_, err := s.service.Set(ctx, &configv1.SetRequest{Profile: s.named, Key: key, Value: value})
		var now *configv1.Setting
		if err == nil {
			var response *configv1.GetResponse
			if response, err = s.service.Get(ctx, &configv1.GetRequest{Profile: s.named, Key: setting.GetKey()}); err == nil {
				now = response.GetSetting()
			}
		}
		return func() tea.Cmd {
			if err != nil {
				if form, ok := from.(*cards.Form); ok && form == s.typed && s.hub != nil && s.hub.Shown() {
					s.hub.Quiet()
					form.SetError("value", tui.Reason(err))
					return nil
				}
				return s.fail(tui.Reason(err))
			}
			for i, have := range s.all {
				if have.GetKey() == now.GetKey() {
					s.all[i] = now
				}
			}
			for _, view := range s.views {
				view.Refresh()
			}
			if told == "" {
				told = now.GetTitle() + " is now " + now.GetShown() + "."
			}
			if when := applies(now); when != "" {
				told += " " + when + "."
			}
			switch now.GetSource() {
			case configv1.Source_SOURCE_ENVIRONMENT:
				told += " The environment variable " + now.GetVariable() + " still wins over it."
			case configv1.Source_SOURCE_MANAGED:
				told += " Your administrator's value still wins over it."
			}
			var cmds []tea.Cmd
			// Screens follow the settings of the profile in use, so a change to another profile's is theirs to hear only when every profile shares it
			if s.named == "" || s.global(now.GetSection()) {
				cmds = append(cmds, s.kit.Changed(now.GetKey(), now.GetValue()))
			}
			switch {
			case s.direct && from != nil && key == setting.GetKey():
				cmds = append(cmds, s.kit.WM.DismissCard(s.hub), s.kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL))
			case s.hub == nil || !s.hub.Shown():
				cmds = append(cmds, s.kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL))
			default:
				if from != nil {
					cmds = append(cmds, s.hub.Close(from))
				}
				cmds = append(cmds, s.hub.Done(told))
			}
			return tea.Batch(cmds...)
		}
	})
}

// Says why something failed under the hub, or in a toast while none shows
func (s *settings) fail(text string) tea.Cmd {
	if s.hub != nil && s.hub.Shown() {
		return s.hub.Fail(text)
	}
	return s.kit.WM.Toast(text, wm.Error, wm.DefaultToastTTL)
}

// Files the settings and keys are kept in: where they are and whether they exist, each opened in the editor, the settings file checked
// after an edit and the keys counted again, and their paths copied
func (s *settings) files() tea.Cmd {
	if s.hub == nil {
		return nil
	}
	var files *configv1.FilesResponse
	var acts []action
	where := func(path string, exists bool) string {
		if exists {
			return path
		}
		return path + " (not made yet)"
	}
	detail := func() []string {
		lines := []string{"Every change on these screens is saved in these files. You can also edit them by hand: icebath reads a change the next time it needs the setting.", ""}
		if own := files.GetOwn(); own != "" {
			lines = append(lines, "Settings file: "+where(own, files.GetOwnExists()))
		}
		lines = append(lines, "Keys file: "+where(files.GetEnv(), files.GetEnvExists()))
		switch {
		case files.GetManagedSystem() != "":
			lines = append(lines, "", files.GetManagedSystem()+" manages this install, so its files are not edited here.")
		case files.GetOwn() == "":
			lines = append(lines, "", "This profile keeps its settings in a store that has no file, so there is nothing to edit by hand.")
		}
		return lines
	}
	var view *cards.List
	reread := func(then func() tea.Cmd) tea.Cmd {
		return tui.Call(s.hub, func() (*configv1.FilesResponse, error) {
			return s.service.Files(s.kit.Env.Ctx, &configv1.FilesRequest{Profile: s.named})
		}, func(response *configv1.FilesResponse) tea.Cmd {
			files = response
			acts = s.fileActions(files)
			if view != nil {
				view.Refresh()
			}
			return then()
		})
	}
	return reread(func() tea.Cmd {
		t := s.kit.Env.Theme
		view = cards.NewList(s.kit.Env, cards.ListSpec{
			Title:   "Settings files",
			Glyph:   t.Glyphs.Settings,
			Key:     "settings:files",
			Context: cards.HubContext,
			Primary: "hub.use",
			Columns: []render.Column{{}, {Flex: 1, Whole: true}},
			Rows: func() [][]string {
				rows := make([][]string, len(acts))
				for i, act := range acts {
					rows[i] = []string{act.title, render.Dim(act.about, t)}
				}
				return rows
			},
			Detail: func(i int) []string {
				if view.Shows(1) {
					return detail()
				}
				return append([]string{acts[i].about, ""}, detail()...)
			},
			Hints: func(_ *cards.List, i int) []wm.Hint {
				if i < 0 {
					return nil
				}
				return []wm.Hint{{Action: "hub.use", Label: acts[i].verb}}
			},
			Actions: map[string]func(*cards.List, int) tea.Cmd{
				"hub.use":     func(_ *cards.List, i int) tea.Cmd { return acts[i].run() },
				"hub.refresh": func(*cards.List, int) tea.Cmd { return reread(func() tea.Cmd { return nil }) },
			},
			Free: map[string]bool{"hub.refresh": true},
		})
		view.ToggleDetail()
		return s.hub.Push(view)
	})
}

// What the files view offers for the files as they are: editing the settings file and the keys file, there yet or not, unless a package
// manager keeps them or the profile keeps no file, checking the settings file once it is there, and copying both paths
func (s *settings) fileActions(files *configv1.FilesResponse) []action {
	var out []action
	own := files.GetOwn()
	if files.GetManagedSystem() == "" && own != "" {
		out = append(out, action{"Edit the settings file", "Open config.yaml in your editor, then check it", "edit", func() tea.Cmd { return s.editSettings(files) }})
		// Offered before the file is there too: the first key a person keeps by hand makes it
		if s.local(files, files.GetEnv()) {
			out = append(out, action{"Edit the keys file", "Open .env, where your API keys are kept, in your editor", "edit", func() tea.Cmd { return s.editKeys(files.GetEnv()) }})
		}
	}
	if own != "" && files.GetOwnExists() {
		out = append(out, action{"Check the settings file", "Look for mistakes and for keys that are missing", "check", s.check})
	}
	if own != "" {
		out = append(out, action{"Copy the path of the settings file", "Put where config.yaml is on the clipboard", "copy", func() tea.Cmd { return s.copy(own, "settings file") }})
	}
	return append(out, action{"Copy the path of the keys file", "Put where .env is on the clipboard", "copy", func() tea.Cmd { return s.copy(files.GetEnv(), "keys file") }})
}

// Whether the file is on this machine, where the editor runs, rather than on the machine of a server the app reaches
func (s *settings) local(files *configv1.FilesResponse, path string) bool {
	host, _ := os.Hostname()
	info, err := os.Stat(filepath.Dir(path))
	return host == files.GetHost() && err == nil && info.IsDir()
}

func (s *settings) copy(path, what string) tea.Cmd {
	return tea.Batch(clip.Copy(path), s.hub.Done("Copied the path of the "+what+"."))
}

// Runs the editor on the file with the terminal handed over to it, then runs then once it is back
func (s *settings) edit(path string, then func() tea.Cmd) tea.Cmd {
	editor := config.Editor()
	if editor == "" {
		return s.hub.Fail("No editor was found. Set EDITOR, or open the file yourself: " + path)
	}
	// An editor that ends with a status of its own still ran; the answer comes back as the message the callback returns
	return tea.ExecProcess(config.EditorCommand(s.kit.Env.Ctx, editor, path), func(err error) tea.Msg {
		return tui.Async(func() func() tea.Cmd {
			return func() tea.Cmd {
				if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
					return s.hub.Fail("The editor did not start: " + err.Error())
				}
				return then()
			}
		})()
	})
}

// Opens the profile's config.yaml in the editor, made first from the example when it is not there, then checks it: in place on this
// machine, else a copy saved back through the server
func (s *settings) editSettings(files *configv1.FilesResponse) tea.Cmd {
	path := files.GetOwn()
	open := func(made bool) tea.Cmd {
		note := func() tea.Cmd { return nil }
		if made {
			note = func() tea.Cmd { return s.hub.Done("Made " + path + " from the example.") }
		}
		if s.local(files, path) {
			return tea.Batch(note(), s.edit(path, s.check))
		}
		return tui.Call(s.hub, func() (*configv1.ReadResponse, error) {
			return s.service.Read(s.kit.Env.Ctx, &configv1.ReadRequest{Path: path, Profile: s.named})
		}, func(read *configv1.ReadResponse) tea.Cmd {
			temp, err := os.MkdirTemp("", "icebath-config-")
			if err != nil {
				return s.hub.Fail(err.Error())
			}
			copied := filepath.Join(temp, filepath.Base(path))
			if err := os.WriteFile(copied, []byte(read.GetText()), 0o600); err != nil {
				os.RemoveAll(temp)
				return s.hub.Fail(err.Error())
			}
			return tea.Batch(note(), s.edit(copied, func() tea.Cmd {
				edited, err := os.ReadFile(copied)
				os.RemoveAll(temp)
				if err != nil {
					return s.hub.Fail(err.Error())
				}
				if string(edited) == read.GetText() {
					return s.check()
				}
				return tui.Call(s.hub, func() (*configv1.WriteResponse, error) {
					return s.service.Write(s.kit.Env.Ctx, &configv1.WriteRequest{Path: path, Text: string(edited), Profile: s.named})
				}, func(*configv1.WriteResponse) tea.Cmd { return s.check() })
			}))
		})
	}
	if files.GetOwnExists() {
		return open(false)
	}
	return tui.Call(s.hub, func() (*configv1.SeedResponse, error) {
		return s.service.Seed(s.kit.Env.Ctx, &configv1.SeedRequest{Path: path, Profile: s.named})
	}, func(seeded *configv1.SeedResponse) tea.Cmd { return open(seeded.GetSeeded()) })
}

// Opens the profile's .env in the editor, then says how many of its variables changed, which icebath reads at their next use
func (s *settings) editKeys(path string) tea.Cmd {
	before, err := config.ReadDotenv(path)
	if err != nil {
		return s.hub.Fail(err.Error())
	}
	// A keys file that is not there yet is made empty and readable by its owner alone, so the editor does not make one others can read
	if file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		file.Close()
	} else {
		return s.hub.Fail("Could not make the keys file. " + capital(strings.TrimSuffix(err.Error(), ".")) + ".")
	}
	return s.edit(path, func() tea.Cmd {
		after, err := config.ReadDotenv(path)
		if err != nil {
			return s.hub.Fail(err.Error())
		}
		updated := 0
		for name, value := range after {
			if old, ok := before[name]; !ok || old != value {
				updated++
			}
		}
		for name := range before {
			if _, ok := after[name]; !ok {
				updated++
			}
		}
		if updated == 0 {
			return s.hub.Done("Read the keys file again. Nothing in it changed.")
		}
		return s.hub.Done("Read the keys file again. " + render.Count(updated, "key") + " changed.")
	})
}

// Checks the settings file and the keys the settings read, the report shown over the hub: what is wrong in the file first, then the keys
// that are missing, those that are set, and last the ones that are not, counted before they are named
func (s *settings) check() tea.Cmd {
	return tui.Call(s.hub, func() (*configv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &configv1.CheckRequest{Profile: s.named})
	}, func(report *configv1.CheckResponse) tea.Cmd {
		t := s.kit.Env.Theme
		var lines []string
		part := func(heading string, body []string) {
			if len(body) == 0 {
				return
			}
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(append(lines, render.Heading(heading, t)), body...)
		}
		if problems := report.GetDiagnostics(); len(problems) > 0 {
			var body []string
			for _, problem := range problems {
				body = append(body, render.Warm("  "+t.Glyphs.Warn+" "+problem, t))
			}
			part("Problems in the settings file", body)
		} else {
			lines = append(lines, render.Good("No problems found in config.yaml.", t))
		}
		var missing, set, unset []string
		named := func(variable *configv1.Variable) string {
			line := "  " + variable.GetName()
			if about := variable.GetAbout(); about != "" {
				line += render.Dim("  "+about, t)
			}
			return line
		}
		for _, variable := range report.GetRequired() {
			if variable.GetSet() {
				set = append(set, named(variable))
			} else {
				missing = append(missing, render.Warm(named(variable), t))
			}
		}
		for _, variable := range report.GetOptional() {
			if variable.GetSet() {
				set = append(set, named(variable))
			} else {
				unset = append(unset, render.Dim("  "+variable.GetName(), t))
			}
		}
		part("Keys that are needed and missing", missing)
		part("Keys that are set", set)
		if len(unset) > 0 {
			part(render.Count(len(unset), "key")+" that can be set and are not", unset)
		}
		return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: "Check", Key: "settings:check",
			Lines: func(int) []string { return lines }}))
	})
}

func decode(value string) any {
	var decoded any
	if yaml.Unmarshal([]byte(value), &decoded) != nil {
		return nil
	}
	return decoded
}

// Value as text, a scalar written out and anything else empty
func text(value string) string {
	switch decoded := decode(value).(type) {
	case nil:
		return ""
	case string:
		return decoded
	case int, int64, float64, bool:
		return fmt.Sprint(decoded)
	}
	return ""
}

// Value as a list of text
func list(value string) []string {
	items, _ := decode(value).([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// Value as one line of YAML
func encode(value any) (string, error) {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return "", err
	}
	node.Style = yaml.FlowStyle
	out, err := yaml.Marshal(&node)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
