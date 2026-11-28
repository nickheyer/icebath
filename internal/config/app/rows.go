package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Option rows a page shows among its own: the settings as the service lists them, drawn and changed as Settings draws and changes them
type Rows struct{ s *settings }

// Option rows of the profile for a page, their controls opening over the hub; empty until Load reads them
func NewRows(kit *tui.Kit, hub *cards.Hub, service configv1connect.ConfigServiceClient, profile string) *Rows {
	return &Rows{s: newSettings(kit, service, profile).on(hub)}
}

// Reads the settings again, refreshes the lists that watch them, then runs then
func (r *Rows) Load(then func() tea.Cmd) tea.Cmd { return r.s.read(then) }

// List refreshed whenever the settings are read or one is saved
func (r *Rows) Watch(view *cards.List) { r.s.views.Add(view) }

func (r *Rows) Setting(key string) *configv1.Setting { return r.s.find(key) }

// Settings of the section a person changes there, the everyday ones and those behind Advanced; Custom and Internal ones left out
func (r *Rows) Section(key string) (everyday, advanced []*configv1.Setting) {
	own, rare := r.s.own(key)
	return settingsOf(own), settingsOf(rare)
}

func (r *Rows) Cells(setting *configv1.Setting) []string {
	return r.s.cells(row{setting: r.s.current(setting)})
}

func (r *Rows) Detail(setting *configv1.Setting, cut bool) []string {
	return r.s.detail(row{setting: r.s.current(setting)}, cut)
}

func (r *Rows) Hints(setting *configv1.Setting) []wm.Hint {
	return r.s.hints([]row{{setting: r.s.current(setting)}}, 0)
}

func (r *Rows) Change(setting *configv1.Setting) tea.Cmd { return r.s.change(r.s.current(setting)) }

func (r *Rows) Reset(list *cards.List, setting *configv1.Setting) tea.Cmd {
	return r.s.reset(list, r.s.current(setting))
}

// The row that opens the advanced settings of a section and the list it opens, false when the section has none to put behind it
func (r *Rows) Advanced(section string) (cells []string, open func() tea.Cmd, ok bool) {
	at := r.s.section(section)
	if at == nil || !r.s.apart(section) {
		return nil, nil, false
	}
	return r.s.cells(row{advanced: at}), func() tea.Cmd { return r.s.advanced(at) }, true
}

// The section of the key as the service lists it, with its title and what is set there; nil before the settings are read or for none
func (r *Rows) Info(key string) *configv1.Section { return r.s.section(key) }

// Settings of one entry of a table of things: its declared fields, each keyed below the entry and holding what the entry holds, which Cells,
// Detail, Hints, Change and Reset take like any other setting, a field reset going back to what the entry does without it; the everyday
// ones and those behind Advanced
func (r *Rows) Entry(table, name string) (everyday, advanced []*configv1.Setting) {
	own, rare := r.s.fields(table, name)
	return settingsOf(own), settingsOf(rare)
}

// The row that opens the advanced fields of one entry of a table and the list it opens, false when it has too few to put behind a row
func (r *Rows) EntryAdvanced(table, name string) (cells []string, open func() tea.Cmd, ok bool) {
	return r.s.entryAdvanced(table, name)
}

// Settings of one section alone, such as a page inside an area: those of the sections folded into the same area left with theirs, and the
// keys left out that the page shows rows of its own for; the everyday ones and those behind Advanced. Custom and Internal ones are left out,
// and so are the keys a picker keeps beside its setting, which that setting's row covers
func (r *Rows) Own(section string, leave ...string) (everyday, advanced []*configv1.Setting) {
	own, rare := r.s.mine(section, leave, false)
	return settingsOf(own), settingsOf(rare)
}

// The row that opens the advanced settings of one section alone and the list it opens, the keys left out as Own leaves them; false when
// it has too few to put behind a row of their own. Typing in the list searches those settings
func (r *Rows) OwnAdvanced(section string, leave ...string) (cells []string, open func() tea.Cmd, ok bool) {
	return r.s.ownAdvanced(section, leave, false)
}

// Own for the page a picker opens: the keys that picker keeps beside its setting, which Own leaves to the row of the setting, are rows of
// their own here. Leave the setting the picker is for when the page has a row of its own for it
func (r *Rows) OwnCovered(section string, leave ...string) (everyday, advanced []*configv1.Setting) {
	own, rare := r.s.mine(section, leave, true)
	return settingsOf(own), settingsOf(rare)
}

// OwnAdvanced for the page a picker opens, the keys that picker covers among the settings, as OwnCovered has them
func (r *Rows) OwnCoveredAdvanced(section string, leave ...string) (cells []string, open func() tea.Cmd, ok bool) {
	return r.s.ownAdvanced(section, leave, true)
}

// The row that opens advanced settings a page gathers itself, such as those of a service with a key of its own beside them, and the list it
// opens, keyed key and saying they are settings of of; false while they are too few to go behind a row (fewer than three, or nothing else
// is shown), so the page shows them in place. Advanced is asked again on each paint of the list
func (r *Rows) AdvancedOf(key, of string, everyday []*configv1.Setting, advanced func() []*configv1.Setting) (cells []string, open func() tea.Cmd, ok bool) {
	rare := r.s.rowsOf(advanced())
	if !apart(r.s.rowsOf(everyday), rare) {
		return nil, nil, false
	}
	about := "Settings of " + of + " that few people change"
	return r.s.advancedCells(rare), func() tea.Cmd {
		return r.s.hub.Push(r.s.list("Advanced", key, about, "Search in "+of, func(bool) []row { return r.s.rowsOf(advanced()) }))
	}, true
}

// Rows of settings, each as last read
func (s *settings) rowsOf(settings []*configv1.Setting) []row {
	rows := make([]row, len(settings))
	for i, setting := range settings {
		rows[i] = row{setting: s.current(setting)}
	}
	return rows
}

// Settings of rows
func settingsOf(rows []row) []*configv1.Setting {
	out := make([]*configv1.Setting, len(rows))
	for i, r := range rows {
		out[i] = r.setting
	}
	return out
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

// Words a search reads for a row besides the cells it shows: what it does, its key, the variables that set it and its value, and those of
// the settings its row covers
func (s *settings) wordsOf(r row) string {
	switch {
	case r.setting != nil:
		words := []string{strings.Join(r.trail, " ")}
		for _, setting := range s.group(r.setting) {
			words = append(words, setting.GetTitle(), setting.GetDescription(), setting.GetHelp(), setting.GetKey(), setting.GetShown())
			words = append(words, setting.GetEnv()...)
		}
		return strings.Join(words, " ")
	case r.section != nil:
		// A section is found by its own words: what it is, and for an area the names of the commands of its screen. An area whose page is a
		// screen is found too by what that screen shows and changes with views of its own, but only what no page or setting row of a search
		// shows, so each thing is found once, as its own row
		key := r.section.GetKey()
		words := []string{r.section.GetDescription()}
		for _, screen := range s.kit.Screens() {
			if r.section.GetArea() && screen.Area == key {
				words = append(append(words, screen.Slash...), screen.Aliases...)
			}
		}
		for _, inside := range s.sections {
			if s.theirs(inside) && s.areaOf(inside).GetKey() == key && !s.rowed(key, inside.GetKey()) {
				words = append(words, inside.GetTitle())
			}
		}
		for _, setting := range s.all {
			if setting.GetCustom() && !setting.GetInternal() && s.areaOf(s.section(setting.GetSection())).GetKey() == key &&
				!s.paged(key, setting.GetKey()) {
				words = append(words, setting.GetTitle())
			}
		}
		return strings.Join(words, " ")
	case r.act != nil:
		return strings.Join(append(slices.Clone(r.trail), r.act.about, r.act.words), " ")
	}
	return ""
}

// The setting with those its row covers, kept beside it as one choice
func (s *settings) group(setting *configv1.Setting) []*configv1.Setting {
	out := []*configv1.Setting{setting}
	for _, key := range s.beside(setting) {
		if other := s.find(key); other != nil {
			out = append(out, other)
		}
	}
	return out
}

// Mark before a value that was changed from its default
func changedMark(plain bool) string {
	if plain {
		return "*"
	}
	return "●"
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
			if setting := (*shown)[i].setting; setting != nil {
				return s.reset(l, setting)
			}
			return nil
		},
		"hub.refresh": func(*cards.List, int) tea.Cmd { return s.read(nil) },
	}
}

// Asks before the setting, with every setting its row covers, goes back to its default
func (s *settings) reset(l *cards.List, setting *configv1.Setting) tea.Cmd {
	if !s.yours(setting) {
		return nil
	}
	if setting.GetLocked() != "" {
		return s.fail(locked(setting))
	}
	text := "Reset " + setting.GetTitle() + "?"
	if !setting.GetSecret() {
		text = "Reset " + setting.GetTitle() + " to " + setting.GetDefaultShown() + "?"
	}
	values := map[string]string{}
	for _, each := range s.group(setting) {
		values[each.GetKey()] = s.cleared(each)
	}
	l.Confirm(cards.Question{Text: text, Verb: "Reset"}, func() tea.Cmd { return s.keep(setting, values, nil, false, "", nil) })
	return nil
}

// Whether the profile's settings file holds a value of the setting, or of a setting its row covers
func (s *settings) yours(setting *configv1.Setting) bool {
	return slices.ContainsFunc(s.group(setting), func(each *configv1.Setting) bool { return each.GetSource() == configv1.Source_SOURCE_FILE })
}

// Whether the setting, or a setting its row covers, differs from its default
func (s *settings) differs(setting *configv1.Setting) bool {
	return slices.ContainsFunc(s.group(setting), func(each *configv1.Setting) bool { return each.GetSource() != configv1.Source_SOURCE_DEFAULT })
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
	if _, picked := s.picker(r.setting); !picked && r.setting.GetKind() == configv1.Kind_KIND_TOGGLE {
		verb = "turn on"
		if on, _ := decode(r.setting.GetValue()).(bool); on {
			verb = "turn off"
		}
	}
	hints := []wm.Hint{{Action: "hub.use", Label: verb}}
	if s.yours(r.setting) {
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
		Columns: Columns,
		Rows: func() [][]string {
			shown = rows(list.Query() != "")
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = s.cells(r)
			}
			return cells
		},
		Words: func(i int) string { return s.wordsOf(shown[i]) },
		Detail: func(i int) []string {
			// A row that does something says what in its own value cell, said again only when the row cut it
			if shown[i].act != nil && !list.Cut(i, 2) {
				return nil
			}
			return s.detail(shown[i], list.Cut(i, 2))
		},
		Empty:   func() string { return tui.Waiting(s.loaded, s.err, "No settings to show.") },
		Free:    map[string]bool{"hub.refresh": true},
		Hints:   func(_ *cards.List, i int) []wm.Hint { return s.hints(shown, i) },
		Actions: s.rowActions(&shown),
	})
	list.ToggleDetail()
	s.views.Add(list)
	return list
}

// List of a section: its everyday settings, the sections under it, then its advanced settings behind one row, or in place when they are few
// or all it has. Typing searches the section and everything under it
func (s *settings) sectionList(section *configv1.Section) *cards.List {
	key := section.GetKey()
	return s.list(section.GetTitle(), "settings:"+key, section.GetDescription(), "Search in "+section.GetTitle(), func(searching bool) []row {
		if searching {
			return s.found(key)
		}
		everyday, rare := s.own(key)
		rows := everyday
		apart := s.apart(key)
		if !apart {
			rows = append(rows, rare...)
		}
		rows = append(rows, s.children(key)...)
		if apart {
			rows = append(rows, row{advanced: section})
		}
		return rows
	})
}

// Whether the advanced settings of a section go behind a row of their own
func (s *settings) apart(key string) bool { return apart(s.own(key)) }

// Whether advanced settings go behind a row of their own: three or more of them beside everyday ones
func apart(everyday, rare []row) bool { return len(rare) >= 3 && len(everyday) > 0 }

// Opens a section over the hub
func (s *settings) enter(section *configv1.Section) tea.Cmd {
	return s.hub.Push(s.sectionList(section))
}

// Opens the advanced settings of a section over the hub
func (s *settings) advanced(section *configv1.Section) tea.Cmd {
	// Typing searches the section and everything under it
	return s.behind(section, "", func(searching bool) []row {
		if searching {
			return s.found(section.GetKey())
		}
		_, rare := s.own(section.GetKey())
		return rare
	})
}

// Opens a list of the advanced settings of a section over the hub, the rows read again on each paint and again while typing searches.
// Tag tells lists of other advanced settings of the same section apart
func (s *settings) behind(section *configv1.Section, tag string, rows func(searching bool) []row) tea.Cmd {
	about := "Settings of " + section.GetTitle() + " that few people change"
	return s.hub.Push(s.list("Advanced", "settings:"+section.GetKey()+tag+":advanced", about, "Search in "+section.GetTitle(), rows))
}

// The row that opens the advanced settings of one section alone and what it opens, the keys left out, with the keys a picker covers when
// covered says so; typing in the list searches its own rows
func (s *settings) ownAdvanced(key string, leave []string, covered bool) ([]string, func() tea.Cmd, bool) {
	at := s.section(key)
	own, rare := s.mine(key, leave, covered)
	if at == nil || !apart(own, rare) {
		return nil, nil, false
	}
	tag := ":own"
	if covered {
		tag = ":covered"
	}
	return s.advancedCells(rare), func() tea.Cmd {
		return s.behind(at, tag, func(bool) []row {
			_, rare := s.mine(key, leave, covered)
			return rare
		})
	}, true
}

// Settings of one section alone, the keys left out, the everyday ones and those listed as advanced. The keys a picker keeps beside its
// setting are rows of their own when covered says so
func (s *settings) mine(key string, leave []string, covered bool) (everyday, rare []row) {
	for _, setting := range s.all {
		shown := s.visible(setting) || covered && s.covered[setting.GetKey()] != "" && !setting.GetInternal() && !setting.GetCustom()
		switch {
		case setting.GetSection() != key || !shown || slices.Contains(leave, setting.GetKey()):
		case setting.GetAdvanced():
			rare = append(rare, row{setting: setting})
		default:
			everyday = append(everyday, row{setting: setting})
		}
	}
	return everyday, rare
}

// Cells of a row that opens advanced settings: its name, the mark of a row that opens, and how many there are and how many were changed
func (s *settings) advancedCells(rare []row) []string {
	t := s.kit.Env.Theme
	return []string{"Advanced", render.Dim(t.Glyphs.Forward, t), render.Dim(s.tally(rare), t)}
}

// Section whose page lists the setting: its own, or the area that the settings of its section go into
func (s *settings) place(setting *configv1.Setting) string {
	if section := s.section(setting.GetSection()); section != nil && section.GetInto() != "" {
		return section.GetInto()
	}
	return setting.GetSection()
}

// Settings a person changes on the page of a section, the everyday ones and those listed as advanced
func (s *settings) own(key string) (everyday, rare []row) {
	for _, setting := range s.all {
		switch {
		case s.place(setting) != key || !s.visible(setting):
		case setting.GetAdvanced():
			rare = append(rare, row{setting: setting})
		default:
			everyday = append(everyday, row{setting: setting})
		}
	}
	return everyday, rare
}

// Sections listed as rows under the parent that hold something to change, in the order the server lists them. An area is no row of
// the section its key sits under, a section whose settings go into an area is no row at all, and nor is one the screen of its area shows
func (s *settings) children(parent string) []row {
	var rows []row
	for _, section := range s.sections {
		if s.up(section) == parent && section.GetInto() == "" && !s.theirs(section) && s.holds(section.GetKey()) {
			rows = append(rows, row{section: section})
		}
	}
	return rows
}

// Every section and setting under the base, the whole tree for no base, each with the trail of the sections between the base and it.
// A section the screen of its area shows is no row of its own; its settings are found one by one under the titles of the sections above
func (s *settings) found(base string) []row {
	within := func(key string) ([]string, bool) {
		var trail []string
		for at := s.section(key); at != nil; at = s.section(s.up(at)) {
			if at.GetKey() == base {
				return trail, true
			}
			if at.GetInto() == "" {
				trail = append([]string{at.GetTitle()}, trail...)
			}
		}
		return trail, base == ""
	}
	var rows []row
	for _, section := range s.sections {
		if section.GetInto() != "" || section.GetKey() == base || s.theirs(section) || !s.holds(section.GetKey()) {
			continue
		}
		if trail, ok := within(s.up(section)); ok {
			rows = append(rows, row{section: section, trail: trail})
		}
	}
	for _, setting := range s.all {
		if !s.visible(setting) {
			continue
		}
		if trail, ok := within(setting.GetSection()); ok {
			rows = append(rows, row{setting: setting, trail: trail})
		}
	}
	return rows
}

// Whether the section sits under an area whose page is a screen, which shows it: no list of the menu has a row for it
func (s *settings) theirs(section *configv1.Section) bool {
	area := s.areaOf(section)
	return area != nil && area.GetKey() != section.GetKey() && s.mounted(area.GetKey())
}

// Whether a setting is a row of its own a person changes here: not one icebath keeps for itself, not one its area's page changes with a
// view of its own, not one kept beside another as one choice, and not a field of one entry of a table, which the entry's page shows
func (s *settings) visible(setting *configv1.Setting) bool {
	return !setting.GetInternal() && !setting.GetCustom() && s.covered[setting.GetKey()] == "" && setting.GetEntryOf() == ""
}

// Declared fields of one entry of a table as settings of their own, the everyday ones and those listed as advanced
func (s *settings) fields(table, name string) (everyday, rare []row) {
	prefix := table + "." + name + "."
	for _, setting := range s.all {
		switch {
		// A field another field's picker keeps beside it is one row with that field, as with the settings of a section
		case setting.GetEntryOf() != table || !strings.HasPrefix(setting.GetKey(), prefix) || setting.GetInternal() || setting.GetCustom() ||
			s.covered[setting.GetKey()] != "":
		case setting.GetAdvanced():
			rare = append(rare, row{setting: setting})
		default:
			everyday = append(everyday, row{setting: setting})
		}
	}
	return everyday, rare
}

// Whether the section or one under it holds a setting to change, or has a page made for it
func (s *settings) holds(key string) bool {
	total, _ := s.count(key)
	return total > 0 || s.mounted(key)
}

// Rows of settings on the page of the section and the sections under it, and how many of them differ from their defaults
func (s *settings) count(key string) (total, changed int) {
	for _, setting := range s.all {
		if s.place(setting) == key && s.visible(setting) {
			total++
			if s.differs(setting) {
				changed++
			}
		}
	}
	for _, section := range s.sections {
		if section.GetParent() == key && section.GetInto() == "" && !section.GetArea() {
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
	if s.yours(r.setting) {
		mark = render.Cool(changedMark(t.Plain), t)
	}
	return []string{s.name(r), mark, value}
}

// What a row that opens a section says is inside: how many settings, and how many of them were changed. For the advanced settings of a
// section only those count
func (s *settings) inside(key string, advanced bool) string {
	if advanced {
		_, rare := s.own(key)
		return s.tally(rare)
	}
	total, changed := s.count(key)
	// A page with no setting of its own to count says what is there instead
	if section := s.section(key); total == 0 && section != nil {
		return section.GetDescription()
	}
	return counted(total, changed)
}

// How many settings the rows are and how many of them were changed
func (s *settings) tally(rows []row) string {
	changed := 0
	for _, r := range rows {
		if s.differs(r.setting) {
			changed++
		}
	}
	return counted(len(rows), changed)
}

// A count of settings in words, with how many were changed when any were
func counted(total, changed int) string {
	said := render.Count(total, "setting")
	if changed > 0 {
		said += ", " + strconv.Itoa(changed) + " changed"
	}
	return said
}

// Value of a setting as a row shows it: what its picker says, else the words the server gives, a list cut after its last whole item that
// fits and any other value cut at the room a row has, then where it comes from when that is not this screen. Whether it was cut
func (s *settings) value(setting *configv1.Setting) (string, bool) {
	shown, cut := s.shown(setting), false
	if items := list(setting.GetValue()); setting.GetKind() == configv1.Kind_KIND_LIST && len(items) > 0 {
		for i := range items {
			items[i] = s.itemWords(setting, items[i])
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

// The value in words: what the picker of its kind says of the values held, else what the server says
func (s *settings) shown(setting *configv1.Setting) string {
	// A picker words one value; a list's items are worded one by one
	if picker, ok := s.picker(setting); ok && picker.Shown != nil && setting.GetKind() != configv1.Kind_KIND_LIST {
		if said := picker.Shown(s.ask(setting)); said != "" {
			return said
		}
	}
	return setting.GetShown()
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
// command that changes it too, then where its value comes from when that is not the default, whether every profile shares it, its keys and
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
	case s.yours(setting):
		lines = append(lines, render.Cool("You changed this. "+s.kit.Env.Keys.Display("hub.remove")+" puts the default back.", t))
	}
	if s.global(setting.GetSection()) {
		lines = append(lines, render.Dim("Shared by every profile.", t))
	}
	var keys, variables []string
	for _, each := range s.group(setting) {
		keys = append(keys, each.GetKey())
		variables = append(variables, each.GetEnv()...)
	}
	where := "Config key: " + keys[0]
	if len(keys) > 1 {
		where = "Config keys: " + strings.Join(keys, ", ")
	}
	switch len(variables) {
	case 0:
	case 1:
		where += " · Variable: " + variables[0]
	default:
		where += " · Variables: " + strings.Join(variables, ", ")
	}
	lines = append(lines, render.Dim(where, t))
	// The value whole, where its row could not show it so
	if _, long := s.value(setting); long || cut {
		lines = append(lines, render.Dim("Value: "+s.shown(setting), t))
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

// Whether a page inside the area shows the setting at the key, so a search finds it as that page
func (s *settings) paged(area, key string) bool {
	return slices.ContainsFunc(s.pages[area], func(page tui.Page) bool { return page.Shows(key) })
}

// Whether a search finds what a section inside the area holds as rows of their own: a page inside the area that shows the section, or its
// settings one by one
func (s *settings) rowed(area, section string) bool {
	inside := strings.ToLower(section) + "."
	for _, page := range s.pages[area] {
		for _, key := range page.Keys {
			key = strings.ToLower(key)
			if strings.HasPrefix(key, inside) || strings.HasSuffix(key, ".") && strings.HasPrefix(inside, key) {
				return true
			}
		}
	}
	at := s.section(section)
	if at == nil {
		return false
	}
	// A page or a setting row that carries the section's own title stands for it, such as the page Terminal windows or the option Key proxy
	title := strings.ToLower(at.GetTitle())
	if slices.ContainsFunc(s.pages[area], func(page tui.Page) bool { return strings.ToLower(page.Title) == title }) {
		return true
	}
	own := func(setting *configv1.Setting) bool { return setting.GetSection() == section && s.visible(setting) }
	if slices.ContainsFunc(s.all, func(setting *configv1.Setting) bool {
		return own(setting) && strings.ToLower(setting.GetTitle()) == title
	}) {
		return true
	}
	// Its settings show its title before theirs only when it is not folded into the section above it
	return at.GetInto() == "" && slices.ContainsFunc(s.all, own)
}
