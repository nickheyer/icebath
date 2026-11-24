package tui

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the settings screens
const (
	settingsGroupsContext = "tui.settings.groups"
	settingsRowsContext   = "tui.settings.rows"
)

// Keys of the settings screens beyond the shared hub keys
func registerSettingsKeys(km *keys.Map) {
	for name, doc := range map[string]string{settingsGroupsContext: "settings groups", settingsRowsContext: "settings of a group"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	km.Register(keys.Action{ID: "tui.settings.advanced", Context: settingsRowsContext, Label: "advanced", Doc: "show or hide the advanced settings", Default: []string{"tab"}})
	km.Register(keys.Action{ID: "tui.settings.search", Context: settingsGroupsContext, Label: "search", Doc: "search every setting by its words", Default: []string{"/"}})
	km.Register(keys.Action{ID: "tui.settings.files", Context: settingsGroupsContext, Label: "files", Doc: "show where settings and keys are kept and open them in your editor", Default: []string{"f"}})
	km.Register(keys.Action{ID: "tui.settings.suggest", Context: settingsRowsContext, Label: "suggest", Doc: "propose allowed commands from the risky commands you approved before", Default: []string{"s"}})
	km.Register(keys.Action{ID: "tui.settings.check", Context: settingsRowsContext, Label: "check a command", Doc: "show what the approval rules do with a command, without running it", Default: []string{"c"}})
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "settings", order: 30, title: txt.HubsTitleSettings, short: txt.HubsShortSettings, slash: "settings", usage: "[group or words to search]", open: func(a *app, arg string) tea.Cmd { return a.openSettings(arg) }})
}

// Settings screen: the groups, typing searching every setting; a group named opens it and other words open the search
func (a *app) openSettings(arg string) tea.Cmd {
	sid := a.store.ui.Sid
	var (
		groups []tuigateway.SettingsGroup
		h      *cards.Hub
		list   *cards.List
	)
	label := func(id string) string {
		for _, g := range groups {
			if g.ID == id {
				return g.Label
			}
		}
		return id
	}
	open := func(_ *cards.List, i int) tea.Cmd {
		if i < 0 || i >= len(groups) {
			return nil
		}
		return a.settingsRows(h, groups[i].ID, groups[i].Label, "", sid, label)
	}
	search := func(text string) tea.Cmd {
		return a.settingsRows(h, "", tr(txt.HubsSettingsSearchTitle), text, sid, label)
	}
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSettings), Glyph: a.toolkit.Glyphs.Settings, Key: "tui:settings", Context: settingsGroupsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColGroup)}, {Title: tr(txt.HubsColWhatItCovers), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(groups))
			for i, g := range groups {
				out[i] = []string{g.Label, g.Short}
			}
			return out
		},
		Empty: func() string { return tr(txt.HubsLoading) },
		Help:  func(int) []string { return []string{tr(txt.HubsSettingsHelp)} },
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelOpen)}, {Action: "tui.settings.search"}, {Action: "tui.settings.files"}}
		},
		Quick: open,
		Free:  map[string]bool{"tui.settings.search": true, "tui.settings.files": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":             open,
			"tui.settings.search": func(*cards.List, int) tea.Cmd { return search("") },
			"tui.settings.files":  func(*cards.List, int) tea.Cmd { return a.settingsFiles(h, sid) },
		},
	})
	h = a.newHub("tui:settings", list)
	return tea.Batch(a.wm.ShowOverlay(h), hubCall(a, h, "", tuigateway.SettingsGroups, tuigateway.SettingsGroupsParams{SessionID: optionalText(sid)}, func(r *tuigateway.SettingsGroupsResult) tea.Cmd {
		groups = r.Groups
		list.Refresh()
		arg = strings.TrimSpace(arg)
		if arg == "" {
			return nil
		}
		for _, g := range groups {
			if strings.EqualFold(g.ID, arg) || strings.EqualFold(g.Label, arg) {
				return a.settingsRows(h, g.ID, g.Label, "", sid, label)
			}
		}
		return search(arg)
	}))
}

// Files the settings and keys are kept in: where they are and whether they exist, each opened in the editor, the settings file checked
// after an edit and the keys read again by the background service, and their paths copied
func (a *app) settingsFiles(h *cards.Hub, sid string) tea.Cmd {
	var files tuigateway.ConfigFilesResult
	read := func(seed bool, then func() tea.Cmd) tea.Cmd {
		return hubCall(a, h, tr(txt.HubsLoading), tuigateway.ConfigFiles, tuigateway.ConfigFilesParams{Seed: seed}, func(r *tuigateway.ConfigFilesResult) tea.Cmd {
			files = *r
			return then()
		})
	}
	where := func(path string, exists bool) string {
		if exists {
			return path
		}
		return path + " (" + tr(txt.HubsSettingsFilesMissing) + ")"
	}
	detail := func() []string {
		out := []string{tr(txt.HubsSettingsFilesAbout), "", tr(txt.HubsSettingsFilesConfig, where(files.Config, files.ConfigExists)),
			tr(txt.HubsSettingsFilesEnv, where(files.Env, files.EnvExists))}
		switch files.Locked {
		case "managed":
			out = append(out, "", tr(txt.HubsSettingsFilesManaged, files.Manager))
		case "no_file":
			out = append(out, "", tr(txt.HubsSettingsFilesNoFile))
		}
		return out
	}
	check := func() tea.Cmd {
		return a.hubRunCLI(h, tr(txt.HubsSettingsFilesCheck), []string{"config", "check"}, func(*cliRun) tea.Cmd { return nil })
	}
	// The editor on one file, then the settings file checked or the keys read again into the background service
	edit := func(path string, after func() tea.Cmd) tea.Cmd {
		return editFile(path, func(err error) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd {
				if err != nil {
					return h.Fail(tr(txt.SessionInputFailedToOpenEditorWith, err.Error()))
				}
				return after()
			}}
		})
	}
	reloadKeys := func() tea.Cmd {
		return hubCall(a, h, "", tuigateway.ReloadEnv, tuigateway.ReloadEnvParams{SessionID: sid}, func(r *tuigateway.ReloadEnvResult) tea.Cmd {
			key := txt.SlashCmdOpsReloadEnvReloadedOther
			if r.Updated == 1 {
				key = txt.SlashCmdOpsReloadEnvReloadedOne
			}
			return h.Done(tr(key, r.Updated))
		})
	}
	actions := func() []hubAction {
		var out []hubAction
		if files.Locked == "" {
			out = append(out, hubAction{label: tr(txt.HubsSettingsFilesEdit), does: tr(txt.HubsSettingsFilesEditDoes), run: func() tea.Cmd {
				return read(true, func() tea.Cmd {
					if files.Seeded {
						return tea.Batch(h.Done(tr(txt.HubsSettingsFilesSeeded, files.Config)), edit(files.Config, check))
					}
					return edit(files.Config, check)
				})
			}})
			if files.EnvExists {
				out = append(out, hubAction{label: tr(txt.HubsSettingsFilesEditEnv), does: tr(txt.HubsSettingsFilesEditEnvDoes), run: func() tea.Cmd {
					return edit(files.Env, reloadKeys)
				}})
			}
		}
		if files.ConfigExists {
			out = append(out, hubAction{label: tr(txt.HubsSettingsFilesCheck), does: tr(txt.HubsSettingsFilesCheckDoes), run: check})
		}
		return append(out,
			hubAction{label: tr(txt.HubsSettingsFilesCopy), does: tr(txt.HubsSettingsFilesCopyDoes), run: func() tea.Cmd {
				return a.hubCopy(h, files.Config, tr(txt.HubsSettingsFilesPathWord))
			}},
			hubAction{label: tr(txt.HubsSettingsFilesCopyEnv), does: tr(txt.HubsSettingsFilesCopyEnvDoes), run: func() tea.Cmd {
				return a.hubCopy(h, files.Env, tr(txt.HubsSettingsFilesPathWord))
			}})
	}
	return read(false, func() tea.Cmd { return a.hubActions(h, tr(txt.HubsSettingsFilesTitle), detail, actions) })
}

// Settings of one group, the everyday ones first and the advanced ones on tab; with no group every setting, narrowed by the words typed
func (a *app) settingsRows(h *cards.Hub, group, label, query, sid string, groupLabel func(id string) string) tea.Cmd {
	searching := group == ""
	advanced := searching
	loaded := false
	var rows []tuigateway.SettingsRow
	var list *cards.List
	params := func() tuigateway.SettingsListParams {
		p := tuigateway.SettingsListParams{SessionID: optionalText(sid), Advanced: &advanced}
		if !searching {
			p.Group = &group
		}
		return p
	}
	reload := func() tea.Cmd {
		return hubCall(a, h, "", tuigateway.SettingsList, params(), func(r *tuigateway.SettingsListResult) tea.Cmd {
			rows, loaded = r.Rows, true
			list.Refresh()
			return nil
		})
	}
	at := func(i int) (tuigateway.SettingsRow, bool) {
		if i < 0 || i >= len(rows) {
			return tuigateway.SettingsRow{}, false
		}
		return rows[i], true
	}
	edit := func(_ *cards.List, i int) tea.Cmd {
		row, ok := at(i)
		switch {
		case !ok:
			return nil
		case row.Locked:
			return h.Fail(tr(txt.HubsSettingsLocked, row.Label))
		case row.Kind == "map":
			return a.settingsMap(h, row, sid, reload)
		}
		return a.editSetting(h, row, sid, reload)
	}
	columns := []render.Column{{Title: tr(txt.HubsColSetting), Max: 28}, {Title: tr(txt.HubsColValue), Max: 28}, {Title: ""}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}}
	if searching {
		columns = append(columns, render.Column{Title: tr(txt.HubsColGroup)})
	}
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: label, Glyph: a.toolkit.Glyphs.Settings, Key: "tui:settings:" + group, Context: settingsRowsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: columns,
		Yield:   []int{4, 2},
		Rows: func() [][]string {
			out := make([][]string, len(rows))
			for i, r := range rows {
				out[i] = []string{r.Label, settingValueText(r), settingSourceMark(r), settingWords(r), groupLabel(r.Group)}
			}
			return out
		},
		Detail: func(i int) []string {
			row, ok := at(i)
			if !ok {
				return nil
			}
			return settingDetail(row)
		},
		Help: func(i int) []string {
			row, ok := at(i)
			if !ok {
				return nil
			}
			return append(settingDetail(row), settingPlaces(row)...)
		},
		Status: func() string {
			if advanced && !searching {
				return tr(txt.HubsSettingsAdvancedShown)
			}
			return tr(txt.HubsSettingsLegend)
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.HubsLoading)
			}
			if searching {
				return tr(txt.HubsSettingsNoMatch)
			}
			return tr(txt.HubsSettingsEmpty, a.keys.Display("tui.settings.advanced"))
		},
		// The highlighted setting's full text, default and source show under the table
		OnFocus: func(l *cards.List) tea.Cmd {
			if l.DetailShown() {
				return nil
			}
			return l.ToggleDetail()
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelChange)}}
			if row, ok := at(i); ok && row.Source != "default" && !row.Locked {
				out = append(out, wm.Hint{Action: "hub.remove", Label: tr(txt.HintsLabelReset)})
			}
			if !searching {
				out = append(out, wm.Hint{Action: "tui.settings.advanced"})
			}
			if group == safetyGroup {
				out = append(out, wm.Hint{Action: "tui.settings.suggest"}, wm.Hint{Action: "tui.settings.check"})
			}
			return out
		},
		Free: map[string]bool{"tui.settings.advanced": true, "hub.refresh": true, "tui.settings.suggest": group == safetyGroup, "tui.settings.check": group == safetyGroup},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":     edit,
			"hub.edit":    edit,
			"hub.refresh": func(*cards.List, int) tea.Cmd { return reload() },
			"tui.settings.suggest": func(*cards.List, int) tea.Cmd {
				if group != safetyGroup {
					return nil
				}
				return a.approvalProposals(h, reload)
			},
			"tui.settings.check": func(*cards.List, int) tea.Cmd {
				if group != safetyGroup {
					return nil
				}
				return a.approvalCheck(h)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				row, ok := at(i)
				if !ok || row.Locked || row.Source == "default" {
					return nil
				}
				scope := "global"
				if row.Source == "session" {
					scope = "session"
				}
				l.Confirm(tr(txt.HubsSettingsResetConfirm, row.Label, settingValueText(tuigateway.SettingsRow{Kind: row.Kind, Value: row.Default, Choices: row.Choices, Unit: row.Unit, Null: row.Null})), func() tea.Cmd {
					p := tuigateway.SettingsResetParams{SessionID: optionalText(sid), Key: row.Key, Scope: &scope}
					return hubCall(a, h, "", tuigateway.SettingsReset, p, func(r *tuigateway.SettingsWriteResult) tea.Cmd {
						return tea.Batch(h.Done(settingAppliedText(row, r)), reload())
					})
				})
				return nil
			},
			"tui.settings.advanced": func(*cards.List, int) tea.Cmd {
				if searching {
					return nil
				}
				advanced = !advanced
				return reload()
			},
		},
	})
	cmd := h.Push(list)
	if searching {
		list.Filter(query)
	}
	return tea.Batch(cmd, reload())
}

// Words of a setting a search reads besides its label and value: what it does and its shortcut
func settingWords(r tuigateway.SettingsRow) string {
	if r.Slash != "" {
		return r.Short + " (/" + r.Slash + ")"
	}
	return r.Short
}

// Changes one setting in a form on the hub, its value by kind and, for a setting a session may hold, whether the change is for this session
// or for every session; a switch with one scope flips at once
func (a *app) editSetting(h *cards.Hub, row tuigateway.SettingsRow, sid string, done func() tea.Cmd) tea.Cmd {
	both := sid != "" && slices.Contains(row.Scopes, "session") && slices.Contains(row.Scopes, "global")
	write := func(value any, scope string) tea.Cmd {
		p := tuigateway.SettingsSetParams{SessionID: optionalText(sid), Key: row.Key, Value: value}
		if scope != "" {
			p.Scope = &scope
		}
		return hubCall(a, h, "", tuigateway.SettingsSet, p, func(r *tuigateway.SettingsWriteResult) tea.Cmd {
			return tea.Batch(h.Done(settingAppliedText(row, r)), done())
		})
	}
	if row.Kind == "bool" && !both {
		on, _ := row.Value.(bool)
		return write(!on, "")
	}
	field, ok := settingField(row, "value", row.Label)
	if !ok {
		return h.Fail(tr(txt.HubsSettingsNotEditable, row.Label))
	}
	if row.Kind == "bool" {
		on, _ := row.Value.(bool)
		field.Value = strconv.FormatBool(!on)
	}
	fields := []cards.Field{field}
	if both {
		scope := "global"
		if row.Source == "session" {
			scope = "session"
		}
		fields = append(fields, cards.Field{ID: "scope", Label: tr(txt.HubsSettingsScopeLabel), Kind: cards.FieldChoice, Value: scope, Choices: []cards.Choice{
			{Value: "session", Label: tr(txt.HubsSettingsScopeSession), Meaning: tr(txt.HubsSettingsScopeSessionMeaning)},
			{Value: "global", Label: tr(txt.HubsSettingsScopeAlways), Meaning: tr(txt.HubsSettingsScopeAlwaysMeaning)}}})
	}
	spec := cards.FormSpec{Title: row.Label, Intro: []string{row.Short}, Fields: fields, Submit: tr(txt.HubsSettingsSave)}
	return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
		value, err := settingValue(row.Kind, v["value"])
		if err != nil {
			return h.Fail(err.Error())
		}
		return write(value, v["scope"])
	})
}

// Form field of a setting's value by its kind, false for a kind that is not typed or picked
func settingField(row tuigateway.SettingsRow, id, label string) (cards.Field, bool) {
	f := cards.Field{ID: id, Label: label, Help: row.Help, Value: settingEditText(row)}
	switch row.Kind {
	case "bool":
		f.Kind = cards.FieldBool
		on, _ := row.Value.(bool)
		f.Value = strconv.FormatBool(on)
	case "enum":
		if len(row.Choices) > 0 && !row.Open {
			f.Kind = cards.FieldChoice
			for _, c := range row.Choices {
				f.Choices = append(f.Choices, cards.Choice{Value: c.Value, Label: c.Label, Meaning: c.Meaning})
			}
			return f, true
		}
		f.Kind = cards.FieldText
		if len(row.Choices) > 0 {
			words := make([]string, len(row.Choices))
			for i, c := range row.Choices {
				words[i] = c.Value
			}
			f.Help = tr(txt.HubsSettingsOpenChoices, strings.Join(words, ", "))
		}
	case "secret":
		f.Kind, f.Value, f.Help = cards.FieldSecret, "", tr(txt.HubsSettingsSecretHelp)
	case "int", "float", "duration":
		f.Kind = cards.FieldNumber
		if row.Unit != "" {
			f.Label += " (" + row.Unit + ")"
		}
		if row.Null != "" {
			f.Placeholder = row.Null
		}
	case "list":
		f.Kind, f.Help = cards.FieldText, tr(txt.HubsSettingsListHelp)
	case "string", "path":
		f.Kind = cards.FieldText
		if row.Null != "" {
			f.Placeholder = row.Null
		}
	default:
		return f, false
	}
	return f, true
}

// Value a setting of the kind takes from the text of its field
func settingValue(kind, text string) (any, error) {
	text = strings.TrimSpace(text)
	switch kind {
	case "bool":
		return text == "true", nil
	case "int", "float", "duration":
		if text == "" {
			return nil, nil
		}
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("%s", tr(txt.KitFormNumber))
		}
		return n, nil
	case "list":
		var out []any
		for _, part := range strings.Split(text, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out, nil
	}
	return text, nil
}

// Entries of a setting that holds named entries, each changed, added or taken out on the hub, the whole setting written after each change
func (a *app) settingsMap(h *cards.Hub, row tuigateway.SettingsRow, sid string, done func() tea.Cmd) tea.Cmd {
	entries, _ := row.Value.(map[string]any)
	entries = maps.Clone(entries)
	if entries == nil {
		entries = map[string]any{}
	}
	var names []string
	var list *cards.List
	sorted := func() {
		names = slices.Sorted(maps.Keys(entries))
		list.Refresh()
	}
	write := func(next map[string]any, said string) tea.Cmd {
		p := tuigateway.SettingsSetParams{SessionID: optionalText(sid), Key: row.Key, Value: next}
		return hubCall(a, h, "", tuigateway.SettingsSet, p, func(r *tuigateway.SettingsWriteResult) tea.Cmd {
			entries = next
			if m, ok := r.Row.Value.(map[string]any); ok {
				entries = maps.Clone(m)
			}
			sorted()
			return tea.Batch(h.Done(said), done())
		})
	}
	// Schema of an entry's value as the registry describes it, a text field for an entry of an open map
	entryField := func(name string, value any, then func(f cards.Field, kind string) tea.Cmd) tea.Cmd {
		p := tuigateway.SettingsListParams{SessionID: optionalText(sid), Keys: []string{row.Key + "." + name}, Advanced: ptr(true)}
		return call(a.gw, tuigateway.SettingsList, p, func(r *tuigateway.SettingsListResult, err error) tea.Cmd {
			if err == nil && len(r.Rows) == 1 {
				entry := r.Rows[0]
				entry.Value = value
				if f, ok := settingField(entry, "value", tr(txt.HubsSettingsEntryValue)); ok {
					return then(f, entry.Kind)
				}
			}
			text := ""
			if value != nil {
				text = settingEditText(tuigateway.SettingsRow{Value: value})
			}
			return then(cards.Field{ID: "value", Label: tr(txt.HubsSettingsEntryValue), Kind: cards.FieldText, Value: text, Help: tr(txt.HubsSettingsEntryHelp)}, "")
		})
	}
	// Value of an entry from its field, JSON taken as JSON for an entry of an open map
	parse := func(kind, text string) (any, error) {
		if kind != "" {
			return settingValue(kind, text)
		}
		var v any
		if err := json.Unmarshal([]byte(text), &v); err == nil {
			return v, nil
		}
		return strings.TrimSpace(text), nil
	}
	editEntry := func(name string) tea.Cmd {
		return entryField(name, entries[name], func(f cards.Field, kind string) tea.Cmd {
			spec := cards.FormSpec{Title: row.Label + " › " + name, Fields: []cards.Field{f}, Submit: tr(txt.HubsSettingsSave)}
			return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
				value, err := parse(kind, v["value"])
				if err != nil {
					return h.Fail(err.Error())
				}
				next := maps.Clone(entries)
				next[name] = value
				return write(next, tr(txt.HubsSettingsEntrySaved, name))
			})
		})
	}
	list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: row.Label, Key: "tui:settings:map", Context: settingsRowsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColName), Max: 32}, {Title: tr(txt.HubsColValue), Flex: 1}},
		Rows: func() [][]string {
			out := make([][]string, len(names))
			for i, n := range names {
				out[i] = []string{n, settingValueText(tuigateway.SettingsRow{Value: entries[n]})}
			}
			return out
		},
		Help:  func(int) []string { return append([]string{row.Short, ""}, settingDetail(row)...) },
		Empty: func() string { return tr(txt.HubsSettingsMapEmpty, a.keys.Display("hub.add")) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: tr(txt.HintsLabelChange)}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Free: map[string]bool{"hub.add": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.use":  func(_ *cards.List, i int) tea.Cmd { return editEntry(names[i]) },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd { return editEntry(names[i]) },
			"hub.add": func(*cards.List, int) tea.Cmd {
				return a.hubAsk(h, tr(txt.HubsSettingsEntryAdd, row.Label), tr(txt.HubsSettingsEntryName), "", false, func(name string) tea.Cmd {
					if _, taken := entries[name]; taken {
						return editEntry(name)
					}
					return entryField(name, nil, func(f cards.Field, kind string) tea.Cmd {
						spec := cards.FormSpec{Title: row.Label + " › " + name, Fields: []cards.Field{f}, Submit: tr(txt.HubsSettingsAddSubmit)}
						return a.hubForm(h, spec, func(_ *cards.Form, v map[string]string) tea.Cmd {
							value, err := parse(kind, v["value"])
							if err != nil {
								return h.Fail(err.Error())
							}
							next := maps.Clone(entries)
							next[name] = value
							return write(next, tr(txt.HubsSettingsEntryAdded, name))
						})
					})
				})
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				name := names[i]
				l.Confirm(tr(txt.HubsSettingsEntryRemoveConfirm, name), func() tea.Cmd {
					next := maps.Clone(entries)
					delete(next, name)
					return write(next, tr(txt.HubsSettingsEntryRemoved, name))
				})
				return nil
			},
		},
	})
	cmd := h.Push(list)
	sorted()
	return cmd
}

// Value of a setting as one short cell
func settingValueText(r tuigateway.SettingsRow) string {
	switch v := r.Value.(type) {
	case nil:
		if r.Null != "" {
			return r.Null
		}
		return "—"
	case bool:
		if v {
			return tr(txt.HubsSettingsOn)
		}
		return tr(txt.HubsSettingsOff)
	case string:
		if r.Kind == "enum" {
			for _, c := range r.Choices {
				if c.Value == v {
					return c.Label
				}
			}
		}
		if v == "" {
			return "—"
		}
		return v
	case float64:
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if r.Unit != "" {
			text += " " + r.Unit
		}
		return text
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return tr(txt.HubsSettingsEntries, len(v))
	}
	raw, _ := json.Marshal(r.Value)
	return string(raw)
}

// Text a setting's value is edited from
func settingEditText(r tuigateway.SettingsRow) string {
	switch v := r.Value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, ", ")
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	raw, _ := json.Marshal(r.Value)
	return string(raw)
}

// Mark of where a setting's value comes from: yours, this session's, the environment's or locked, nothing for the default
func settingSourceMark(r tuigateway.SettingsRow) string {
	switch {
	case r.Locked:
		return "🔒"
	case r.Source == "global":
		return "●"
	case r.Source == "session":
		return "◆"
	case r.Source == "env":
		return "$"
	}
	return ""
}

// Where a setting's value comes from, in plain words
func settingSourceText(source string) string {
	switch source {
	case "global":
		return tr(txt.HubsSettingsSourceGlobal)
	case "workspace":
		return tr(txt.HubsSettingsSourceWorkspace)
	case "session":
		return tr(txt.HubsSettingsSourceSession)
	case "env":
		return tr(txt.HubsSettingsSourceEnv)
	case "managed":
		return tr(txt.HubsSettingsSourceManaged)
	}
	return tr(txt.HubsSettingsSourceDefault)
}

// Lines of a setting's detail pane: what it does, its default, where its value comes from and when a change applies
func settingDetail(r tuigateway.SettingsRow) []string {
	out := []string{r.Label + " · " + r.Key, r.Short}
	if r.Help != "" {
		out = append(out, r.Help)
	}
	out = append(out, tr(txt.HubsSettingsDetailDefault, settingValueText(tuigateway.SettingsRow{Kind: r.Kind, Value: r.Default, Choices: r.Choices, Unit: r.Unit, Null: r.Null})))
	out = append(out, tr(txt.HubsSettingsDetailSource, settingSourceText(r.Source)))
	if r.Session != nil && r.Source != "session" {
		out = append(out, tr(txt.HubsSettingsDetailSessionValue, settingValueText(tuigateway.SettingsRow{Kind: r.Kind, Value: r.Session, Choices: r.Choices, Unit: r.Unit})))
	}
	if r.Apply != "" && r.Apply != "live" {
		out = append(out, tr(txt.HubsSettingsDetailApply, settingApplyText(r.Apply)))
	}
	for _, c := range r.Choices {
		if c.Meaning != "" {
			out = append(out, "  "+c.Label+": "+c.Meaning)
		}
	}
	return out
}

// Every other place that changes a setting, for its help: its shortcut, the setup section that asks it and the environment, nothing when there is none
func settingPlaces(r tuigateway.SettingsRow) []string {
	var places []string
	if r.Slash != "" {
		places = append(places, "  "+tr(txt.HubsSettingsPlaceSlash, r.Slash))
	}
	for _, title := range setupSectionsWriting(r.Key) {
		places = append(places, "  "+tr(txt.HubsSettingsPlaceSetup, title))
	}
	if r.Env != "" {
		places = append(places, "  "+tr(txt.HubsSettingsPlaceEnv, r.Env))
	}
	if len(places) == 0 {
		return nil
	}
	return append([]string{"", tr(txt.HubsSettingsPlacesTitle)}, places...)
}

// When a change takes effect, in plain words
func settingApplyText(apply string) string {
	switch apply {
	case "next_turn":
		return tr(txt.HubsSettingsApplyNextTurn)
	case "new_session":
		return tr(txt.HubsSettingsApplyNewSession)
	case "restart":
		return tr(txt.HubsSettingsApplyRestart)
	}
	return tr(txt.HubsSettingsApplyLive)
}

// Note after a setting was written, saying when the change takes effect unless it is live
func settingAppliedText(row tuigateway.SettingsRow, r *tuigateway.SettingsWriteResult) string {
	text := tr(txt.HubsSettingsSaved, row.Label, settingValueText(r.Row))
	if r.Apply != "live" && r.Apply != "" {
		text += " " + tr(txt.HubsSettingsDetailApply, settingApplyText(r.Apply))
	}
	return text
}
