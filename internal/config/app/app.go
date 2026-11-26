package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/profile"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	// Screen of every setting, opened on a section by its key or filtered by any other words
	Settings = "settings"
	// Screen that changes one setting, opened with its key
	Edit = "setting"
)

// What a secret shows once it is set
const hidden = "••••••••"

// Sections under another its detail names before it counts the rest
const sectionsNamed = 6

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{
		{
			ID:    Settings,
			Title: "Settings",
			Short: "Change settings",
			Slash: []string{"settings", "config"},
			Usage: "[@profile] [section|search]",
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

// Opens the way to change one setting on its own
func (s *settings) one(key string) tea.Cmd {
	s.direct = true
	return s.read(func() tea.Cmd {
		setting := s.find(key)
		if setting == nil {
			return s.kit.WM.Toast("No setting named "+key, wm.Warn, wm.DefaultToastTTL)
		}
		return s.change(setting)
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
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search settings",
		Columns: []render.Column{{Max: 48}, {Flex: 1}},
		Rows: func() [][]string {
			shown = rows()
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = s.cells(r, list == s.root && list.Query() != "")
			}
			return cells
		},
		Detail: func(i int) []string { return s.detail(shown[i]) },
		Empty:  func() string { return tui.Waiting(s.loaded, s.err, "No settings here") },
		Free:   map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return s.hints(shown[i])
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
				if !setting.GetSecret() {
					return s.save(setting, "null", nil, "")
				}
				l.Confirm("Clear the saved "+strings.ToLower(setting.GetTitle())+"?", func() tea.Cmd { return s.save(setting, "null", nil, "") })
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.read(func() tea.Cmd { return nil }) },
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

// Cells of a row, one found by a search led by the sections it is in
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
		return []string{name + " " + render.Dim(t.Glyphs.Forward, t), about}
	}
	name := r.setting.GetTitle()
	if searched {
		name = trail(append(s.path(r.setting.GetSection()), name)...)
	}
	return []string{name, s.shown(r.setting)}
}

// Value of a setting as shown in its row, dim while it is the default and naming the variable that sets it
func (s *settings) shown(setting *configv1.Setting) string {
	t := s.kit.Env.Theme
	text := s.words(setting, setting.GetValue())
	switch setting.GetSource() {
	case configv1.Source_SOURCE_DEFAULT:
		return render.Dim(text, t)
	case configv1.Source_SOURCE_ENVIRONMENT:
		return text + render.Dim("  from "+setting.GetVariable(), t)
	}
	return text
}

// Value of the setting as text
func (s *settings) words(setting *configv1.Setting, value string) string {
	if setting.GetSecret() {
		if value == "" {
			return "Not set"
		}
		return hidden
	}
	decoded := decode(value)
	switch setting.GetKind() {
	case configv1.Kind_KIND_TOGGLE:
		if on, _ := decoded.(bool); on {
			return "On"
		}
		return "Off"
	case configv1.Kind_KIND_LIST:
		var titles []string
		for _, item := range list(value) {
			titles = append(titles, title(setting, item))
		}
		if len(titles) == 0 {
			return "None"
		}
		return strings.Join(titles, ", ")
	case configv1.Kind_KIND_TABLE:
		entries, _ := decoded.(map[string]any)
		if len(entries) == 0 {
			return "None"
		}
		return render.Count(len(entries), "entry")
	case configv1.Kind_KIND_CHOICE:
		return title(setting, text(value))
	}
	if words := text(value); words != "" {
		return words
	}
	return "Not set"
}

// Lines under the list about the highlighted row: what it is, where its value comes from and its key in config.yaml
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
	lines := []string{setting.GetDescription()}
	switch setting.GetSource() {
	case configv1.Source_SOURCE_ENVIRONMENT:
		lines = append(lines, render.Warm("Set by "+setting.GetVariable()+", which overrides config.yaml", t))
	case configv1.Source_SOURCE_FILE:
		if !setting.GetSecret() {
			lines = append(lines, "Default: "+s.words(setting, setting.GetDefaultValue()))
		}
	}
	if setting.GetSource() != configv1.Source_SOURCE_ENVIRONMENT && len(setting.GetEnv()) > 0 {
		lines = append(lines, render.Dim("Can also be set with "+strings.Join(setting.GetEnv(), " or "), t))
	}
	return append(lines, render.Dim(s.scope(setting.GetSection())+", under "+setting.GetKey(), t))
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
	use := wm.Hint{Action: "hub.use", Label: "change"}
	switch r.setting.GetKind() {
	case configv1.Kind_KIND_TOGGLE:
		use.Label = "toggle"
	case configv1.Kind_KIND_LIST:
		use.Label = "edit"
	}
	hints := []wm.Hint{use}
	if r.setting.GetSource() == configv1.Source_SOURCE_FILE {
		hints = append(hints, wm.Hint{Action: "hub.reset"})
	}
	return hints
}

// Changes the setting the way its kind takes: a toggle flips at once, a choice opens its choices, a list opens what it holds, and the
// rest open a field to type into
func (s *settings) change(setting *configv1.Setting) tea.Cmd {
	switch setting.GetKind() {
	case configv1.Kind_KIND_TOGGLE:
		on, _ := decode(setting.GetValue()).(bool)
		return s.save(setting, strconv.FormatBool(!on), nil, "")
	case configv1.Kind_KIND_CHOICE:
		return s.show(s.choices(setting))
	case configv1.Kind_KIND_LIST:
		return s.show(s.items(setting))
	case configv1.Kind_KIND_TABLE:
		return s.fail(setting.GetTitle() + " can only be changed in config.yaml, under " + setting.GetKey())
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
				mark := ""
				if option.GetValue() == held() {
					mark = s.kit.Env.Theme.Glyphs.Live
				}
				rows[i] = []string{mark, title(setting, option.GetValue()), render.Dim(option.GetDescription(), s.kit.Env.Theme)}
			}
			return rows
		},
		Empty: func() string { return "No choices" },
		Hints: func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.use", Label: "choose"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return s.save(setting, strconv.Quote(options[i].GetValue()), list, "")
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
	return cards.NewList(s.kit.Env, cards.ListSpec{
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
	return s.save(setting, value, nil, told)
}

// Field to type the value of a setting into: a number, a secret or text
func (s *settings) field(setting *configv1.Setting) *cards.Form {
	field := cards.Field{ID: "value", Label: setting.GetTitle(), Help: setting.GetDescription()}
	switch {
	case setting.GetSecret():
		field.Kind, field.Required = cards.FieldSecret, true
	case setting.GetKind() == configv1.Kind_KIND_NUMBER:
		field.Kind, field.Required, field.Value = cards.FieldNumber, true, text(setting.GetValue())
		field.Check = func(value string) string {
			if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
				return "Enter a whole number"
			}
			return ""
		}
	default:
		field.Value = text(setting.GetValue())
		if fallback := text(setting.GetDefaultValue()); fallback != "" {
			field.Placeholder = fallback
		}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  setting.GetTitle(),
		Fields: []cards.Field{field},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			typed := strings.TrimSpace(values["value"])
			value := strconv.Quote(typed)
			if setting.GetKind() == configv1.Kind_KIND_NUMBER && !setting.GetSecret() {
				value = typed
			}
			return s.save(setting, value, form, "")
		},
	})
	return form
}

// Saves a value written in YAML, null putting the default back, and tells the app the value it holds now. The view it came from closes, and
// a setting opened alone closes its hub with it. The note says what changed, told when given and else the new value
func (s *settings) save(setting *configv1.Setting, value string, from wm.Card, told string) tea.Cmd {
	key := setting.GetKey()
	if s.hub != nil && s.hub.Shown() {
		s.hub.Busy("Saving…")
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := s.kit.Env.Ctx
		_, err := s.service.Set(ctx, &configv1.SetRequest{Profile: s.named, Key: key, Value: value})
		var now *configv1.Setting
		if err == nil {
			var response *configv1.GetResponse
			if response, err = s.service.Get(ctx, &configv1.GetRequest{Profile: s.named, Key: key}); err == nil {
				now = response.GetSetting()
			}
		}
		return func() tea.Cmd {
			if err != nil {
				return s.fail(tui.Reason(err))
			}
			for i, have := range s.all {
				if have.GetKey() == key {
					s.all[i] = now
				}
			}
			for _, view := range s.views {
				view.Refresh()
			}
			switch {
			case told != "":
			case value == "null":
				told = now.GetTitle() + " reset to " + s.words(now, now.GetDefaultValue())
			default:
				told = now.GetTitle() + ": " + s.words(now, now.GetValue())
			}
			if now.GetSource() == configv1.Source_SOURCE_ENVIRONMENT {
				told += " (" + now.GetVariable() + " overrides it)"
			}
			var cmds []tea.Cmd
			// Screens follow the settings of the profile in use, so a change to another profile's is theirs to hear only when every profile shares it
			if s.named == "" || s.global(now.GetSection()) {
				cmds = append(cmds, s.kit.Changed(key, now.GetValue()))
			}
			switch {
			case s.direct && from != nil:
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
