package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Whether a table's entries are things of several named fields, each changed on the entry's own page
func things(setting *configv1.Setting) bool {
	return slices.ContainsFunc(setting.GetEntry().GetFields(), func(field *configv1.Setting) bool { return field.GetKey() != "" })
}

// The row that opens the advanced fields of one entry and the list it opens, false when they are too few to go behind a row
func (s *settings) entryAdvanced(table, name string) ([]string, func() tea.Cmd, bool) {
	own, rare := s.fields(table, name)
	if !apart(own, rare) {
		return nil, nil, false
	}
	return s.advancedCells(rare), func() tea.Cmd { return s.entryBehind(table, name) }, true
}

// Opens the advanced fields of one entry over the hub
func (s *settings) entryBehind(table, name string) tea.Cmd {
	said := s.thingName(table, name)
	about := "Settings of " + said + " that few people change"
	return s.hub.Push(s.list("Advanced", "setting:"+table+":"+name+":advanced", about, "Search in "+said, func(bool) []row {
		_, rare := s.fields(table, name)
		return rare
	}))
}

// Name of one entry of a table as a person reads it
func (s *settings) thingName(table, name string) string {
	setting := s.find(table)
	return s.entryName(setting, setting.GetEntry(), name, true)
}

// Row of the page of a thing: one of its fields, the row of those few people change, renaming it or removing it
type thingRow struct {
	setting          *configv1.Setting
	advanced         bool
	rename, removing bool
}

// Page of one entry of a table of things: its fields as option rows with the controls of their kinds, those few people change behind
// Advanced, then Rename and Remove. Moved is told the new name once the thing is renamed
func (s *settings) thing(setting *configv1.Setting, name string, moved func(to string)) *cards.List {
	t := s.kit.Env.Theme
	table := setting.GetKey()
	said := func() string { return s.thingName(table, name) }
	var shown []thingRow
	var list *cards.List
	list = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   said(),
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + table + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search in " + said(),
		Intro:   func() []string { return []string{setting.GetDescription()} },
		Columns: Columns,
		Rows: func() [][]string {
			own, rare := s.fields(table, name)
			shown = shown[:0]
			for _, r := range own {
				shown = append(shown, thingRow{setting: r.setting})
			}
			if apart(own, rare) {
				shown = append(shown, thingRow{advanced: true})
			} else {
				for _, r := range rare {
					shown = append(shown, thingRow{setting: r.setting})
				}
			}
			shown = append(shown, thingRow{rename: true}, thingRow{removing: true})
			cells := make([][]string, len(shown))
			for i, r := range shown {
				switch {
				case r.setting != nil:
					cells[i] = s.cells(row{setting: r.setting})
				case r.advanced:
					cells[i] = s.advancedCells(rare)
				case r.rename:
					cells[i] = []string{"Rename", "", ""}
				default:
					cells[i] = []string{"Remove", "", ""}
				}
			}
			return cells
		},
		Words: func(i int) string {
			if r := shown[i]; r.setting != nil {
				return s.wordsOf(row{setting: r.setting})
			}
			return ""
		},
		Detail: func(i int) []string {
			switch r := shown[i]; {
			case r.setting != nil:
				return s.detail(row{setting: r.setting}, list.Cut(i, 2))
			case r.advanced:
				return []string{"Settings of " + said() + " that few people change"}
			case r.rename:
				return []string{"Give " + said() + " another name."}
			}
			return []string{"Take " + said() + " out of " + setting.GetTitle() + "."}
		},
		Empty: func() string { return tui.Waiting(s.loaded, s.err, said()+" is no longer there.") },
		Free:  map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.setting != nil:
				return s.hints([]row{{setting: r.setting}}, 0)
			case r.advanced:
				return []wm.Hint{{Action: "hub.use", Label: "open"}}
			case r.rename:
				return []wm.Hint{{Action: "hub.use", Label: "rename"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch r := shown[i]; {
				case r.setting != nil:
					return s.change(r.setting)
				case r.advanced:
					return s.entryBehind(table, name)
				case r.rename:
					return s.renameThing(setting, name, list, moved)
				}
				l.Confirm(cards.Question{Text: "Remove " + said() + "?", Verb: "Remove", Deletes: true}, func() tea.Cmd {
					return tea.Batch(s.hub.Close(list), s.write(setting, "", name, nil, nil, "Removed "+said()+"."))
				})
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if r := shown[i]; r.setting != nil {
					return s.reset(l, r.setting)
				}
				return nil
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.read(nil) },
		},
	})
	list.ToggleDetail()
	s.views.Add(list)
	return list
}

// Gives a thing another name: what it holds kept under the new name first, then the old name taken out, its page closing and moved told
// the new name once both are kept. A table named after things renames to another of them, picked as one is added, and a table that knows
// its names to another of those
func (s *settings) renameThing(setting *configv1.Setting, name string, page *cards.List, moved func(to string)) tea.Cmd {
	schema, what, key := setting.GetEntry(), noun(setting), setting.GetKey()
	held := func() map[string]any {
		entries, _ := decode(s.current(setting).GetValue()).(map[string]any)
		return entries
	}
	taken := func(to string) string {
		if have, there := holding(schema, held(), to); there && have != name {
			return "There is " + article(what) + " " + what + " named " + s.thingName(key, have) + " already."
		}
		return ""
	}
	said := s.thingName(key, name)
	title := "Rename " + said
	move := func(to string) tea.Cmd {
		if to == "" || to == name {
			return nil
		}
		if why := taken(to); why != "" {
			return s.fail(why)
		}
		encoded, err := encode(held()[name])
		if err != nil {
			return s.fail(err.Error())
		}
		told := said + " is now called " + s.thingName(key, to) + "."
		values := map[string]string{key + "." + to: encoded, key + "." + name: "null"}
		// Where names ignore case, the new spelling of the same name is written over the old one, which removing it would remove
		if schema.GetIgnoreCase() && strings.EqualFold(to, name) {
			delete(values, key+"."+name)
		}
		// A name that holds a dot, which a key can't name, moves with the whole table written at once
		if strings.Contains(name+to, ".") {
			whole := cloned(held())
			whole[to] = whole[name]
			delete(whole, name)
			if encoded, err = encode(whole); err != nil {
				return s.fail(err.Error())
			}
			values = map[string]string{key: encoded}
		}
		return tea.Batch(s.hub.Close(page), s.keep(setting, values, nil, false, told, func() { moved(to) }))
	}
	if cmd, ok := s.pickName(setting, title, "Rename", move); ok {
		return cmd
	}
	if known := schema.GetNames(); len(known) > 0 {
		var choices []tui.Choice
		for _, option := range known {
			if _, there := held()[option.GetValue()]; !there {
				choices = append(choices, tui.Choice{ID: option.GetValue(), Title: optionTitle(setting, option), Detail: option.GetDescription()})
			}
		}
		if len(choices) == 0 {
			return s.hub.Say("Every "+what+" is in the list already, so "+said+" has no other name to take.", cards.NoteInfo)
		}
		return s.kit.Pick(s.hub, title, "Rename", choices, "", func(choice tui.Choice) tea.Cmd { return move(choice.ID) })
	}
	field := nameField(schema, what, func(typed string) string { return taken(strings.TrimSpace(typed)) })
	field.Help = "The new name of " + said + "."
	field.Value, field.Select = name, true
	return s.kit.Ask(s.hub, title, field, "Rename", func(typed string) tea.Cmd { return move(strings.TrimSpace(typed)) })
}

// Adds a thing to a table as a job: its name, picked from the things the table is named after, from the names it knows, or typed; then
// only what the thing can't do without: the typed fields in one form, each choice from its list, each field with a picker from that picker.
// The thing is kept once the last step is answered, and shown is told its name; a name the table holds already opens that thing's page, and
// so does a thing needing a field no step can ask, with that field to set
func (s *settings) addThing(setting *configv1.Setting, title string, held func() map[string]any, shown func(name string), open func(name string) tea.Cmd) tea.Cmd {
	schema := setting.GetEntry()
	what := noun(setting)
	flow := s.kit.Flow(s.hub, title)
	var typed, picked, chosen, later []*configv1.Setting
	for _, field := range schema.GetFields() {
		_, picker := s.kit.Picker(field.GetPicks())
		switch {
		case field.GetKey() == "" || !field.GetNeed():
		case picker:
			chosen = append(chosen, field)
		case field.GetKind() == configv1.Kind_KIND_CHOICE:
			picked = append(picked, field)
		case field.GetKind() == configv1.Kind_KIND_TEXT, field.GetKind() == configv1.Kind_KIND_NUMBER, field.GetKind() == configv1.Kind_KIND_DURATION:
			typed = append(typed, field)
		case field.GetKind() == configv1.Kind_KIND_LIST && len(field.GetOptions()) == 0:
			typed = append(typed, field)
		default:
			later = append(later, field)
		}
	}
	values := map[string]any{}
	finish := func(name string) tea.Cmd {
		entry := map[string]any{}
		for key, value := range values {
			entry[key] = value
		}
		// The settings file keeps no empty entry, so a thing with nothing asked is kept with what its fields hold by default
		if len(entry) == 0 {
			for _, field := range schema.GetFields() {
				if value := decode(field.GetDefaultValue()); field.GetKey() != "" && value != nil {
					entry[field.GetKey()] = value
				}
			}
		}
		said := s.entryName(setting, schema, name, true)
		told := "Added " + said + "."
		if len(later) == 0 {
			return tea.Batch(flow.End(), s.keepEntry(setting, name, entry, told, func() { shown(name) }))
		}
		// A field no step can ask is set on the thing's page, which opens once the thing is kept
		var titles []string
		for _, field := range later {
			titles = append(titles, field.GetTitle())
		}
		told = "Added " + said + ". Set " + strs.And(titles) + " here."
		return tea.Batch(flow.End(), s.keepEntry(setting, name, entry, told, func() { shown(name) }), open(name))
	}
	// Each field with a picker is picked from it, the values it hands back being fields of the entry by their keys
	var choosing func(name string, at int) tea.Cmd
	choosing = func(name string, at int) tea.Cmd {
		if at == len(chosen) {
			return finish(name)
		}
		field := chosen[at]
		picker, _ := s.kit.Picker(field.GetPicks())
		key := field.GetKey()
		current := map[string]string{key: ""}
		if picker.Beside != nil {
			for _, other := range picker.Beside(key) {
				current[other] = ""
			}
		}
		var first []tui.Choice
		if zero := field.GetZero(); zero != "" {
			first = []tui.Choice{{Title: zero}}
		}
		verb := ""
		if at == len(chosen)-1 {
			verb = "Add"
		}
		return picker.Open(s.kit, s.hub, tui.Ask{Title: field.GetTitle(), Intro: []string{field.GetDescription()}, Key: key, Values: current,
			First: first, Profile: s.named, Verb: verb, Done: func(answers map[string]string) tea.Cmd {
				for other, answer := range answers {
					if value := decode(answer); value != nil {
						values[other] = value
					} else {
						delete(values, other)
					}
				}
				return choosing(name, at+1)
			}})
	}
	var choose func(name string, at int) tea.Cmd
	choose = func(name string, at int) tea.Cmd {
		if at == len(picked) {
			return choosing(name, 0)
		}
		field := picked[at]
		var choices []tui.Choice
		for _, option := range field.GetOptions() {
			choices = append(choices, tui.Choice{ID: option.GetValue(), Title: optionTitle(field, option), Detail: option.GetDescription()})
		}
		verb := ""
		if at == len(picked)-1 && len(chosen) == 0 {
			verb = "Add"
		}
		return flow.PickVerb(field.GetDescription(), choices, text(field.GetDefaultValue()), verb, func(choice tui.Choice) tea.Cmd {
			values[field.GetKey()] = choice.ID
			return choose(name, at+1)
		})
	}
	ask := func(name string) tea.Cmd {
		if _, taken := held()[name]; taken {
			return open(name)
		}
		if len(typed) == 0 {
			return choose(name, 0)
		}
		fields := make([]cards.Field, len(typed))
		for i, field := range typed {
			fields[i] = entryField(field, field.GetKey(), nil)
			fields[i].Required = true
		}
		verb := "Add"
		if len(picked)+len(chosen) > 0 {
			verb = "Next"
		}
		return flow.Ask("", fields, verb, func(answers map[string]string) tea.Cmd {
			for _, field := range typed {
				if value := fieldValue(field, answers[field.GetKey()], nil); value != nil {
					values[field.GetKey()] = value
				}
			}
			return choose(name, 0)
		})
	}
	// The name is the last step when the thing needs nothing else, so its button says the job's verb
	last := ""
	if len(typed)+len(picked)+len(chosen) == 0 {
		last = "Add"
	}
	if cmd, ok := s.pickName(setting, title, last, ask); ok {
		return cmd
	}
	if known := schema.GetNames(); len(known) > 0 {
		var choices []tui.Choice
		for _, option := range known {
			if _, taken := held()[option.GetValue()]; !taken {
				choices = append(choices, tui.Choice{ID: option.GetValue(), Title: optionTitle(setting, option), Detail: option.GetDescription()})
			}
		}
		if len(choices) == 0 {
			return s.hub.Say("Every "+what+" is in the list already.", cards.NoteInfo)
		}
		return flow.PickVerb("Which "+what+" to add.", choices, "", last, func(choice tui.Choice) tea.Cmd { return ask(choice.ID) })
	}
	name := nameField(schema, what, func(typed string) string {
		if have, taken := holding(schema, held(), strings.TrimSpace(typed)); taken {
			return "There is " + article(what) + " " + what + " named " + s.entryName(setting, schema, have, true) + " already."
		}
		return ""
	})
	return flow.Ask("", []cards.Field{name}, "Next", func(answers map[string]string) tea.Cmd { return ask(strings.TrimSpace(answers["name"])) })
}

// Title of an option of a setting, its value when it has none
func optionTitle(setting *configv1.Setting, option *configv1.Option) string {
	if option.GetTitle() != "" {
		return option.GetTitle()
	}
	return title(setting, option.GetValue())
}

// What a thing holds, in a few words for its row: the fields it sets, each by its title and its value in words; empty before its fields
// were read
func (s *settings) thingSummary(table, name string) string {
	own, rare := s.fields(table, name)
	var parts []string
	for _, r := range append(own, rare...) {
		if r.setting.GetSource() != configv1.Source_SOURCE_DEFAULT {
			parts = append(parts, r.setting.GetTitle()+": "+s.shown(r.setting))
		}
	}
	// A thing whose fields are all as by default says how many it has, as the row of a section does
	if len(parts) == 0 && len(own)+len(rare) > 0 {
		return s.tally(append(own, rare...))
	}
	if len(parts) > namedFields {
		parts = append(parts[:namedFields:namedFields], "+"+strconv.Itoa(len(parts)-namedFields)+" more")
	}
	return strings.Join(parts, " · ")
}

// What resetting a setting writes: null, but for the last field an entry holds the field's default, as the settings file keeps no empty
// entry and the thing would be gone
func (s *settings) cleared(setting *configv1.Setting) string {
	table := s.find(setting.GetEntryOf())
	if table == nil {
		return "null"
	}
	// The key of the field is the table's key, the name of the entry and the field's key, each spelled as the settings keep it
	rest, below := strings.CutPrefix(setting.GetKey(), table.GetKey()+".")
	at := strings.LastIndex(rest, ".")
	if !below || at < 0 {
		return "null"
	}
	name, field := rest[:at], rest[at+1:]
	entries, _ := decode(s.current(table).GetValue()).(map[string]any)
	entry, held := entries[name].(map[string]any)
	if !held {
		return "null"
	}
	for key := range entry {
		if !strings.EqualFold(key, field) {
			return "null"
		}
	}
	return cmp.Or(setting.GetDefaultValue(), "null")
}
