package app

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Picker of the kind of thing the setting's value names, false for a setting that names none or a kind no screen offers a picker for. A
// table names things by the names of its entries, which its own add row picks, so it has none of its own
func (s *settings) picker(setting *configv1.Setting) (tui.Picker, bool) {
	if setting.GetKind() == configv1.Kind_KIND_TABLE {
		return tui.Picker{}, false
	}
	return s.kit.Picker(setting.GetPicks())
}

// Keys of the settings its picker keeps beside the setting as one choice, none for a setting with no picker or a choice of one value
func (s *settings) beside(setting *configv1.Setting) []string {
	picker, ok := s.picker(setting)
	if !ok || picker.Beside == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(picker.Beside(setting.GetKey())), func(key string) bool { return key == setting.GetKey() })
}

// What the picker of the setting is asked: the values of the setting and of those beside it, and what is in effect when nothing is picked
func (s *settings) ask(setting *configv1.Setting) tui.Ask {
	values := map[string]string{}
	for _, each := range s.group(setting) {
		values[each.GetKey()] = each.GetValue()
	}
	var first []tui.Choice
	if zero := setting.GetZero(); zero != "" {
		first = []tui.Choice{{Title: zero}}
	}
	_, of := tui.PickOf(setting.GetPicks())
	return tui.Ask{Title: setting.GetTitle(), Intro: []string{setting.GetDescription()}, Key: setting.GetKey(), Values: values, First: first,
		Profile: s.named, Of: of}
}

// Opens the picker of the setting over the hub, every value it hands back saved as the row's own change: the job then finishes, the view
// the picker showed closing, or the menu the person came straight into for it
func (s *settings) choose(picker tui.Picker, setting *configv1.Setting) tea.Cmd {
	ask := s.ask(setting)
	// One value picked ends the job once it is kept, however long the picker took to show its list. What it hands back is kept for the
	// setting and the settings beside it, a key that names no setting here left out
	ask.Done = func(values map[string]string) tea.Cmd {
		kept := map[string]string{}
		for _, each := range s.group(setting) {
			if value, ok := values[each.GetKey()]; ok {
				kept[each.GetKey()] = value
			}
		}
		if len(kept) == 0 {
			return nil
		}
		return s.keep(setting, kept, nil, true, "", nil)
	}
	return picker.Open(s.kit, s.hub, ask)
}

// Changes the setting the way its kind takes: a value that names a thing opens the picker of that kind, a switch flips at once, a choice
// opens its choices, a list opens what it holds, a table opens its entries, a value that names a thing of the profile opens those things,
// and the rest open a field to type into. One the administrator, Remote Config or a package manager decides is refused with why
func (s *settings) change(setting *configv1.Setting) tea.Cmd {
	if setting.GetLocked() != "" {
		return s.fail(locked(setting))
	}
	// A list of things is a list of its items, each picked with the picker of their kind
	if picker, ok := s.picker(setting); ok && setting.GetKind() != configv1.Kind_KIND_LIST {
		return s.choose(picker, setting)
	}
	switch setting.GetKind() {
	case configv1.Kind_KIND_TOGGLE:
		on, _ := decode(setting.GetValue()).(bool)
		return s.save(setting, setting.GetKey(), strconv.FormatBool(!on), nil, "")
	case configv1.Kind_KIND_CHOICE:
		return s.push(s.choices(setting))
	case configv1.Kind_KIND_LIST:
		switch {
		case recorded(setting):
			return s.push(s.records(setting.GetKey(), setting.GetTitle(), setting.GetDescription(), setting.GetEntry(), func() []any {
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
			return s.push(s.checks(setting))
		}
		return s.push(s.items(setting))
	case configv1.Kind_KIND_TABLE:
		if single(setting) {
			return s.push(s.pairs(setting))
		}
		return s.push(s.entries(setting, ""))
	}
	if len(setting.GetNames()) > 0 {
		return s.push(s.things(setting))
	}
	return s.push(s.field(setting))
}

// Shows a view to change a setting over the hub, or as a dialog of its own when there is no hub to show it over
func (s *settings) push(view wm.Card) tea.Cmd {
	if s.hub == nil {
		hub, cmd := s.kit.Show(Edit, view)
		s.hub = hub
		return cmd
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
	held := func() string { return text(s.current(setting).GetValue()) }
	return s.pick(setting, setting.GetOptions(), held, func(option *configv1.Option) string { return strconv.Quote(option.GetValue()) })
}

// Things of the profile a setting's value names, each by its title: first what is in effect when nothing is picked, then the things, and
// a value held that none of them has, shown as it is
func (s *settings) things(setting *configv1.Setting) *cards.List {
	held := func() string { return text(s.current(setting).GetValue()) }
	none := setting.GetZero()
	if none == "" {
		none = strs.Capital(setting.GetUnset())
	}
	if none == "" {
		none = config.NotSet
	}
	options := append([]*configv1.Option{{Title: none}}, setting.GetNames()...)
	if now := held(); now != "" && !slices.ContainsFunc(options, func(option *configv1.Option) bool { return option.GetValue() == now }) {
		options = append(options, &configv1.Option{Value: now, Title: now})
	}
	return s.pick(setting, options, held, func(option *configv1.Option) string {
		if option.GetValue() == "" {
			return "null"
		}
		return strconv.Quote(option.GetValue())
	})
}

// List of the options of a setting to pick one from, the one held marked and in hand; enter saves the one in hand as value says it and
// finishes the job
func (s *settings) pick(setting *configv1.Setting, options []*configv1.Option, held func() string, value func(*configv1.Option) string) *cards.List {
	t := s.kit.Env.Theme
	said := func(option *configv1.Option) string {
		if option.GetTitle() != "" {
			return option.GetTitle()
		}
		return title(setting, option.GetValue())
	}
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     len(options) > 8,
		Search:  "Search in " + setting.GetTitle(),
		Intro:   func() []string { return []string{setting.GetDescription()} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(options))
			for i, option := range options {
				rows[i] = []string{s.kit.Mark(true, option.GetValue() == held()), said(option), render.Dim(option.GetDescription(), t)}
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
				return s.save(setting, setting.GetKey(), value(options[i]), list, "")
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
		Search:  "Search in " + setting.GetTitle(),
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
					return s.store(setting, slices.DeleteFunc(held(), func(have string) bool { return have == value }), name+" is now off.", nil)
				}
				return s.store(setting, append(held(), value), name+" is now on.", nil)
			},
		},
	})
	view.ToggleDetail()
	s.views.Add(view)
	return view
}

// What one item of a list of text is called where one is asked for: what the list says an item is, else the list's own title
func item(setting *configv1.Setting) string {
	if noun := setting.GetEntry().GetNoun(); noun != "" {
		return strs.Capital(noun)
	}
	return setting.GetTitle()
}

// Items a list setting holds, each changed, removed after the question, or added: picked with the picker of its kind when it names things,
// else typed under the name of what an item is. The last row adds another and is in hand while the list is empty. A list whose order
// matters moves the item in hand up and down
func (s *settings) items(setting *configv1.Setting) *cards.List {
	held := func() []string { return list(s.current(setting).GetValue()) }
	noun := setting.GetEntry().GetNoun()
	label := item(setting)
	picker, picks := s.picker(setting)
	ordered := setting.GetOrdered()
	// What to type, under the field: what the list is for, then the example its help gives
	line := strings.TrimSpace(setting.GetDescription() + "\n" + setting.GetHelp())
	// One item asked for, picked or typed; was is the item changed, empty for a new one
	ask := func(heading, was, verb string, done func(item string) tea.Cmd) tea.Cmd {
		check := func(item string) tea.Cmd {
			switch {
			case item == "" || item == was:
				return nil
			case slices.Contains(held(), item):
				return s.hub.Say(s.itemWords(setting, item)+" is already in the list.", cards.NoteInfo)
			}
			return done(item)
		}
		if picks {
			asked := s.itemAsk(setting, was)
			asked.Title = heading
			asked.Done = func(values map[string]string) tea.Cmd { return check(text(values[setting.GetKey()])) }
			return picker.Open(s.kit, s.hub, asked)
		}
		return s.kit.Ask(s.hub, heading, cards.Field{Label: label, Value: was, Help: line, Path: pathOf(setting)}, verb, check)
	}
	adding, changing := adds(noun, setting.GetTitle()), setting.GetTitle()
	if noun != "" {
		changing = "Change " + noun
	}
	var shown []string
	var view *cards.List
	add := func() tea.Cmd {
		return ask(adding, "", "Add", func(item string) tea.Cmd {
			return s.store(setting, append(held(), item), "Added "+s.itemWords(setting, item)+".", func() { view.Select(slices.Index(shown, item)) })
		})
	}
	// The item in hand moved by one place, up for a step of -1, and kept in hand
	move := func(i, step int) tea.Cmd {
		items := held()
		to := i + step
		if !ordered || i < 0 || i >= len(items) || to < 0 || to >= len(items) {
			return nil
		}
		items[i], items[to] = items[to], items[i]
		moved := items[to]
		return s.store(setting, items, s.itemWords(setting, moved)+" is now "+ordinal(to+1)+".", func() { view.Select(to) })
	}
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   s.kit.Env.Theme.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search in " + setting.GetTitle(),
		Intro: func() []string {
			return s.opening(setting.GetDescription(), noun, setting.GetTitle(), len(held()) == 0)
		},
		Columns: []render.Column{{Flex: 1}},
		Rows: func() [][]string {
			shown = held()
			rows := make([][]string, len(shown), len(shown)+1)
			for i, item := range shown {
				rows[i] = []string{s.itemWords(setting, item)}
			}
			return append(rows, []string{s.adder(adding)})
		},
		Words: func(i int) string {
			if i < len(shown) {
				return shown[i]
			}
			return ""
		},
		Detail: func(i int) []string {
			if !ordered || i >= len(shown) {
				return nil
			}
			return []string{ordinal(i+1) + " of " + strconv.Itoa(len(shown)) + "."}
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add"}}
			case i >= len(shown):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			hints := []wm.Hint{{Action: "hub.use", Label: "change"}}
			if ordered && i > 0 {
				hints = append(hints, wm.Hint{Action: "hub.up", Label: "move up"})
			}
			if ordered && i < len(shown)-1 {
				hints = append(hints, wm.Hint{Action: "hub.down", Label: "move down"})
			}
			return append(hints, wm.Hint{Action: "hub.add"}, wm.Hint{Action: "hub.remove"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					return add()
				}
				was := shown[i]
				return ask(changing, was, "Save", func(item string) tea.Cmd {
					items := held()
					if at := slices.Index(items, was); at >= 0 {
						items[at] = item
					}
					told := "Changed " + s.itemWords(setting, was) + " to " + s.itemWords(setting, item) + "."
					return s.store(setting, items, told, func() { view.Select(slices.Index(shown, item)) })
				})
			},
			"hub.add":  func(*cards.List, int) tea.Cmd { return add() },
			"hub.up":   func(_ *cards.List, i int) tea.Cmd { return move(i, -1) },
			"hub.down": func(_ *cards.List, i int) tea.Cmd { return move(i, 1) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(shown) {
					return nil
				}
				item := shown[i]
				said := s.itemWords(setting, item)
				l.Confirm(cards.Question{Text: "Remove " + said + "?", Verb: "Remove", Deletes: true}, func() tea.Cmd {
					return s.store(setting, slices.DeleteFunc(held(), func(have string) bool { return have == item }), "Removed "+said+".", nil)
				})
				return nil
			},
		},
	})
	s.views.Add(view)
	return view
}

// What the picker of a list's kind is asked for one item of it: that item, none for a new one
func (s *settings) itemAsk(setting *configv1.Setting, item string) tui.Ask {
	_, of := tui.PickOf(setting.GetPicks())
	values := map[string]string{}
	if item != "" {
		if encoded, err := encode(item); err == nil {
			values[setting.GetKey()] = encoded
		}
	}
	return tui.Ask{Title: setting.GetTitle(), Intro: []string{setting.GetDescription()}, Key: setting.GetKey(), Values: values, Profile: s.named,
		Of: of, Item: true}
}

// An item of a list in words: what the picker of its kind says of it, else the title the setting gives it, else the item itself
func (s *settings) itemWords(setting *configv1.Setting, item string) string {
	if picker, ok := s.picker(setting); ok && picker.Shown != nil {
		if said := picker.Shown(s.itemAsk(setting, item)); said != "" {
			return said
		}
	}
	return title(setting, item)
}

// Place in a list as a person says it: first, second, third, then 4th and on
func ordinal(n int) string {
	switch n {
	case 1:
		return "first"
	case 2:
		return "second"
	case 3:
		return "third"
	}
	return strconv.Itoa(n) + "th"
}

// Words of the row and the job that add another to a list: "Add a header" for a list that says what an item is, else what it adds to
func adds(noun, title string) string {
	if noun != "" {
		return "Add " + article(noun) + " " + noun
	}
	return "Add to " + title
}

// Cell of the row that adds another to a list: its words and the mark of a row that opens
func (s *settings) adder(words string) string {
	t := s.kit.Env.Theme
	return words + "  " + render.Dim(t.Glyphs.Forward, t)
}

// Lines over a list that can grow: what it is, then while it is empty one sentence saying what would be here, the things by the noun the
// list gives them, else by its title
func (s *settings) opening(about, noun, title string, empty bool) []string {
	lines := []string{about}
	// A noun is declared in the case it reads in inside a sentence; a title is a name as it is written
	switch {
	case empty && noun != "":
		lines = append(lines, "No "+strings.TrimPrefix(render.Count(2, noun), "2 ")+" yet.")
	case empty:
		lines = append(lines, "Nothing in "+title+" yet.")
	}
	return slices.DeleteFunc(lines, func(line string) bool { return line == "" })
}

// Indefinite article before a noun as it is said
func article(noun string) string {
	if noun != "" && strings.ContainsRune("aeiou", rune(strings.ToLower(noun)[0])) {
		return "an"
	}
	return "a"
}

// Saves the items of a list setting, saying what changed, then runs then. A setting that names its values keeps only those it names now,
// so an item gone since it was added, such as a provider removed from config.yaml, can't fail the save
func (s *settings) store(setting *configv1.Setting, items []string, told string, then func()) tea.Cmd {
	if options := setting.GetOptions(); len(options) > 0 {
		items = slices.DeleteFunc(slices.Clone(items), func(item string) bool {
			return !slices.ContainsFunc(options, func(option *configv1.Option) bool { return option.GetValue() == item })
		})
	}
	value, err := encode(append([]string{}, items...))
	if err != nil {
		return s.fail(err.Error())
	}
	return s.keep(setting, map[string]string{setting.GetKey(): value}, nil, false, told, then)
}

// Title of the choice or of the thing named with the value, the value itself when none has it
func title(setting *configv1.Setting, value string) string {
	for _, listed := range [][]*configv1.Option{setting.GetOptions(), setting.GetNames()} {
		for _, option := range listed {
			if option.GetValue() == value && option.GetTitle() != "" {
				return option.GetTitle()
			}
		}
	}
	return value
}

// What typing a value for the setting is told under its field: what its help says, what an empty field means, its bounds, what it counts
// and how a length of time is written
func guide(setting *configv1.Setting) string {
	var lines []string
	if help := setting.GetHelp(); help != "" {
		lines = append(lines, help)
	}
	kind := setting.GetKind()
	empty := ""
	switch {
	case setting.GetSecret():
	case setting.GetUnset() != "":
		empty = strs.Capital(setting.GetUnset())
	case setting.GetDefaultShown() != config.NotSet && setting.GetDefaultShown() != config.None:
		empty = setting.GetDefaultShown()
	}
	if empty != "" {
		lines = append(lines, "Leave empty for: "+empty+".")
	}
	if zero := setting.GetZero(); zero != "" && zero != empty && (kind == configv1.Kind_KIND_NUMBER || kind == configv1.Kind_KIND_DURATION) {
		lines = append(lines, text(setting.GetZeroValue())+" means: "+zero+".")
	}
	if bounds := setting.GetBounds(); bounds != "" {
		lines = append(lines, bounds+".")
	}
	if starts := setting.GetStarts(); len(starts) > 0 {
		lines = append(lines, "It starts with "+strs.Or(starts)+".")
	}
	if path := setting.GetPath(); path != nil {
		lines = append(lines, pathWords(path))
	}
	switch {
	case setting.GetSecret() && setting.GetValue() != "":
		lines = append(lines, "The value kept now is never shown. Leave empty to keep it.")
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

// What is wrong with text typed for a number or a length of time before it is sent, empty when it may be: an empty field, which puts the
// default back, else not a number, or outside the bounds of the setting unless it is the value its zero words speak of
func check(setting *configv1.Setting, typed string) string {
	typed = strings.TrimSpace(typed)
	var number float64
	switch setting.GetKind() {
	case configv1.Kind_KIND_NUMBER:
		if typed == "" {
			return ""
		}
		n, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return "Enter a number."
		}
		number = n
	case configv1.Kind_KIND_DURATION:
		if typed == "" {
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
			return "Must start with " + strs.Or(starts) + "."
		}
		return ""
	}
	if zero, ok := decode(setting.GetZeroValue()).(int); ok && setting.GetZero() != "" && number == float64(zero) {
		return ""
	}
	least, low := bound(setting.GetLeast())
	most, high := bound(setting.GetMost())
	if (low && number < least) || (high && number > most) {
		return "Must be " + strs.Lower(setting.GetBounds()) + "."
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

// Form to type the value of a setting into: text, a number, a length of time or a secret. The field holds only what was typed into it
// before, what an empty field means said under it with the bounds and how to write it, and a value that can't be right is refused under it
func (s *settings) field(setting *configv1.Setting) *cards.Form {
	value := ""
	if setting.GetSource() == configv1.Source_SOURCE_FILE {
		value = text(setting.GetValue())
	}
	// What the setting is, then what an empty field means and how to write it, all under the field and none of it above
	field := cards.Field{ID: "value", Label: setting.GetTitle(), Value: value, Help: strings.TrimSpace(setting.GetDescription() + "\n" + guide(setting)),
		Select: true, Check: func(typed string) string { return check(setting, typed) }, Path: pathOf(setting)}
	switch {
	case setting.GetSecret():
		field.Kind, field.Value = cards.FieldSecret, ""
	case setting.GetKind() == configv1.Kind_KIND_NUMBER:
		field.Kind = cards.FieldNumber
	case setting.GetKind() == configv1.Kind_KIND_DURATION:
		field.Value = length(field.Value, setting.GetUnit())
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:   setting.GetTitle(),
		Fields:  []cards.Field{field},
		Submit:  "Save",
		Context: fieldContext,
		Actions: map[string]func(*cards.Form) tea.Cmd{
			"settings.default": func(*cards.Form) tea.Cmd { return s.save(setting, setting.GetKey(), "null", form, "") },
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			typed := strings.TrimSpace(values["value"])
			value := strconv.Quote(typed)
			switch {
			case setting.GetSecret() && typed == "":
				// Nothing typed keeps the secret as it is
				return s.hub.Finish(form)
			case typed == "":
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

// Saves a value written in YAML under the key, the setting's own or one of its entries, null putting the default back. A view it came from
// is the job, which finishes once the value is kept
func (s *settings) save(setting *configv1.Setting, key, value string, from wm.Card, told string) tea.Cmd {
	return s.keep(setting, map[string]string{key: value}, from, from != nil, told, nil)
}

// Saves values written in YAML by key, each the setting's own, one of a setting beside it or one of its entries, and tells the app the
// values the settings hold now. The note says what changed, told when given and else the setting's new value, and when the change takes
// hold. A job that ends with the save finishes: the view it came from closes, or the menu the person came straight into for it. A refusal
// shows under the field of the form it came from. Then runs once the values are kept
func (s *settings) keep(setting *configv1.Setting, values map[string]string, from wm.Card, ends bool, told string, then func()) tea.Cmd {
	if s.hub != nil && s.hub.Shown() {
		s.hub.Busy("Saving…")
	}
	keys := slices.Sorted(maps.Keys(values))
	// What is kept before what is taken out, so a thing that moves is never lost on the way, and the setting's own value last
	slices.SortStableFunc(keys, func(a, b string) int {
		return cmp.Compare(order(setting, a, values[a]), order(setting, b, values[b]))
	})
	var reads []string
	for _, each := range s.group(setting) {
		reads = append(reads, each.GetKey())
	}
	// A field of one entry of a table changes the table too, read again after it so what the table holds is never older than its fields
	if table := setting.GetEntryOf(); table != "" {
		reads = append(reads, table)
	}
	return tui.Async(func() func() tea.Cmd {
		ctx := s.kit.Env.Ctx
		var err error
		for _, key := range keys {
			if _, err = s.service.Set(ctx, &configv1.SetRequest{Profile: s.named, Key: key, Value: values[key]}); err != nil {
				break
			}
		}
		var read []*configv1.Setting
		if err == nil {
			for _, key := range reads {
				response, failed := s.service.Get(ctx, &configv1.GetRequest{Profile: s.named, Key: key})
				if failed != nil {
					err = failed
					break
				}
				read = append(read, response.GetSetting())
			}
		}
		return func() tea.Cmd {
			s.writes++
			if err != nil {
				if form, ok := from.(*cards.Form); ok && form == s.typed && s.hub != nil && s.hub.Shown() {
					s.hub.Quiet()
					form.SetError("value", tui.Reason(err))
					return nil
				}
				return s.fail(tui.Reason(err))
			}
			for _, now := range read {
				for i, have := range s.all {
					if have.GetKey() == now.GetKey() {
						s.all[i] = now
					}
				}
			}
			s.views.Refresh()
			if then != nil {
				then()
			}
			now := read[0]
			if told == "" {
				told = now.GetTitle() + " is now " + s.shown(now) + "."
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
			for _, each := range read {
				if s.named == "" || s.global(each.GetSection()) {
					cmds = append(cmds, s.kit.Changed(each.GetKey(), each.GetValue()))
				}
			}
			// A table of things changed: the fields of its entries are read again, those of a thing added or renamed among them; the pages
			// inside the areas are read again, as a change may add or rename one
			if things(setting) {
				cmds = append(cmds, s.read(nil))
			} else if s.again != nil {
				cmds = append(cmds, s.again())
			}
			if s.hub == nil || !s.hub.Shown() {
				return tea.Batch(append(cmds, s.kit.WM.Toast(told, wm.Success, wm.DefaultToastTTL))...)
			}
			cmds = append(cmds, s.hub.Done(told))
			switch {
			case ends && from == nil:
				cmds = append(cmds, s.hub.Picked())
			case ends:
				cmds = append(cmds, s.hub.Finish(from))
			}
			return tea.Batch(cmds...)
		}
	})
}

// Place of a write among those kept together: values first, then what is taken out, then the setting's own key
func order(setting *configv1.Setting, key, value string) int {
	switch {
	case key == setting.GetKey():
		return 2
	case value == "null":
		return 1
	}
	return 0
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

// Kind of path the text of a setting names, which tab completes in its field
func pathOf(setting *configv1.Setting) cards.PathKind {
	switch setting.GetPath().GetKind() {
	case configv1.PathKind_PATH_KIND_FILE:
		return cards.PathFile
	case configv1.PathKind_PATH_KIND_FOLDER:
		return cards.PathFolder
	case configv1.PathKind_PATH_KIND_PROGRAM:
		return cards.PathProgram
	}
	return cards.PathNone
}

// What to type for a path, under its field: the whole path or the name of a program, that tab completes it, and that it must be there
func pathWords(path *configv1.Path) string {
	said := "The whole path of the file, starting with / or ~. Tab completes it."
	switch path.GetKind() {
	case configv1.PathKind_PATH_KIND_FOLDER:
		said = "The whole path of the folder, starting with / or ~. Tab completes it."
	case configv1.PathKind_PATH_KIND_PROGRAM:
		said = "The name of a program on PATH, or its whole path. Tab completes it."
	}
	if path.GetExists() {
		said += " It must be there already."
	}
	return said
}
