package app

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
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
	// Screen of every setting, opened on a section by its key or filtered by any other words
	Settings = "settings"
	// Screen that changes one setting, opened with its key, or one entry of a table setting with the entry's key
	Edit = "setting"
)

// Keymap context of the lists of settings
const settingsContext = "settings.rows"

var actions = []keys.Action{
	{ID: "settings.files", Context: settingsContext, Label: "files", Doc: "show where settings and keys are kept and open them in your editor", Default: []string{"f"}},
	{ID: "settings.suggest", Context: settingsContext, Label: "suggest", Doc: "propose allowed commands from the risky commands you approved before", Default: []string{"s"}},
	{ID: "settings.check", Context: settingsContext, Label: "check a command", Doc: "show what the approval rules do with a command, without running it", Default: []string{"c"}},
}

// Screen of the allowed commands, where the proposals and the check of a command the settings of approvals offer are made
const allowedScreen = "approvals"

// What a secret shows once it is set
const hidden = "••••••••"

// Sections under another its detail names before it counts the rest
const sectionsNamed = 6

// Marks of where a setting's value comes from, as the legend of every list says them
const legend = "● yours  $ environment  🔒 locked"

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{
		{
			ID:      Settings,
			Title:   "Settings",
			Short:   "every setting, by group, with what it does",
			Slash:   []string{"settings", "config", "preferences"},
			Usage:   "[@profile] [section|search]",
			Actions: actions,
			Where:   map[string]string{settingsContext: "settings"},
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
}

// Row of a list: a section to open, or a setting to change
type row struct {
	section *configv1.Section
	setting *configv1.Setting
}

func (s *settings) open(target string) tea.Cmd {
	s.target = target
	heading := "Settings"
	if s.named != "" {
		heading = "Settings of " + s.named
	}
	s.root = s.list(heading, "settings", func() []row {
		if s.root.Query() == "" {
			return s.children("")
		}
		var rows []row
		for _, section := range s.sections {
			if s.holds(section.GetKey()) {
				rows = append(rows, row{section: section})
			}
		}
		for _, setting := range s.all {
			if s.visible(setting) {
				rows = append(rows, row{setting: setting})
			}
		}
		return rows
	})
	s.hub = s.kit.Hub(Settings, s.root)
	return tea.Batch(s.kit.WM.ShowOverlay(s.hub), s.load())
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
		return s.kit.WM.Toast("No setting named "+key, wm.Warn, wm.DefaultToastTTL)
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
				s.sections, s.all, s.profile = response.GetSections(), response.GetSettings(), response.GetProfile()
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

// List of rows over the hub, the highlighted row explained under it
func (s *settings) list(title, key string, rows func() []row) *cards.List {
	var shown []row
	var list *cards.List
	// The settings of approvals offer the proposals from approval history and the check of a command
	approvals := key == "settings:"+approvalsconfig.Key
	extra := []wm.Hint{{Action: "settings.files"}}
	if approvals {
		extra = append(extra, wm.Hint{Action: "settings.suggest"}, wm.Hint{Action: "settings.check"})
	}
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     key,
		Context: settingsContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Search:  "Search settings",
		Columns: []render.Column{{Title: "setting", Max: 48}, {Title: "value", Max: 28}, {Title: ""}, {Title: "what it does", Flex: 1}},
		Yield:   []int{3},
		Rows: func() [][]string {
			shown = rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = s.cells(r, list == s.root && list.Query() != "")
			}
			return cells
		},
		Detail: func(i int) []string { return s.detail(shown[i]) },
		Status: func() string { return legend },
		Empty: func() string {
			if list == s.root && list.Query() != "" && s.loaded && s.err == nil {
				return "No setting matches those words."
			}
			return tui.Waiting(s.loaded, s.err, "No settings here")
		},
		Free: map[string]bool{"hub.refresh": true, "settings.files": true, "settings.suggest": approvals, "settings.check": approvals},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return extra
			}
			return append(s.hints(shown[i]), extra...)
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if r := shown[i]; r.section != nil {
					return s.enter(r.section)
				}
				return s.change(shown[i].setting)
			},
			"hub.reset": func(l *cards.List, i int) tea.Cmd {
				setting := shown[i].setting
				if setting == nil || setting.GetSource() != configv1.Source_SOURCE_FILE {
					return nil
				}
				if setting.GetLocked() != "" {
					return s.fail(locked(setting))
				}
				l.Confirm("Put "+setting.GetTitle()+" back to "+s.words(setting, setting.GetDefaultValue())+"?", func() tea.Cmd {
					return s.save(setting, setting.GetKey(), "null", nil, "")
				})
				return nil
			},
			"hub.refresh":    func(*cards.List, int) tea.Cmd { return s.read(func() tea.Cmd { return nil }) },
			"settings.files": func(*cards.List, int) tea.Cmd { return s.files() },
			"settings.suggest": func(*cards.List, int) tea.Cmd {
				if !approvals {
					return nil
				}
				return s.kit.Open(allowedScreen, "suggest")
			},
			"settings.check": func(*cards.List, int) tea.Cmd {
				if !approvals {
					return nil
				}
				return s.kit.Open(allowedScreen, "check")
			},
		},
	})
	list.ToggleDetail()
	s.views = append(s.views, list)
	return list
}

// Opens a section over the hub
func (s *settings) enter(section *configv1.Section) tea.Cmd {
	key := section.GetKey()
	return s.hub.Push(s.list(section.GetTitle(), "settings:"+key, func() []row {
		var rows []row
		for _, setting := range s.all {
			if setting.GetSection() == key && s.visible(setting) {
				rows = append(rows, row{setting: setting})
			}
		}
		return append(rows, s.children(key)...)
	}))
}

// Sections under the parent that hold something to change, in the order the server sorts them
func (s *settings) children(parent string) []row {
	var rows []row
	for _, section := range s.sections {
		if section.GetParent() == parent && s.holds(section.GetKey()) {
			rows = append(rows, row{section: section})
		}
	}
	return rows
}

// Whether a setting is one a person changes here, those icebath keeps for itself left out
func (s *settings) visible(setting *configv1.Setting) bool { return !setting.GetInternal() }

// Whether the section or one under it holds a setting to change
func (s *settings) holds(key string) bool {
	for _, setting := range s.all {
		if setting.GetSection() == key && s.visible(setting) {
			return true
		}
	}
	for _, section := range s.sections {
		if section.GetParent() == key && s.holds(section.GetKey()) {
			return true
		}
	}
	return false
}

// Settings of the section and those under it that differ from their defaults
func (s *settings) changed(key string) int {
	n := 0
	for _, setting := range s.all {
		if setting.GetSection() == key && s.visible(setting) && setting.GetSource() != configv1.Source_SOURCE_DEFAULT {
			n++
		}
	}
	for _, section := range s.sections {
		if section.GetParent() == key {
			n += s.changed(section.GetKey())
		}
	}
	return n
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

// Titles of the sections from the top down to the one with the key
func (s *settings) path(key string) []string {
	var titles []string
	for at := s.section(key); at != nil; at = s.section(at.GetParent()) {
		titles = append([]string{at.GetTitle()}, titles...)
	}
	return titles
}

// Cells of a row: its name, its value, the mark of where the value comes from and what it does; one found by a search led by the
// sections it is in
func (s *settings) cells(r row, searched bool) []string {
	t := s.kit.Env.Theme
	trail := func(titles ...string) string { return strings.Join(titles, " "+t.Glyphs.Forward+" ") }
	if r.section != nil {
		name := r.section.GetTitle()
		if searched {
			name = trail(s.path(r.section.GetKey())...)
		}
		about := render.Dim(r.section.GetDescription(), t)
		if n := s.changed(r.section.GetKey()); n > 0 {
			about = strings.TrimSpace(about + "  " + render.Warm(strconv.Itoa(n)+" changed", t))
		}
		return []string{name + " " + render.Dim(t.Glyphs.Forward, t), "", "", about}
	}
	name := r.setting.GetTitle()
	if searched {
		name = trail(append(s.path(r.setting.GetSection()), name)...)
	}
	value := s.words(r.setting, r.setting.GetValue())
	if r.setting.GetSource() == configv1.Source_SOURCE_DEFAULT {
		value = render.Dim(value, t)
	}
	return []string{name, value, mark(r.setting), render.Dim(r.setting.GetDescription(), t)}
}

// Mark of where a setting's value comes from: yours, the environment's or locked, nothing for the default
func mark(setting *configv1.Setting) string {
	switch {
	case setting.GetLocked() != "" || setting.GetSource() == configv1.Source_SOURCE_MANAGED:
		return "🔒"
	case setting.GetSource() == configv1.Source_SOURCE_FILE:
		return "●"
	case setting.GetSource() == configv1.Source_SOURCE_ENVIRONMENT:
		return "$"
	}
	return ""
}

// Why a setting can't be changed here, as the refusal says it
func locked(setting *configv1.Setting) string {
	return setting.GetTitle() + " can't be changed here: " + setting.GetLocked()
}

// Value of the setting as one short cell: on or off, the title of a choice, a number with its unit, the items of a list, the count of the
// entries of a table, what an unset value means, a dash for nothing
func (s *settings) words(setting *configv1.Setting, value string) string {
	if setting.GetSecret() {
		if value == "" {
			return "—"
		}
		return hidden
	}
	decoded := decode(value)
	switch v := decoded.(type) {
	case nil:
		return cmp.Or(setting.GetUnset(), "—")
	case bool:
		if v {
			return "on"
		}
		return "off"
	case string:
		if v == "" {
			return "—"
		}
		return title(setting, v)
	case []any:
		titles := make([]string, len(v))
		for i, item := range v {
			titles[i] = title(setting, fmt.Sprint(item))
		}
		if len(titles) == 0 {
			return "—"
		}
		return strings.Join(titles, ", ")
	case map[string]any:
		return strconv.Itoa(len(v)) + " entries"
	}
	if unit := setting.GetUnit(); unit != "" {
		return config.WithUnit(decoded, unit)
	}
	return text(value)
}

// Where a value comes from, in plain words
func sourceWords(source configv1.Source) string {
	switch source {
	case configv1.Source_SOURCE_FILE:
		return "your config"
	case configv1.Source_SOURCE_ENVIRONMENT:
		return "an environment variable"
	case configv1.Source_SOURCE_MANAGED:
		return "your install, locked"
	}
	return "the default"
}

// Lines under the list about the highlighted row: a section's settings and sections, or what a setting does, its default, where its
// value comes from, its choices, why it can't be changed, the places that change it besides this screen and where it is kept
func (s *settings) detail(r row) []string {
	t := s.kit.Env.Theme
	if r.section != nil {
		var lines []string
		if about := r.section.GetDescription(); about != "" {
			lines = append(lines, about)
		}
		var own, inside []string
		for _, child := range s.all {
			if child.GetSection() == r.section.GetKey() && s.visible(child) {
				own = append(own, child.GetTitle())
			}
		}
		for _, child := range s.children(r.section.GetKey()) {
			inside = append(inside, child.section.GetTitle())
		}
		if len(own) > 0 {
			lines = append(lines, render.Dim(strings.Join(own, ", "), t))
		}
		if len(inside) > sectionsNamed {
			inside = append(inside[:sectionsNamed-1:sectionsNamed-1], "and "+strconv.Itoa(len(inside)-sectionsNamed+1)+" more")
		}
		if len(inside) > 0 {
			lines = append(lines, render.Dim(strings.Join(inside, ", "), t))
		}
		return append(lines, render.Dim(s.scope(r.section.GetKey()), t))
	}
	setting := r.setting
	lines := []string{setting.GetTitle() + " · " + setting.GetKey(), setting.GetDescription()}
	if !setting.GetSecret() {
		lines = append(lines, "default: "+s.words(setting, setting.GetDefaultValue()))
	}
	lines = append(lines, "value from: "+sourceWords(setting.GetSource()))
	if why := setting.GetLocked(); why != "" {
		lines = append(lines, render.Warm("Can't be changed here: "+why, t))
	}
	for _, option := range setting.GetOptions() {
		if option.GetDescription() != "" {
			lines = append(lines, "  "+cmp.Or(option.GetTitle(), option.GetValue())+": "+option.GetDescription())
		}
	}
	if variables := setting.GetEnv(); len(variables) > 0 {
		lines = append(lines, "", "Also changed by")
		for _, variable := range variables {
			lines = append(lines, "  the environment variable "+variable+", which wins over the rest")
		}
	}
	return append(lines, render.Dim(s.scope(setting.GetSection()), t))
}

// Where the settings of the section are kept: once for every profile, or in the profile shown
func (s *settings) scope(key string) string {
	if s.global(key) {
		return "Shared by every profile"
	}
	return "Kept in profile " + cmp.Or(s.profile, "default")
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

func (s *settings) hints(r row) []wm.Hint {
	if r.section != nil {
		return []wm.Hint{{Action: "hub.use", Label: "open"}}
	}
	if r.setting.GetLocked() != "" {
		return []wm.Hint{{Label: "can't be changed here"}}
	}
	hints := []wm.Hint{{Action: "hub.use", Label: "change"}}
	if r.setting.GetSource() == configv1.Source_SOURCE_FILE {
		hints = append(hints, wm.Hint{Action: "hub.reset", Label: "reset"})
	}
	return hints
}

// Changes the setting the way its kind takes: a toggle flips at once, a choice opens its choices, a list opens what it holds, a table opens
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
		return s.show(s.items(setting))
	case configv1.Kind_KIND_TABLE:
		return s.show(s.entries(setting))
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

// Choices of a setting, the one it holds marked
func (s *settings) choices(setting *configv1.Setting) *cards.List {
	held := func() string { return text(s.current(setting).GetValue()) }
	options := setting.GetOptions()
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search " + strings.ToLower(setting.GetTitle()),
		Columns: []render.Column{{}, {Max: 40}, {Flex: 1}},
		Yield:   []int{2},
		Rows: func() [][]string {
			rows := make([][]string, len(options))
			for i, option := range options {
				live := ""
				if option.GetValue() == held() {
					live = s.kit.Env.Theme.Glyphs.Live
				}
				rows[i] = []string{live, title(setting, option.GetValue()), render.Dim(option.GetDescription(), s.kit.Env.Theme)}
			}
			return rows
		},
		Empty: func() string { return "No choices" },
		Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "choose"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return s.save(setting, setting.GetKey(), strconv.Quote(options[i].GetValue()), list, "")
			},
		},
	})
	list.Refresh()
	list.Select(slices.IndexFunc(options, func(option *configv1.Option) bool { return option.GetValue() == held() }))
	return list
}

// Items a list setting holds, one added from the values the setting may take when it names them and typed in when it does not, and any one
// removed
func (s *settings) items(setting *configv1.Setting) *cards.List {
	held := func() []string { return list(s.current(setting).GetValue()) }
	named := len(setting.GetOptions()) > 0
	about := func(item string) string {
		for _, option := range setting.GetOptions() {
			if option.GetValue() == item {
				return option.GetDescription()
			}
		}
		return ""
	}
	columns, yield := []render.Column{{Flex: 1}}, []int(nil)
	if named {
		columns, yield = []render.Column{{Max: 40}, {Flex: 1}}, []int{1}
	}
	var shown []string
	view := cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.add",
		Pick:    true,
		Search:  "Search " + strings.ToLower(setting.GetTitle()),
		Columns: columns,
		Yield:   yield,
		Rows: func() [][]string {
			shown = held()
			rows := make([][]string, len(shown))
			for i, item := range shown {
				rows[i] = []string{title(setting, item)}
				if named {
					rows[i] = append(rows[i], render.Dim(about(item), s.kit.Env.Theme))
				}
			}
			return rows
		},
		Empty: func() string { return "Empty" },
		Free:  map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{{Action: "hub.add"}}
			if i >= 0 {
				hints = append(hints, wm.Hint{Action: "hub.remove"})
			}
			return hints
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.add": func(*cards.List, int) tea.Cmd { return s.add(setting, held) },
			"hub.remove": func(_ *cards.List, i int) tea.Cmd {
				item := shown[i]
				rest := slices.DeleteFunc(held(), func(have string) bool { return have == item })
				return s.store(setting, rest, "Removed "+title(setting, item))
			},
		},
	})
	s.views = append(s.views, view)
	return view
}

// Adds an item to a list setting: picked from the values it may take and does not hold yet, or typed in
func (s *settings) add(setting *configv1.Setting, held func() []string) tea.Cmd {
	heading := "Add to " + strings.ToLower(setting.GetTitle())
	if options := setting.GetOptions(); len(options) > 0 {
		var choices []tui.Choice
		for _, option := range options {
			if !slices.Contains(held(), option.GetValue()) {
				choices = append(choices, tui.Choice{ID: option.GetValue(), Title: cmp.Or(option.GetTitle(), option.GetValue()), Detail: option.GetDescription()})
			}
		}
		if len(choices) == 0 {
			return s.hub.Say("Everything is already added", cards.NoteInfo)
		}
		return s.kit.Pick(s.hub, heading, "add", choices, "", func(choice tui.Choice) tea.Cmd {
			return s.store(setting, append(held(), choice.ID), "Added "+choice.Title)
		})
	}
	return s.kit.Ask(s.hub, heading, cards.Field{Label: "Value"}, "Add", func(item string) tea.Cmd {
		if item == "" {
			return nil
		}
		if slices.Contains(held(), item) {
			return s.hub.Say(item+" is already in the list", cards.NoteInfo)
		}
		return s.store(setting, append(held(), item), "Added "+item)
	})
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

// Entries of a table setting by name, each changed, added or taken out on the hub. An entry is written under its own key, so a part of the
// table the administrator pins is refused on its own and the rest of the table is never copied over; an entry whose name holds a dot, which
// a key can't name, is written with the whole table
func (s *settings) entries(setting *configv1.Setting) *cards.List {
	held := func() map[string]any {
		entries, _ := decode(s.current(setting).GetValue()).(map[string]any)
		return entries
	}
	write := func(name string, value any, from wm.Card, told string) tea.Cmd {
		key := setting.GetKey() + "." + name
		if strings.Contains(name, ".") {
			whole := maps.Clone(held())
			if whole == nil {
				whole = map[string]any{}
			}
			if value == nil {
				delete(whole, name)
			} else {
				whole[name] = value
			}
			key, value = setting.GetKey(), whole
		}
		encoded, err := encode(value)
		if err != nil {
			return s.fail(err.Error())
		}
		if value == nil {
			encoded = "null"
		}
		return s.save(setting, key, encoded, from, told)
	}
	// Form of one entry's value, text or JSON, saving it or adding it
	entryForm := func(name string, value any, adding bool) tea.Cmd {
		submit, told := "Save", "Saved "+name+"."
		if adding {
			submit, told = "Add", "Added "+name+"."
		}
		var form *cards.Form
		form = cards.NewForm(s.kit.Env, cards.FormSpec{
			Title:  setting.GetTitle() + " › " + name,
			Fields: []cards.Field{{ID: "value", Label: "Value", Value: entryText(value), Help: "Text, or a value written as JSON"}},
			Submit: submit,
			Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
				return write(name, entryValue(values["value"]), form, told)
			},
		})
		return s.hub.Push(form)
	}
	var names []string
	view := cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search " + strings.ToLower(setting.GetTitle()),
		Columns: []render.Column{{Title: "name", Max: 32}, {Title: "value", Flex: 1}},
		Rows: func() [][]string {
			entries := held()
			names = slices.Sorted(maps.Keys(entries))
			rows := make([][]string, len(names))
			for i, name := range names {
				rows[i] = []string{name, entryWords(entries[name])}
			}
			return rows
		},
		Empty: func() string { return "No entries yet. " + s.kit.Env.Keys.Display("hub.add") + " adds one." },
		Free:  map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return entryForm(names[i], held()[names[i]], false) },
			"hub.add": func(*cards.List, int) tea.Cmd {
				return s.kit.Ask(s.hub, "add to "+setting.GetTitle(), cards.Field{Label: "Name"}, "Add", func(name string) tea.Cmd {
					if name == "" {
						return nil
					}
					if value, taken := held()[name]; taken {
						return entryForm(name, value, false)
					}
					return entryForm(name, nil, true)
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				name := names[i]
				l.Confirm("Take "+name+" out?", func() tea.Cmd { return write(name, nil, nil, "Took "+name+" out.") })
				return nil
			},
		},
	})
	s.views = append(s.views, view)
	return view
}

// Text an entry of a table is edited from: nothing for none, text as it is and every other value as the JSON its field reads back
func entryText(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, _ := json.Marshal(value)
	return string(data)
}

// Value of an entry from the text of its field: JSON taken as JSON, anything else as text
func entryValue(text string) any {
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		return value
	}
	return strings.TrimSpace(text)
}

// Value of an entry as one short cell
func entryWords(value any) string {
	switch v := value.(type) {
	case nil:
		return "—"
	case bool:
		if v {
			return "on"
		}
		return "off"
	case string:
		return cmp.Or(v, "—")
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = entryText(item)
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return strconv.Itoa(len(v)) + " entries"
	}
	return entryText(value)
}

// Field to type the value of a setting into: a number, a length of time, a secret or text
func (s *settings) field(setting *configv1.Setting) *cards.Form {
	field := cards.Field{ID: "value", Label: setting.GetTitle(), Value: text(setting.GetValue()), Placeholder: cmp.Or(setting.GetUnset(), text(setting.GetDefaultValue()))}
	switch {
	case setting.GetSecret():
		field.Kind, field.Required, field.Value, field.Placeholder, field.Help = cards.FieldSecret, true, "", "", "The old value is never shown"
	case setting.GetKind() == configv1.Kind_KIND_NUMBER:
		field.Kind, field.Required = cards.FieldNumber, true
		field.Check = func(value string) string {
			if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
				return "Enter a number"
			}
			return ""
		}
	case setting.GetKind() == configv1.Kind_KIND_DURATION:
		field.Label += " (" + setting.GetUnit() + ")"
		field.Required = setting.GetUnset() == ""
		field.Check = func(value string) string {
			value = strings.TrimSpace(value)
			if value == "" {
				return ""
			}
			if _, err := strconv.ParseFloat(value, 64); err == nil {
				return ""
			}
			if _, err := time.ParseDuration(value); err == nil {
				return ""
			}
			return "Enter a number or a length such as 30s or 5m"
		}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  setting.GetTitle(),
		Intro:  []string{setting.GetDescription()},
		Fields: []cards.Field{field},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			typed := strings.TrimSpace(values["value"])
			value := strconv.Quote(typed)
			switch {
			case setting.GetKind() == configv1.Kind_KIND_DURATION && typed == "":
				value = "null"
			case setting.GetKind() == configv1.Kind_KIND_NUMBER && !setting.GetSecret():
				value = typed
			}
			return s.save(setting, setting.GetKey(), value, form, "")
		},
	})
	return form
}

// Saves a value written in YAML under the key, the setting's own or one of its entries, null putting the default back, and tells the app
// the value the setting holds now. The view it came from closes, and a setting opened alone closes its hub with it. The note says what
// changed, told when given and else the new value
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
				told = now.GetTitle() + " is now " + s.words(now, now.GetValue()) + "."
			}
			switch now.GetSource() {
			case configv1.Source_SOURCE_ENVIRONMENT:
				told += " (" + now.GetVariable() + " overrides it)"
			case configv1.Source_SOURCE_MANAGED:
				told += " (your administrator pins it)"
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

// One thing the files view does, with what it does in plain words
type fileAction struct {
	label, does string
	run         func() tea.Cmd
}

// Files the settings and keys are kept in: where they are and whether they exist, each opened in the editor, the settings file checked
// after an edit and the keys counted again, and their paths copied
func (s *settings) files() tea.Cmd {
	if s.hub == nil {
		return nil
	}
	var files *configv1.FilesResponse
	var acts []fileAction
	where := func(path string, exists bool) string {
		if exists {
			return path
		}
		return path + " (not made yet)"
	}
	settingsFile := func() (string, bool) {
		if own := files.GetOwn(); own != "" {
			return own, files.GetOwnExists()
		}
		return "", false
	}
	detail := func() []string {
		lines := []string{"Every change on these screens is written to these files. Edit them by hand when you like; icebath reads a change the next time it needs the setting.", ""}
		if path, exists := settingsFile(); path != "" {
			lines = append(lines, "settings: "+where(path, exists))
		}
		lines = append(lines, "keys: "+where(files.GetEnv(), files.GetEnvExists()))
		switch {
		case files.GetManagedSystem() != "":
			lines = append(lines, "", files.GetManagedSystem()+" manages this install, so its files are not edited here.")
		case files.GetOwn() == "":
			lines = append(lines, "", "This profile keeps its settings in a store without a file, so there is nothing to edit by hand.")
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
		view = cards.NewList(s.kit.Env, cards.ListSpec{
			Title:   "Settings files",
			Glyph:   s.kit.Env.Theme.Glyphs.Settings,
			Key:     "settings:files",
			Context: cards.HubContext,
			Primary: "hub.use",
			Columns: []render.Column{{Title: "action"}, {Title: "what it does", Flex: 1}},
			Rows: func() [][]string {
				rows := make([][]string, len(acts))
				for i, act := range acts {
					rows[i] = []string{act.label, render.Dim(act.does, s.kit.Env.Theme)}
				}
				return rows
			},
			Detail: func(int) []string { return detail() },
			Hints:  func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "run"}} },
			Actions: map[string]func(*cards.List, int) tea.Cmd{
				"hub.use": func(_ *cards.List, i int) tea.Cmd {
					act := acts[i]
					return act.run()
				},
				"hub.refresh": func(*cards.List, int) tea.Cmd { return reread(func() tea.Cmd { return nil }) },
			},
			Free: map[string]bool{"hub.refresh": true},
		})
		view.ToggleDetail()
		return s.hub.Push(view)
	})
}

// What the files view offers for the files as they are: editing the settings file and the keys file unless a package manager keeps them or
// the profile keeps no file, checking the settings file once it is there, and copying both paths
func (s *settings) fileActions(files *configv1.FilesResponse) []fileAction {
	var out []fileAction
	own := files.GetOwn()
	if files.GetManagedSystem() == "" && own != "" {
		out = append(out, fileAction{"Edit the settings file", "open config.yaml in your editor, then check it", func() tea.Cmd { return s.editSettings(files) }})
		if files.GetEnvExists() && s.local(files, files.GetEnv()) {
			out = append(out, fileAction{"Edit the keys file", "open .env, where API keys and tokens are kept, in your editor", func() tea.Cmd { return s.editKeys(files.GetEnv()) }})
		}
	}
	if own != "" && files.GetOwnExists() {
		out = append(out, fileAction{"Check the settings file", "find mistakes and settings icebath does not know", s.check})
	}
	if own != "" {
		out = append(out, fileAction{"Copy the settings file path", "put the path of config.yaml on the clipboard", func() tea.Cmd { return s.copy(own) }})
	}
	return append(out, fileAction{"Copy the keys file path", "put the path of .env on the clipboard", func() tea.Cmd { return s.copy(files.GetEnv()) }})
}

// Whether the file is on this machine, where the editor runs, rather than on the machine of a server the app reaches
func (s *settings) local(files *configv1.FilesResponse, path string) bool {
	host, _ := os.Hostname()
	info, err := os.Stat(filepath.Dir(path))
	return host == files.GetHost() && err == nil && info.IsDir()
}

func (s *settings) copy(path string) tea.Cmd {
	return tea.Batch(clip.Copy(path), s.hub.Done("Copied the path."))
}

// Runs the editor on the file with the terminal handed over to it, then runs then once it is back
func (s *settings) edit(path string, then func() tea.Cmd) tea.Cmd {
	editor := config.Editor()
	if editor == "" {
		return s.hub.Fail("No editor found. The file is at " + path)
	}
	// An editor that ends with a status of its own still ran; the answer comes back as the message the callback returns
	return tea.ExecProcess(config.EditorCommand(s.kit.Env.Ctx, editor, path), func(err error) tea.Msg {
		return tui.Async(func() func() tea.Cmd {
			return func() tea.Cmd {
				if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
					return s.hub.Fail("Failed to open the editor: " + err.Error())
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
		if updated == 1 {
			return s.hub.Done("reloaded .env (1 var updated)")
		}
		return s.hub.Done(fmt.Sprintf("reloaded .env (%d vars updated)", updated))
	})
}

// Checks which variables the settings read are set and what the saved settings leave inactive, the report shown over the hub
func (s *settings) check() tea.Cmd {
	return tui.Call(s.hub, func() (*configv1.CheckResponse, error) {
		return s.service.Check(s.kit.Env.Ctx, &configv1.CheckRequest{Profile: s.named})
	}, func(report *configv1.CheckResponse) tea.Cmd {
		t := s.kit.Env.Theme
		lines := []string{"📋 Configuration Status"}
		for _, group := range []struct {
			title     string
			variables []*configv1.Variable
			required  bool
		}{{"Required", report.GetRequired(), true}, {"Optional", report.GetOptional(), false}} {
			lines = append(lines, "", "  "+group.title+":")
			for _, variable := range group.variables {
				switch {
				case variable.GetSet():
					lines = append(lines, "    ✓ "+variable.GetName())
				case group.required:
					lines = append(lines, render.Warm("    ✗ "+variable.GetName()+" (missing)", t))
				default:
					line := "    ○ " + variable.GetName()
					if readers := variable.GetSettings(); len(readers) > 0 {
						line += " → " + strings.Join(readers[:min(2, len(readers))], ", ")
					}
					lines = append(lines, render.Dim(line, t))
				}
			}
		}
		if diagnostics := report.GetDiagnostics(); len(diagnostics) > 0 {
			lines = append(lines, "", "  Saved configuration:")
			for _, line := range diagnostics {
				lines = append(lines, render.Warm("    ⚠ "+line, t))
			}
		}
		return s.hub.Push(cards.NewPage(s.kit.Env, cards.PageSpec{Title: "Check the settings file", Key: "settings:check",
			Lines: func(int) []string { return lines }}))
	})
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
