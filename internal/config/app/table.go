package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What one entry of the table is called: the noun its setting gives, entry for a table that gives none
func noun(setting *configv1.Setting) string {
	if name := setting.GetEntry().GetNoun(); name != "" {
		return name
	}
	return "entry"
}

// Entries of a table setting by name, each changed, added or removed on the hub, the path naming a group of values inside an entry of a
// table whose entries are free. An entry is changed in the fields its setting declares; one of a table that declares none is one value
// typed as text, or a group of values opened as a list of its own
func (s *settings) entries(setting *configv1.Setting, path string) *cards.List {
	t := s.kit.Env.Theme
	schema := setting.GetEntry()
	if path != "" {
		schema = nil
	}
	held := func() map[string]any {
		whole := decode(s.current(setting).GetValue())
		if path != "" {
			whole = config.Dig(whole, path)
		}
		entries, _ := whole.(map[string]any)
		return entries
	}
	what := noun(setting)
	if path != "" {
		what = "value"
	}
	heading := setting.GetTitle()
	if path != "" {
		heading = path[strings.LastIndex(path, ".")+1:]
	}
	said := func(name string) string { return s.entryName(setting, schema, name, path == "") }
	var names []string
	var view *cards.List
	// A thing of several named fields has a page of its own rows, one per field
	thing := path == "" && things(setting)
	inHand := func(name string) { view.Select(slices.Index(names, name)) }
	open := func(name string, adding bool) tea.Cmd {
		if thing {
			return s.hub.Push(s.thing(setting, name, inHand))
		}
		value := held()[name]
		// A group of values no field is declared for opens as a list of its own
		if group, grouped := value.(map[string]any); grouped && len(schema.GetFields()) == 0 && group != nil && !strings.Contains(name, ".") {
			return s.hub.Push(s.entries(setting, strings.TrimPrefix(path+"."+name, ".")))
		}
		put := func(next any, from wm.Card, told string) tea.Cmd {
			return s.write(setting, path, name, next, from, told)
		}
		// An entry that is a list of records opens as that list
		if fields := schema.GetFields(); len(fields) == 1 && fields[0].GetKey() == "" && recorded(fields[0]) {
			return s.hub.Push(s.records(setting.GetKey()+":"+path+":"+name, said(name), fields[0].GetDescription(), fields[0].GetEntry(),
				func() []any {
					items, _ := held()[name].([]any)
					return items
				}, func(next []any, from wm.Card, told string) tea.Cmd { return put(next, from, told) }))
		}
		return s.hub.Push(s.entry(schema, said(name), value, adding, put))
	}
	adding := "Add " + article(what) + " " + what
	add := func() tea.Cmd {
		if thing {
			return s.addThing(setting, adding, held, inHand, func(name string) tea.Cmd { return open(name, false) })
		}
		// A table whose entries are named after things picks the name from those things; the names inside a group of values are free, and
		// asking for the picker opens it, so it is asked for the table's own entries alone
		if path == "" {
			if cmd, ok := s.pickName(setting, adding, "", func(name string) tea.Cmd {
				_, taken := held()[name]
				return open(name, !taken)
			}); ok {
				return cmd
			}
		}
		// A table that knows the names its entries may have offers the ones it does not hold yet
		if known := schema.GetNames(); len(known) > 0 {
			var choices []tui.Choice
			for _, option := range known {
				if _, taken := held()[option.GetValue()]; !taken {
					choices = append(choices, tui.Choice{ID: option.GetValue(), Title: cmp.Or(option.GetTitle(), option.GetValue()), Detail: option.GetDescription()})
				}
			}
			if len(choices) == 0 {
				return s.hub.Say("Every "+what+" is in the list already.", cards.NoteInfo)
			}
			return s.kit.Pick(s.hub, adding, "choose", choices, "", func(choice tui.Choice) tea.Cmd { return open(choice.ID, true) })
		}
		return s.kit.Ask(s.hub, adding, nameField(schema, what, nil), "Next", func(name string) tea.Cmd {
			name = strings.TrimSpace(name)
			if have, taken := holding(schema, held(), name); taken {
				return open(have, false)
			}
			return open(name, true)
		})
	}
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   heading,
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey() + ":" + path,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search in " + heading,
		Intro: func() []string {
			about := setting.GetDescription()
			if path != "" {
				about = ""
			}
			return s.opening(about, what, heading, len(held()) == 0)
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows: func() [][]string {
			entries := held()
			names = slices.Sorted(maps.Keys(entries))
			rows := make([][]string, len(names), len(names)+1)
			for i, name := range names {
				words := summary(schema, entries[name])
				if thing {
					words = cmp.Or(s.thingSummary(setting.GetKey(), name), words)
				}
				rows[i] = []string{said(name), render.Dim(words, t)}
			}
			return append(rows, []string{adding, render.Dim(t.Glyphs.Forward, t)})
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add"}}
			case i >= len(names):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			// A thing opens a page of its own; any other entry is changed at once
			verb := "change"
			if thing {
				verb = "open"
			}
			return []wm.Hint{{Action: "hub.use", Label: verb}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(names) {
					return add()
				}
				return open(names[i], false)
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return add() },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(names) {
					return nil
				}
				name := names[i]
				l.Confirm(cards.Question{Text: "Remove " + said(name) + "?", Verb: "Remove", Deletes: true},
					func() tea.Cmd { return s.write(setting, path, name, nil, nil, "Removed "+said(name)+".") })
				return nil
			},
		},
	})
	s.views.Add(view)
	return view
}

// Field a new entry of a table is named in, under what its table calls a name. A name that can't be one part of a key is refused first, then
// check refuses a name the table holds already
func nameField(schema *configv1.Entry, what string, check func(typed string) string) cards.Field {
	return cards.Field{ID: "name", Label: cmp.Or(schema.GetName(), strs.Capital(what)+" name"), Help: "The name of the new " + what + ".", Required: true,
		Check: func(typed string) string {
			if err := config.KeyName(typed); err != nil {
				return err.Error()
			}
			if check == nil {
				return ""
			}
			return check(typed)
		}}
}

// Name the table holds that a name typed for it names: the same name, or where the table's names ignore case the first in order of those
// that differ from it only in case; false when it holds none
func holding(schema *configv1.Entry, held map[string]any, name string) (string, bool) {
	if _, ok := held[name]; ok {
		return name, true
	}
	if schema.GetIgnoreCase() {
		for _, have := range slices.Sorted(maps.Keys(held)) {
			if strings.EqualFold(have, name) {
				return have, true
			}
		}
	}
	return "", false
}

// Writes one entry of a table, nil removing it, under its own key so a part of the table the administrator pins is refused on its own and
// the rest of the table is never copied over. An entry whose name holds a dot, which a key can't name, is written with the whole table
func (s *settings) write(setting *configv1.Setting, path, name string, value any, from wm.Card, told string) tea.Cmd {
	key := setting.GetKey() + "." + strings.TrimPrefix(path+"."+name, ".")
	if strings.Contains(name, ".") {
		whole, _ := decode(s.current(setting).GetValue()).(map[string]any)
		whole = cloned(whole)
		group := whole
		if path != "" {
			for _, part := range strings.Split(path, ".") {
				next, _ := group[part].(map[string]any)
				next = cloned(next)
				group[part], group = next, next
			}
		}
		if value == nil {
			delete(group, name)
		} else {
			group[name] = value
		}
		key, value = setting.GetKey(), whole
	}
	encoded := "null"
	if value != nil {
		var err error
		if encoded, err = encode(value); err != nil {
			return s.fail(err.Error())
		}
	}
	return s.save(setting, key, encoded, from, told)
}

// Copy of a group of values that can be changed, an empty one for none
func cloned(group map[string]any) map[string]any {
	if group == nil {
		return map[string]any{}
	}
	return maps.Clone(group)
}

// Form of one entry of a table or one record of a list, titled as it is said: the fields its setting declares, or one field for an entry
// that is a single value. A field left empty is left out of the entry, so what it does by default applies; a secret left empty keeps the
// value it has. Put writes what the form holds
func (s *settings) entry(schema *configv1.Entry, said string, value any, adding bool, put func(value any, from wm.Card, told string) tea.Cmd) *cards.Form {
	submit, told := "Save", "Saved "+said+"."
	if adding {
		submit, told = "Add", "Added "+said+"."
	}
	declared := schema.GetFields()
	single := len(declared) == 1 && declared[0].GetKey() == ""
	group, _ := value.(map[string]any)
	var fields []cards.Field
	switch {
	case single:
		fields = []cards.Field{entryField(declared[0], "value", value)}
		fields[0].Label = said
	case len(declared) > 0:
		for _, field := range declared {
			fields = append(fields, entryField(field, field.GetKey(), group[field.GetKey()]))
		}
	default:
		fields = []cards.Field{{ID: "value", Label: strs.Capital(cmp.Or(schema.GetNoun(), "entry")), Value: entryText(value),
			Help: "Text, a number, or true or false. A list or a group of values is written as JSON, such as [\"a\", \"b\"] or {\"name\": \"value\"}."}}
	}
	var form *cards.Form
	form = cards.NewForm(s.kit.Env, cards.FormSpec{
		Title:  said,
		Fields: fields,
		Submit: submit,
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			// A list that names its values is ticked row by row, what is ticked going back into the field
			for _, declared := range declared {
				if id := cmp.Or(declared.GetKey(), "value"); id == field.ID && declared.GetKind() == configv1.Kind_KIND_LIST && len(declared.GetOptions()) > 0 {
					return s.hub.Push(s.ticks(declared, split(f.Values()[field.ID]), func(ticked []string) tea.Cmd {
						return f.Commit(field.ID, strings.Join(ticked, ", "))
					}))
				}
			}
			var choices []tui.Choice
			for _, choice := range field.Choices {
				choices = append(choices, tui.Choice{ID: choice.Value, Title: choice.Label, Detail: choice.Meaning})
			}
			return s.kit.Pick(s.hub, field.Label, "choose", choices, field.Value, func(choice tui.Choice) tea.Cmd {
				return f.Commit(field.ID, choice.ID)
			})
		},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			switch {
			case single:
				return put(fieldValue(declared[0], values["value"], value), form, told)
			case len(declared) == 0:
				return put(entryValue(values["value"]), form, told)
			}
			next := cloned(group)
			for _, field := range declared {
				if kept := fieldValue(field, values[field.GetKey()], group[field.GetKey()]); kept != nil {
					next[field.GetKey()] = kept
				} else {
					delete(next, field.GetKey())
				}
			}
			return put(next, form, told)
		},
	})
	return form
}

// Titles of the values of a list that names its values, as a row or a field says them
func titles(field *configv1.Setting, values []string) string {
	said := make([]string, len(values))
	for i, value := range values {
		said[i] = title(field, value)
	}
	return strings.Join(said, ", ")
}

// What an entry holds for a declared field, in a few words: a secret hidden, the values of a list that names them by their titles
func fieldWords(field *configv1.Setting, value any) string {
	switch items, listed := value.([]any); {
	case field.GetSecret():
		return config.Hidden
	case listed && len(field.GetOptions()) > 0 && len(items) > 0:
		values := make([]string, len(items))
		for i, item := range items {
			values[i] = fmt.Sprint(item)
		}
		return titles(field, values)
	}
	return entryWords(value)
}

// Whether a list setting or field holds records, each a group of declared fields, rather than text
func recorded(setting *configv1.Setting) bool {
	return setting.GetKind() == configv1.Kind_KIND_LIST && len(setting.GetEntry().GetFields()) > 0
}

// Items of text typed with commas between them
func split(typed string) []string {
	var items []string
	for _, item := range strings.Split(typed, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// Records of a list, each a group of the fields the list declares, changed, added or removed on the hub. Key names the view, heading and
// about say what the list is, held reads the records as they are now and put writes them all
func (s *settings) records(key, heading, about string, schema *configv1.Entry, held func() []any, put func(next []any, from wm.Card, told string) tea.Cmd) *cards.List {
	t := s.kit.Env.Theme
	what := cmp.Or(schema.GetNoun(), "entry")
	// What a record is called in a note or a question: what its first field holds, else its place in the list
	named := func(i int, record any) string {
		if group, ok := record.(map[string]any); ok && len(schema.GetFields()) > 0 {
			if text, ok := group[schema.GetFields()[0].GetKey()].(string); ok && strings.TrimSpace(text) != "" {
				return text
			}
		}
		return what + " " + strconv.Itoa(i+1)
	}
	open := func(i int) tea.Cmd {
		records := held()
		adding := i >= len(records)
		var record any
		said := "Add " + article(what) + " " + what
		if !adding {
			record, said = records[i], named(i, records[i])
		}
		return s.hub.Push(s.entry(schema, said, record, adding, func(value any, from wm.Card, _ string) tea.Cmd {
			next := slices.Clone(held())
			told := "Saved " + named(i, value) + "."
			if i >= len(next) {
				next, told = append(next, value), "Added "+named(i, value)+"."
			} else {
				next[i] = value
			}
			return put(next, from, told)
		}))
	}
	view := cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   heading,
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + key,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return s.opening(about, schema.GetNoun(), heading, len(held()) == 0) },
		Columns: []render.Column{{Right: true}, {Flex: 1}},
		Rows: func() [][]string {
			records := held()
			rows := make([][]string, len(records), len(records)+1)
			for i, record := range records {
				rows[i] = []string{render.Dim(strconv.Itoa(i+1), t), summary(schema, record)}
			}
			return append(rows, []string{"", s.adder("Add " + article(what) + " " + what)})
		},
		Detail: func(i int) []string {
			if i >= len(held()) {
				return nil
			}
			group, _ := held()[i].(map[string]any)
			var lines []string
			for _, field := range schema.GetFields() {
				if value, set := group[field.GetKey()]; set {
					lines = append(lines, field.GetTitle()+": "+fieldWords(field, value))
				}
			}
			return lines
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add"}}
			case i >= len(held()):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return open(min(i, len(held()))) },
			"hub.add": func(*cards.List, int) tea.Cmd { return open(len(held())) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(held()) {
					return nil
				}
				name := named(i, held()[i])
				l.Confirm(cards.Question{Text: "Remove " + name + "?", Verb: "Remove", Deletes: true}, func() tea.Cmd {
					next := slices.Delete(slices.Clone(held()), i, i+1)
					return put(next, nil, "Removed "+name+".")
				})
				return nil
			},
		},
	})
	view.ToggleDetail()
	s.views.Add(view)
	return view
}

// Values a list field names, each ticked or not, enter ticking the highlighted one; leaving the list hands what is ticked back
func (s *settings) ticks(field *configv1.Setting, ticked []string, done func(ticked []string) tea.Cmd) *cards.List {
	t := s.kit.Env.Theme
	options := field.GetOptions()
	var view *cards.List
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   field.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     "ticks:" + field.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     len(options) > 8,
		Search:  "Search in " + field.GetTitle(),
		Intro:   func() []string { return []string{field.GetDescription(), "Tick the ones you want, then go back."} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(options))
			for i, option := range options {
				box := "[ ]"
				if slices.Contains(ticked, option.GetValue()) {
					box = "[x]"
				}
				rows[i] = []string{box, title(field, option.GetValue()), render.Dim(option.GetDescription(), t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if view.Shows(2) || options[i].GetDescription() == "" {
				return nil
			}
			return []string{options[i].GetDescription()}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			if slices.Contains(ticked, options[i].GetValue()) {
				return []wm.Hint{{Action: "hub.use", Label: "untick"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "tick"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				value := options[i].GetValue()
				if at := slices.Index(ticked, value); at >= 0 {
					ticked = slices.Delete(ticked, at, at+1)
				} else {
					ticked = append(ticked, value)
				}
				l.Refresh()
				// The field holds the ticks at once, in the order the list names its values
				var ordered []string
				for _, option := range options {
					if slices.Contains(ticked, option.GetValue()) {
						ordered = append(ordered, option.GetValue())
					}
				}
				return done(ordered)
			},
		},
	})
	view.ToggleDetail()
	return view
}

// Field of a form for one declared field of an entry, holding the value the entry has for it
func entryField(field *configv1.Setting, id string, value any) cards.Field {
	// Every field has its line under it: what it holds, then what an empty field means and how to write it
	help := strings.TrimSpace(field.GetDescription() + "\n" + guide(field))
	if help == "" {
		help = "The value of " + field.GetTitle() + "."
	}
	out := cards.Field{ID: id, Label: field.GetTitle(), Help: help, Required: field.GetNeed(), Check: func(typed string) string {
		if strings.TrimSpace(typed) == "" {
			return ""
		}
		return check(field, typed)
	}}
	switch {
	case field.GetSecret():
		out.Kind = cards.FieldSecret
	case field.GetKind() == configv1.Kind_KIND_TOGGLE:
		out.Kind = cards.FieldBool
		on, _ := value.(bool)
		out.Value = strconv.FormatBool(on)
	case field.GetKind() == configv1.Kind_KIND_CHOICE:
		out.Kind = cards.FieldChoice
		out.Value, _ = value.(string)
		for _, option := range field.GetOptions() {
			out.Choices = append(out.Choices, cards.Choice{Value: option.GetValue(), Label: title(field, option.GetValue()), Meaning: option.GetDescription()})
		}
	case field.GetKind() == configv1.Kind_KIND_LIST:
		items, _ := value.([]any)
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = fmt.Sprint(item)
		}
		out.Value = strings.Join(parts, ", ")
		if len(field.GetOptions()) > 0 {
			// Ticked from the values the list names, the form opening them, and shown by their titles
			out.Kind, out.Picked, out.Placeholder = cards.FieldChoice, true, config.None
			out.Says = func(value string) string { return titles(field, split(value)) }
		} else {
			out.Help = strings.TrimSpace(out.Help + "\nSeparate several with commas.")
		}
	case field.GetKind() == configv1.Kind_KIND_TABLE:
		out.Value = entryText(value)
		out.Help = strings.TrimSpace(out.Help + "\nA group of values, written as JSON such as {\"name\": \"value\"}.")
	default:
		out.Value = entryText(value)
	}
	return out
}

// Value an entry keeps for a declared field from the text of its form field, nil for a field left empty, which the entry then leaves out. A
// secret left empty keeps the value it had
func fieldValue(field *configv1.Setting, typed string, had any) any {
	typed = strings.TrimSpace(typed)
	switch {
	case field.GetKind() == configv1.Kind_KIND_TOGGLE:
		// A switch left at its default is left out of the entry, like any field left as it is
		if on, _ := decode(field.GetDefaultValue()).(bool); on == (typed == "true") {
			return nil
		}
		return typed == "true"
	case typed == "" && field.GetSecret():
		return had
	case typed == "":
		return nil
	}
	switch field.GetKind() {
	case configv1.Kind_KIND_NUMBER:
		if whole, err := strconv.Atoi(typed); err == nil {
			return whole
		}
		if number, err := strconv.ParseFloat(typed, 64); err == nil {
			return number
		}
	case configv1.Kind_KIND_DURATION:
		if number, err := strconv.ParseFloat(typed, 64); err == nil {
			return number
		}
	case configv1.Kind_KIND_LIST:
		var items []any
		for _, item := range strings.Split(typed, ",") {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
		return items
	case configv1.Kind_KIND_TABLE:
		return entryValue(typed)
	}
	return typed
}

// What an entry holds, in a few words for its row: its value, or the fields it sets, the declared ones by their titles
func summary(schema *configv1.Entry, value any) string {
	// An entry that is a list of records says how many it holds
	if fields := schema.GetFields(); len(fields) == 1 && fields[0].GetKey() == "" && recorded(fields[0]) {
		records, _ := value.([]any)
		if len(records) == 0 {
			return config.None
		}
		return render.Count(len(records), cmp.Or(fields[0].GetEntry().GetNoun(), "entry"))
	}
	group, grouped := value.(map[string]any)
	if !grouped {
		return entryWords(value)
	}
	var parts []string
	named := map[string]bool{}
	for _, field := range schema.GetFields() {
		held, ok := group[field.GetKey()]
		named[field.GetKey()] = true
		if ok {
			parts = append(parts, field.GetTitle()+": "+fieldWords(field, held))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(group)) {
		if !named[key] {
			parts = append(parts, key+": "+entryWords(group[key]))
		}
	}
	if len(parts) == 0 {
		return config.None
	}
	if len(parts) > namedFields {
		parts = append(parts[:namedFields:namedFields], "+"+strconv.Itoa(len(parts)-namedFields)+" more")
	}
	return strings.Join(parts, " · ")
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

// Value of an entry as a few words
func entryWords(value any) string {
	switch v := value.(type) {
	case nil:
		return config.NotSet
	case bool:
		if v {
			return "On"
		}
		return "Off"
	case string:
		if v == "" {
			return config.NotSet
		}
		return v
	case []any:
		if len(v) == 0 {
			return config.None
		}
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = entryText(item)
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return render.Count(len(v), "value")
	}
	return entryText(value)
}

// Whether a table's entries are single values typed as text, such as headers or variables: one declared field with no key of its own
func single(setting *configv1.Setting) bool {
	fields := setting.GetEntry().GetFields()
	if len(fields) != 1 || fields[0].GetKey() != "" {
		return false
	}
	switch fields[0].GetKind() {
	case configv1.Kind_KIND_TEXT, configv1.Kind_KIND_NUMBER, configv1.Kind_KIND_DURATION:
		return true
	}
	return fields[0].GetSecret()
}

// Entries of a table whose entries are single values, such as headers or variables: a row per name and its value, enter changing the value,
// alt+n asking the name and then the value as one job, ctrl+d removing one after a question
func (s *settings) pairs(setting *configv1.Setting) *cards.List {
	t := s.kit.Env.Theme
	schema := setting.GetEntry()
	field := schema.GetFields()[0]
	what := noun(setting)
	held := func() map[string]any {
		entries, _ := decode(s.current(setting).GetValue()).(map[string]any)
		return entries
	}
	said := func(name string) string { return s.entryName(setting, schema, name, true) }
	var names []string
	var view *cards.List
	put := func(name string) func(value any, from wm.Card, told string) tea.Cmd {
		return func(value any, from wm.Card, told string) tea.Cmd {
			return s.write(setting, "", name, value, from, told)
		}
	}
	add := func() tea.Cmd {
		flow := s.kit.Flow(s.hub, "Add "+article(what)+" "+what)
		value := func(name string) tea.Cmd {
			asked := entryField(field, "value", nil)
			// The value of a named entry is asked under that name, never as a value alone
			asked.Label, asked.Required = said(name), true
			return flow.Ask("The value of "+field.GetTitle()+" for "+said(name)+".", []cards.Field{asked}, "Add", func(values map[string]string) tea.Cmd {
				kept := fieldValue(field, values["value"], nil)
				// The new entry in hand once it is kept
				return tea.Batch(flow.End(), s.keepEntry(setting, name, kept, "Added "+said(name)+".", func() { view.Select(slices.Index(names, name)) }))
			})
		}
		// A table whose entries are named after things picks the name from those things
		if cmd, ok := s.pickName(setting, "Add "+article(what)+" "+what, "", value); ok {
			return cmd
		}
		if known := schema.GetNames(); len(known) > 0 {
			var choices []tui.Choice
			for _, option := range known {
				if _, taken := held()[option.GetValue()]; !taken {
					choices = append(choices, tui.Choice{ID: option.GetValue(), Title: cmp.Or(option.GetTitle(), option.GetValue()), Detail: option.GetDescription()})
				}
			}
			if len(choices) == 0 {
				return s.hub.Say("Every "+what+" is in the list already.", cards.NoteInfo)
			}
			return flow.Pick("Which "+what+" to add.", choices, "", func(choice tui.Choice) tea.Cmd { return value(choice.ID) })
		}
		name := nameField(schema, what, func(typed string) string {
			if have, taken := holding(schema, held(), strings.TrimSpace(typed)); taken {
				return "There is " + article(what) + " " + what + " named " + said(have) + " already."
			}
			return ""
		})
		return flow.Ask("", []cards.Field{name}, "", func(values map[string]string) tea.Cmd {
			return value(strings.TrimSpace(values["name"]))
		})
	}
	view = cards.NewList(s.kit.Env, cards.ListSpec{
		Title:   setting.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     "setting:" + setting.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search in " + setting.GetTitle(),
		Intro: func() []string {
			return s.opening(setting.GetDescription(), schema.GetNoun(), setting.GetTitle(), len(held()) == 0)
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows: func() [][]string {
			entries := held()
			names = slices.Sorted(maps.Keys(entries))
			rows := make([][]string, len(names), len(names)+1)
			for i, name := range names {
				rows[i] = []string{said(name), render.Dim(fieldWords(field, entries[name]), t)}
			}
			return append(rows, []string{"Add " + article(what) + " " + what, render.Dim(t.Glyphs.Forward, t)})
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add"}}
			case i >= len(names):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(names) {
					return add()
				}
				name := names[i]
				return s.hub.Push(s.entry(schema, said(name), held()[name], false, put(name)))
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return add() },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(names) {
					return nil
				}
				name := names[i]
				l.Confirm(cards.Question{Text: "Remove " + said(name) + "?", Verb: "Remove", Deletes: true},
					func() tea.Cmd { return s.write(setting, "", name, nil, nil, "Removed "+said(name)+".") })
				return nil
			},
		},
	})
	s.views.Add(view)
	return view
}

// Keeps one entry of a table under its own key, saying what was made, then runs then
func (s *settings) keepEntry(setting *configv1.Setting, name string, value any, told string, then func()) tea.Cmd {
	if strings.Contains(name, ".") {
		return s.write(setting, "", name, value, nil, told)
	}
	encoded, err := encode(value)
	if err != nil {
		return s.fail(err.Error())
	}
	return s.keep(setting, map[string]string{setting.GetKey() + "." + name: encoded}, nil, false, told, then)
}

// Name of an entry of a table as a person reads it: what the table calls it when it knows its names, else at the top of the table the
// title of the thing it is named after, else the name as it is written
func (s *settings) entryName(setting *configv1.Setting, schema *configv1.Entry, name string, top bool) string {
	for _, known := range schema.GetNames() {
		if known.GetValue() == name && known.GetTitle() != "" {
			return known.GetTitle()
		}
	}
	if top {
		for _, known := range setting.GetNames() {
			if known.GetValue() == name && known.GetTitle() != "" {
				return known.GetTitle()
			}
		}
		if picker, ok := s.kit.Picker(setting.GetPicks()); ok && picker.Shown != nil {
			key := setting.GetKey()
			if said := picker.Shown(tui.Ask{Key: key, Values: map[string]string{key: strconv.Quote(name)}, Profile: s.named}); said != "" {
				return said
			}
		}
	}
	return name
}

// Opens the picker of the kind of thing a table's entries are named after, the name picked handed to done; false for a table whose names
// are free, which asks for one in a field
func (s *settings) pickName(setting *configv1.Setting, title, verb string, done func(name string) tea.Cmd) (tea.Cmd, bool) {
	picker, ok := s.kit.Picker(setting.GetPicks())
	if !ok {
		return nil, false
	}
	key := setting.GetKey()
	ask := tui.Ask{Title: title, Intro: []string{setting.GetDescription()}, Key: key, Values: map[string]string{key: setting.GetValue()},
		Profile: s.named, Verb: verb, Done: func(values map[string]string) tea.Cmd {
			if name := strings.TrimSpace(text(values[key])); name != "" {
				return done(name)
			}
			return nil
		}}
	return picker.Open(s.kit, s.hub, ask), true
}
