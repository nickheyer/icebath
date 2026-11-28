package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Help under the name a new program is asked for
const nameHelp = "A name for the program: lower case letters, digits, - and _, such as my-whisper."

// What is wrong with a name typed for a program before it is sent, empty when it may be one
func nameCheck(name string) string {
	if !config.Named(strings.ToLower(strings.TrimSpace(name))) {
		return "Use only lower case letters, digits, - and _."
	}
	return ""
}

// Help under the command of a program of the part: the field's own, as its settings declare it
func (p *page) commandHelp(k kind) string {
	for _, field := range p.options.Setting(k.table).GetEntry().GetFields() {
		if field.GetKey() == "command" {
			return field.GetHelp()
		}
	}
	return ""
}

// Adds a program of yours as a job of two steps, its name and its command, written only once the second is sent and the server took both;
// the new program is then the one in use, in hand on the list
func (p *page) addProgram(k kind, list *cards.List) tea.Cmd {
	f := p.kit.Flow(p.hub, "Add a program")
	return f.Ask("", []cards.Field{{ID: "name", Label: "Program name", Help: nameHelp, Check: nameCheck}}, "Next", func(named map[string]string) tea.Cmd {
		name := strings.ToLower(strings.TrimSpace(named["name"]))
		field := cards.Field{ID: "command", Label: "Command", Help: p.commandHelp(k)}
		return f.Ask("", []cards.Field{field}, "Add", func(values map[string]string) tea.Cmd {
			return tui.Call(p.hub, func() (*speechv1.ProgramReply, error) {
				ctx := p.kit.Env.Ctx
				reply, err := p.service.AddProgram(ctx, &speechv1.AddProgramRequest{Profile: p.profile, Kind: k.kind, Name: name,
					Command: strings.TrimSpace(values["command"])})
				if err == nil {
					// The name as YAML text, so one such as on or 123 is kept as a name and not read as a switch or a number
					var chosen string
					if chosen, err = encode(reply.GetName()); err == nil {
						_, err = p.settings.Set(ctx, &configv1.SetRequest{Key: k.choice, Value: chosen, Profile: p.profile})
					}
				}
				return reply, err
			}, func(reply *speechv1.ProgramReply) tea.Cmd {
				return tea.Batch(f.End(), p.hub.Done("Added the program "+reply.GetName()+". It now "+k.does+"."),
					p.load(func() tea.Cmd { return p.inHand(list, reply.GetName()) }), p.follow(map[string]any{k.choice: reply.GetName()}))
			})
		})
	})
}

// Puts the cursor on the row of the service with the name
func (p *page) inHand(list *cards.List, name string) tea.Cmd {
	list.Refresh()
	for _, k := range []kind{recognizers, voices} {
		if list.Key() != "voice:"+k.part {
			continue
		}
		for i, row := range p.partRows(k) {
			if row.service.GetName() == name {
				list.Select(i)
			}
		}
	}
	return nil
}

// Asks before a program of yours is removed, then removes it, the choice of it going back to its default
func (p *page) removeProgram(k kind, l *cards.List, name string) tea.Cmd {
	question := cards.Question{Text: "Remove the program " + name + "? Its settings go with it.", Verb: "Remove", Deletes: true}
	l.Confirm(question, func() tea.Cmd {
		return tui.Call(p.hub, func() (*speechv1.ProgramReply, error) {
			return p.service.RemoveProgram(p.kit.Env.Ctx, &speechv1.RemoveProgramRequest{Profile: p.profile, Kind: k.kind, Name: name})
		}, func(reply *speechv1.ProgramReply) tea.Cmd {
			return tea.Batch(p.back(k), p.follow(map[string]any{k.choice: nil}), p.load(func() tea.Cmd {
				note := "Removed the program " + name + "."
				switch {
				case reply.GetChosen() && k.auto:
					note += " " + k.title + " uses the first service that is ready again."
				case reply.GetChosen():
					note += " " + p.voice.GetSpeaking().GetTitle() + " reads replies aloud again."
				}
				return p.hub.Done(note)
			}))
		})
	})
	return nil
}

// Goes back to the page of the part from a page over it
func (p *page) back(k kind) tea.Cmd {
	for _, view := range p.views.Lists() {
		if view.Key() == "voice:"+k.part && p.hub.Holds(view) {
			return p.hub.PopTo(view)
		}
	}
	return nil
}

// Row of the page of a program: using it, a field of its entry, a setting of every program, renaming or removing it
type programRow struct {
	use     bool
	field   *configv1.Setting
	setting *configv1.Setting
	rename  bool
	remove  bool
}

// Rows of the page of a program as it stands now
func (p *page) programRows(k kind, name string) []programRow {
	s := p.find(k, name)
	if s == nil {
		return nil
	}
	var rows []programRow
	if name != p.chosen(k) {
		rows = append(rows, programRow{use: true})
	}
	for _, field := range s.GetFields() {
		rows = append(rows, programRow{field: field})
	}
	if k.part == voices.part {
		for _, setting := range p.settingsOf(everyProgram...) {
			rows = append(rows, programRow{setting: setting})
		}
	}
	return append(rows, programRow{rename: true}, programRow{remove: true})
}

// Opens the page of a program of yours: using it, each field of its entry with the control of its kind, the settings of every program for
// one that reads text aloud, then Rename and Remove
func (p *page) programPage(k kind, name string) tea.Cmd {
	t := p.kit.Env.Theme
	var rows []programRow
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   name,
		Glyph:   t.Glyphs.Session,
		Key:     "voice:" + k.part + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.serviceState(k, name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			rows = p.programRows(k, name)
			cells := make([][]string, len(rows))
			for i, row := range rows {
				cells[i] = p.programCells(row)
			}
			return cells
		},
		Group: func(i int) string {
			if rows[i].setting != nil {
				return "Every program"
			}
			return ""
		},
		Detail: func(i int) []string {
			switch row := rows[i]; {
			case row.use:
				return []string{"Have " + name + " do the work of " + strings.ToLower(k.title) + " from now on."}
			case row.field != nil:
				return fieldDetail(row.field)
			case row.setting != nil:
				return p.options.Detail(row.setting, view.Cut(i, 2))
			case row.rename:
				return []string{"Give the program another name."}
			}
			return []string{"Take the program out of icebath."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "The program "+name+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(rows) {
				return nil
			}
			switch row := rows[i]; {
			case row.use:
				return []wm.Hint{{Action: "hub.use", Label: "use"}}
			case row.field != nil && row.field.GetKind() == configv1.Kind_KIND_TOGGLE:
				return []wm.Hint{{Action: "hub.use", Label: map[bool]string{true: "turn off", false: "turn on"}[row.field.GetValue() == "true"]}}
			case row.setting != nil:
				return p.options.Hints(row.setting)
			case row.rename:
				return []wm.Hint{{Action: "hub.use", Label: "rename"}}
			case row.remove:
				return []wm.Hint{{Action: "hub.use", Label: "remove"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				switch row := rows[i]; {
				case row.use:
					return p.write(name+" now "+k.does+".", map[string]any{k.choice: name})
				case row.field != nil:
					return p.changeField(k, name, row.field)
				case row.setting != nil:
					return p.options.Change(row.setting)
				case row.rename:
					return p.rename(k, name)
				}
				return p.removeProgram(k, l, name)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				switch row := rows[i]; {
				case row.setting != nil:
					return p.options.Reset(l, row.setting)
				case row.field != nil && row.field.GetValue() != "" && !row.field.GetNeed():
					return p.saveField(k, name, row.field, "null")
				}
				return p.removeProgram(k, l, name)
			},
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

func (p *page) programCells(row programRow) []string {
	t := p.kit.Env.Theme
	switch {
	case row.use:
		return []string{"Use it", "", ""}
	case row.field != nil:
		mark := ""
		if row.field.GetValue() != "" {
			mark = render.Cool("●", t)
		}
		return []string{row.field.GetTitle(), mark, row.field.GetShown()}
	case row.setting != nil:
		return p.options.Cells(row.setting)
	case row.rename:
		return []string{"Rename", "", ""}
	}
	return []string{"Remove", "", ""}
}

// Lines under the rows about a field of a program: what it holds, its help, its default and bounds
func fieldDetail(field *configv1.Setting) []string {
	lines := []string{field.GetDescription()}
	if help := field.GetHelp(); help != "" {
		lines = append(lines, help)
	}
	var facts []string
	if zero := field.GetZero(); zero != "" {
		facts = append(facts, "Empty: "+zero)
	}
	if bounds := field.GetBounds(); bounds != "" {
		facts = append(facts, bounds)
	}
	if len(facts) > 0 {
		lines = append(lines, strings.Join(facts, " · "))
	}
	return lines
}

// Changes a field of a program with the control of its kind: a switch flips, a choice is picked, a list of names is a list, the rest is typed
func (p *page) changeField(k kind, name string, field *configv1.Setting) tea.Cmd {
	switch field.GetKind() {
	case configv1.Kind_KIND_TOGGLE:
		return p.saveField(k, name, field, map[bool]string{true: "false", false: "true"}[field.GetValue() == "true"])
	case configv1.Kind_KIND_CHOICE:
		var choices []tui.Choice
		for _, option := range field.GetOptions() {
			choices = append(choices, tui.Choice{ID: option.GetValue(), Title: option.GetTitle(), Detail: option.GetDescription()})
		}
		current := plain(field.GetValue())
		return p.kit.PickFrom(p.hub, field.GetTitle(), []string{field.GetDescription()}, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
			value, _ := encode(choice.ID)
			if choice.ID == "" {
				value = "null"
			}
			return p.saveField(k, name, field, value)
		})
	case configv1.Kind_KIND_LIST:
		return p.namesList(k, name, field)
	}
	return p.typeField(k, name, field)
}

// Form that types a field of a program, empty putting its default back unless the program can't do without it; a refusal of the server
// shown under the field
func (p *page) typeField(k kind, name string, field *configv1.Setting) tea.Cmd {
	help := field.GetHelp()
	if zero := field.GetZero(); zero != "" && !field.GetNeed() {
		help = strings.TrimSpace(help + " Leave it empty for: " + zero + ".")
	}
	if field.GetKind() == configv1.Kind_KIND_DURATION {
		help = strings.TrimSpace(help + " A length such as 30s, 5m or 2h.")
	}
	kind := cards.FieldText
	if field.GetKind() == configv1.Kind_KIND_NUMBER {
		kind = cards.FieldNumber
	}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  field.GetTitle(),
		Fields: []cards.Field{{ID: "value", Label: field.GetTitle(), Help: help, Kind: kind, Value: plain(field.GetValue()), Select: true, Required: field.GetNeed()}},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			text := strings.TrimSpace(values["value"])
			value := "null"
			if text != "" {
				value, _ = encode(typedValue(field, text))
			}
			return p.saveTyped(k, name, field, value, form)
		},
	})
	return p.hub.Push(form)
}

// Value a typed field holds: a number as a number, any other text as it is
func typedValue(field *configv1.Setting, text string) any {
	if field.GetKind() == configv1.Kind_KIND_NUMBER {
		var number any
		if decoded, err := decodeYAML(text); err == nil {
			number = decoded
		}
		if number != nil {
			return number
		}
	}
	return text
}

// Saves a value of a field of a program through the server's checks, the form closing once it is kept and showing a refusal under its field
func (p *page) saveTyped(k kind, name string, field *configv1.Setting, value string, form *cards.Form) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		_, err := p.service.ChangeProgram(p.kit.Env.Ctx, &speechv1.ChangeProgramRequest{Profile: p.profile, Kind: k.kind, Name: name,
			Field: field.GetKey(), Value: value})
		return func() tea.Cmd {
			if err != nil {
				form.SetError("value", tui.Reason(err))
				return nil
			}
			return tea.Batch(p.hub.Close(form), p.load(func() tea.Cmd { return p.fieldNote(k, name, field) }))
		}
	})
}

// Saves a value of a field of a program through the server's checks
func (p *page) saveField(k kind, name string, field *configv1.Setting, value string) tea.Cmd {
	return tui.Call(p.hub, func() (*speechv1.ProgramReply, error) {
		return p.service.ChangeProgram(p.kit.Env.Ctx, &speechv1.ChangeProgramRequest{Profile: p.profile, Kind: k.kind, Name: name,
			Field: field.GetKey(), Value: value})
	}, func(*speechv1.ProgramReply) tea.Cmd {
		return p.load(func() tea.Cmd { return p.fieldNote(k, name, field) })
	})
}

// Note of a field of a program once it is saved: its new value in words
func (p *page) fieldNote(k kind, name string, field *configv1.Setting) tea.Cmd {
	for _, now := range p.find(k, name).GetFields() {
		if now.GetKey() == field.GetKey() {
			return p.hub.Done(field.GetTitle() + " of " + name + " is now " + now.GetShown() + ".")
		}
	}
	return nil
}

// The names a list field of a program holds, each changed, removed or added by typing it under the name of what one is
func (p *page) namesList(k kind, name string, field *configv1.Setting) tea.Cmd {
	t := p.kit.Env.Theme
	noun := cmp.Or(field.GetEntry().GetNoun(), "item")
	label := strs.Capital(noun)
	held := func() []string {
		s := p.find(k, name)
		for _, f := range s.GetFields() {
			if f.GetKey() == field.GetKey() {
				items, _ := decodeYAML(f.GetValue())
				var out []string
				for _, item := range asList(items) {
					out = append(out, item)
				}
				return out
			}
		}
		return nil
	}
	store := func(items []string, note string) tea.Cmd {
		value := "null"
		if len(items) > 0 {
			value, _ = encode(items)
		}
		return tui.Call(p.hub, func() (*speechv1.ProgramReply, error) {
			return p.service.ChangeProgram(p.kit.Env.Ctx, &speechv1.ChangeProgramRequest{Profile: p.profile, Kind: k.kind, Name: name,
				Field: field.GetKey(), Value: value})
		}, func(*speechv1.ProgramReply) tea.Cmd { return tea.Batch(p.hub.Done(note), p.load(nil)) })
	}
	ask := func(title, was, verb string, done func(item string) tea.Cmd) tea.Cmd {
		help := "The name of a variable of your environment, such as HF_TOKEN."
		return p.kit.Ask(p.hub, title, cards.Field{Label: label, Value: was, Help: help, Select: was != ""}, verb, func(item string) tea.Cmd {
			return done(strings.ToUpper(item))
		})
	}
	var items []string
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   field.GetTitle(),
		Glyph:   t.Glyphs.Settings,
		Key:     "voice:" + k.part + ":" + name + ":" + field.GetKey(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{field.GetDescription()} },
		Columns: []render.Column{{Flex: 1}},
		Rows: func() [][]string {
			items = held()
			cells := make([][]string, len(items))
			for i, item := range items {
				cells[i] = []string{item}
			}
			return cells
		},
		Empty: func() string { return "No " + noun + "s yet. " + p.kit.Env.Keys.Display("hub.add") + " adds one." },
		Free:  map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add", Label: "add"}, {Action: "hub.remove", Label: "remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				was := items[i]
				return ask("Change a "+noun, was, "Save", func(item string) tea.Cmd {
					next := slices.Clone(items)
					next[i] = item
					return store(next, label+" "+was+" is now "+item+".")
				})
			},
			"hub.add": func(*cards.List, int) tea.Cmd {
				return ask("Add a "+noun, "", "Add", func(item string) tea.Cmd {
					if slices.Contains(items, item) {
						return p.hub.Done(item + " is on the list already.")
					}
					return store(append(slices.Clone(items), item), "Added "+item+".")
				})
			},
			"hub.remove": func(_ *cards.List, i int) tea.Cmd {
				gone := items[i]
				return store(slices.Delete(slices.Clone(items), i, i+1), "Removed "+gone+".")
			},
		},
	})
	p.views.Add(list)
	return p.hub.Push(list)
}

// Gives a program another name, the choice of it following, then shows its page again under the new name
func (p *page) rename(k kind, name string) tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Rename",
		Fields: []cards.Field{{ID: "value", Label: "Program name", Help: nameHelp, Value: name, Select: true, Required: true, Check: nameCheck}},
		Submit: "Rename",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			to := strings.ToLower(strings.TrimSpace(values["value"]))
			return tui.Async(func() func() tea.Cmd {
				reply, err := p.service.ChangeProgram(p.kit.Env.Ctx, &speechv1.ChangeProgramRequest{Profile: p.profile, Kind: k.kind, Name: name, Rename: to})
				return func() tea.Cmd {
					if err != nil {
						form.SetError("value", tui.Reason(err))
						return nil
					}
					follow := tea.Cmd(nil)
					if reply.GetChosen() {
						follow = p.follow(map[string]any{k.choice: reply.GetName()})
					}
					return tea.Batch(p.back(k), p.hub.Done("The program "+name+" is now called "+reply.GetName()+"."), follow,
						p.load(func() tea.Cmd { return p.programPage(k, reply.GetName()) }))
				}
			})
		},
	})
	return p.hub.Push(form)
}

// Text of a value written in YAML as a field shows it to be typed again
func plain(value string) string {
	decoded, err := decodeYAML(value)
	if err != nil || decoded == nil {
		return ""
	}
	switch v := decoded.(type) {
	case string:
		return v
	}
	return strings.TrimSpace(value)
}
