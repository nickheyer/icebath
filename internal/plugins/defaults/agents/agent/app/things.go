package app

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What the name of an entry of a table may hold: lower case letters, digits, - and _, a letter or digit first
var entryName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Why a name typed for a new entry can't be kept, empty when it can
func nameRefused(typed, noun string, held map[string]any) string {
	name := strings.ToLower(strings.TrimSpace(typed))
	switch {
	case name == "":
		return ""
	case !entryName.MatchString(name):
		return "Use lower case letters, digits, - and _, starting with a letter or digit."
	}
	if _, taken := held[name]; taken {
		return "There is " + article(noun) + " " + noun + " named " + name + " already."
	}
	return ""
}

// Indefinite article before a noun as it is said
func article(noun string) string {
	if noun != "" && strings.ContainsRune("aeiou", rune(noun[0])) {
		return "an"
	}
	return "a"
}

// Keeps one entry of a table under its own key through the settings service and its owner's checks, null removing it, then reads the
// settings again and runs then. An entry whose name holds a dot, which a key can't name, is written with the whole table
func (p *agentPage) keepEntry(table, name, value string, then func() tea.Cmd) tea.Cmd {
	key := table + "." + name
	if strings.Contains(name, ".") {
		whole := maps.Clone(groupOf(p.options.Setting(table).GetValue()))
		if whole == nil {
			whole = map[string]any{}
		}
		var entry any
		if err := yaml.Unmarshal([]byte(value), &entry); err != nil {
			return p.hub.Fail(err.Error())
		}
		if entry == nil {
			delete(whole, name)
		} else {
			whole[name] = entry
		}
		encoded, err := yaml.Marshal(whole)
		if err != nil {
			return p.hub.Fail(err.Error())
		}
		key, value = table, strings.TrimSpace(string(encoded))
	}
	p.hub.Busy("Saving…")
	return tui.Async(func() func() tea.Cmd {
		_, err := p.d.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: key, Value: value})
		return func() tea.Cmd {
			if err != nil {
				return p.hub.Fail(tui.Reason(err))
			}
			p.hub.Quiet()
			return p.options.Load(then)
		}
	})
}

// Entries of a table setting by name, in the order of their names
func entriesOf(setting *configv1.Setting) (map[string]any, []string) {
	held := groupOf(setting.GetValue())
	return held, slices.Sorted(maps.Keys(held))
}

// Text of a field of an entry, empty for none
func fieldText(entry any, field string) string {
	group, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	if text, ok := group[field].(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// Page of the model shortcuts: each shortcut with the model it stands for, the last row adding one; the page of the shortcut named open
// over it, none for no name
func (p *agentPage) shortcuts(open string) tea.Cmd {
	t := p.kit.Env.Theme
	names := map[string]string{}
	setting := func() *configv1.Setting { return p.options.Setting(agentconfig.Aliases) }
	var list *cards.List
	list = pageList(p.kit, pageSpec{
		title: "Model shortcuts",
		key:   agentScreen + ":shortcuts",
		intro: func() []string {
			lines := []string{"Short names you can use in place of a model's full name."}
			if held, _ := entriesOf(setting()); len(held) == 0 {
				lines = append(lines, "No shortcuts yet.")
			}
			return lines
		},
		rows: func() []pageRow {
			held, order := entriesOf(setting())
			var rows []pageRow
			for _, name := range order {
				rows = append(rows, pageRow{cells: []string{name, "", render.Dim(shortcutWords(held[name], names), t)},
					detail: []string{"Type /model " + name + " to use it in a chat."}, words: shortcutWords(held[name], names),
					hints: []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}},
					use:   func(*cards.List) tea.Cmd { return p.shortcut(name, names) },
					remove: func(l *cards.List) tea.Cmd {
						return p.removeShortcut(l, name)
					}})
			}
			return append(rows, addRow(p.kit, "", "Add a shortcut", "A short name for a model, which /model takes.",
				func() tea.Cmd { return p.addShortcut(list) }))
		},
		add: func() tea.Cmd { return p.addShortcut(list) },
	})
	p.options.Watch(list)
	pushed := p.hub.Push(list)
	// The page of one shortcut opens over the list when one there is is named
	var page tea.Cmd
	if held, _ := entriesOf(setting()); held[open] != nil {
		page = p.shortcut(open, names)
	}
	return tea.Batch(pushed, page, tui.Async(func() func() tea.Cmd {
		read := p.d.providerNames(p.kit.Env.Ctx, p.profile)
		return func() tea.Cmd {
			maps.Copy(names, read)
			list.Refresh()
			return nil
		}
	}))
}

// The model a shortcut stands for, as a row says it: the model and its provider, or the address of its own server
func shortcutWords(entry any, names map[string]string) string {
	model := cmp.Or(fieldText(entry, "model"), "No model")
	switch name, base := fieldText(entry, "provider"), fieldText(entry, "base_url"); {
	case base != "":
		return model + " · " + base
	case name != "":
		return model + " · " + cmp.Or(names[name], name)
	}
	return model
}

// Adds a shortcut in two steps, its name and then its model, and keeps it once the model is picked
func (p *agentPage) addShortcut(list *cards.List) tea.Cmd {
	f := p.kit.Flow(p.hub, "Add a shortcut")
	held, _ := entriesOf(p.options.Setting(agentconfig.Aliases))
	field := cards.Field{ID: "name", Label: "Shortcut name", Required: true, Help: "The short name you type with /model, such as fast.",
		Check: func(typed string) string { return nameRefused(typed, "shortcut", held) }}
	return f.Ask("", []cards.Field{field}, "Next", func(values map[string]string) tea.Cmd {
		name := strings.ToLower(strings.TrimSpace(values["name"]))
		ask := tui.Ask{Title: "Model of " + name, Intro: []string{"The model that " + name + " stands for."}, Profile: p.profile, Verb: "Add",
			Values: map[string]string{}}
		ask.Done = func(picked map[string]string) tea.Cmd {
			provider, model := plain(picked[providerconfig.AskProvider]), plain(picked[providerconfig.AskModel])
			if model == "" {
				return p.hub.Fail("Pick a model for " + name + " to stand for.")
			}
			value := "{model: " + yamlText(model)
			if provider != "" {
				value += ", provider: " + yamlText(provider)
			}
			return p.keepEntry(agentconfig.Aliases, name, value+"}", func() tea.Cmd {
				list.Refresh()
				_, order := entriesOf(p.options.Setting(agentconfig.Aliases))
				list.Select(slices.Index(order, name))
				return tea.Batch(f.End(), p.hub.Done("Added the shortcut "+name+"."))
			})
		}
		cmd, _ := p.kit.Choose(p.hub, providerconfig.PickModel, ask)
		return cmd
	})
}

// Asks before a shortcut is removed, then removes it
func (p *agentPage) removeShortcut(l *cards.List, name string) tea.Cmd {
	l.Confirm(cards.Question{Text: "Remove the shortcut " + name + "? /model " + name + " no longer works.", Verb: "Remove", Deletes: true},
		func() tea.Cmd {
			return p.keepEntry(agentconfig.Aliases, name, "null", func() tea.Cmd {
				return tea.Batch(p.closeView(agentScreen+":shortcut:"+name), p.hub.Done("Removed the shortcut "+name+"."))
			})
		})
	return nil
}

// Closes the view with the key while the hub holds it on top of the trail
func (p *agentPage) closeView(key string) tea.Cmd {
	if top, ok := p.hub.Top().(interface{ Key() string }); ok && top.Key() == key {
		return p.hub.Pop()
	}
	return nil
}

// Page of one shortcut: what it stands for, its model, and under Advanced an address and key of its own; then Remove
func (p *agentPage) shortcut(name string, names map[string]string) tea.Cmd {
	var list *cards.List
	list = pageList(p.kit, pageSpec{
		title: name,
		key:   agentScreen + ":shortcut:" + name,
		intro: func() []string {
			held, _ := entriesOf(p.options.Setting(agentconfig.Aliases))
			return []string{"Stands for " + shortcutWords(held[name], names) + "."}
		},
		rows: func() []pageRow {
			everyday, _ := p.options.Entry(agentconfig.Aliases, name)
			rows := optionRows(p.options, "", everyday...)
			cells, open, ok := p.options.EntryAdvanced(agentconfig.Aliases, name)
			rows = append(rows, advancedRow(cells, open, ok, "An address and key of its own, for a model served outside your providers.")...)
			return append(rows, pageRow{cells: []string{"Remove", "", ""}, detail: []string{"Take the shortcut " + name + " out of icebath."},
				hints: []wm.Hint{{Action: "hub.use", Label: "remove"}}, use: func(l *cards.List) tea.Cmd { return p.removeShortcut(l, name) }})
		},
	})
	p.options.Watch(list)
	return p.hub.Push(list)
}

// What a personality says of itself as a row shows it: its description, else the first line of what it tells the agent
func personalityWords(entry any) string {
	if text, ok := entry.(string); ok {
		first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
		return first
	}
	for _, field := range []string{"description", "system_prompt", "tone", "style"} {
		if text := fieldText(entry, field); text != "" {
			first, _, _ := strings.Cut(text, "\n")
			return first
		}
	}
	return ""
}

// Page of your personalities: each with what it is, the last row adding one; the page of the personality named open over it, none for no
// name
func (p *agentPage) personalities(open string) tea.Cmd {
	t := p.kit.Env.Theme
	setting := func() *configv1.Setting { return p.options.Setting(agentconfig.Personalities) }
	var list *cards.List
	list = pageList(p.kit, pageSpec{
		title: "Your personalities",
		key:   agentScreen + ":personalities",
		intro: func() []string {
			lines := []string{"Personalities of your own that you can pick by name."}
			if held, _ := entriesOf(setting()); len(held) == 0 {
				lines = append(lines, "No personalities yet.")
			}
			return lines
		},
		rows: func() []pageRow {
			held, order := entriesOf(setting())
			var rows []pageRow
			for _, name := range order {
				words := personalityWords(held[name])
				rows = append(rows, pageRow{cells: []string{name, "", render.Dim(words, t)}, detail: []string{p.personalityState(name)}, words: words,
					hints: []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}},
					use:   func(*cards.List) tea.Cmd { return p.personality(name) },
					remove: func(l *cards.List) tea.Cmd {
						return p.removePersonality(l, name)
					}})
			}
			return append(rows, addRow(p.kit, "", "Add a personality", "A personality of your own: the name you pick it by and what it tells the agent.",
				func() tea.Cmd { return p.addPersonality(list) }))
		},
		add: func() tea.Cmd { return p.addPersonality(list) },
	})
	p.options.Watch(list)
	// The page of one personality opens over the list when one there is is named
	if held, _ := entriesOf(setting()); held[open] != nil {
		return tea.Batch(p.hub.Push(list), p.personality(open))
	}
	return p.hub.Push(list)
}

// Whether new chats use the personality, as a sentence
func (p *agentPage) personalityState(name string) string {
	if plain(p.options.Setting(agentconfig.Personality).GetValue()) == name {
		return "New chats use this personality."
	}
	return "New chats don't use this personality."
}

// Adds a personality in two steps, its name and then what it tells the agent, and keeps it once both are given
func (p *agentPage) addPersonality(list *cards.List) tea.Cmd {
	f := p.kit.Flow(p.hub, "Add a personality")
	held, _ := entriesOf(p.options.Setting(agentconfig.Personalities))
	field := cards.Field{ID: "name", Label: "Personality name", Required: true, Help: "The name you pick it by, such as calm.",
		Check: func(typed string) string {
			if neutral := strings.ToLower(strings.TrimSpace(typed)); slices.Contains([]string{"none", "default", "neutral"}, neutral) {
				return neutral + " means no personality. Pick another name."
			}
			return nameRefused(typed, "personality", held)
		}}
	return f.Ask("", []cards.Field{field}, "Next", func(values map[string]string) tea.Cmd {
		name := strings.ToLower(strings.TrimSpace(values["name"]))
		instructions := cards.Field{ID: "instructions", Label: "Instructions", Required: true,
			Help: "What the agent is told to be and do, such as You are a calm assistant who answers in few words."}
		return f.Ask("", []cards.Field{instructions}, "Add", func(values map[string]string) tea.Cmd {
			text := strings.TrimSpace(values["instructions"])
			return p.keepEntry(agentconfig.Personalities, name, "{system_prompt: "+yamlText(text)+"}", func() tea.Cmd {
				list.Refresh()
				_, order := entriesOf(p.options.Setting(agentconfig.Personalities))
				list.Select(slices.Index(order, name))
				return tea.Batch(f.End(), p.hub.Done("Added the personality "+name+"."))
			})
		})
	})
}

// Asks before a personality is removed, then removes it; new chats that used it go back to the agent's own tone
func (p *agentPage) removePersonality(l *cards.List, name string) tea.Cmd {
	inUse := plain(p.options.Setting(agentconfig.Personality).GetValue()) == name
	text := "Remove the personality " + name + "? Nothing picks it any more."
	if inUse {
		text = "Remove the personality " + name + "? New chats go back to the agent's own tone."
	}
	l.Confirm(cards.Question{Text: text, Verb: "Remove", Deletes: true}, func() tea.Cmd {
		return p.keepEntry(agentconfig.Personalities, name, "null", func() tea.Cmd {
			closed, said := p.closeView(agentScreen+":personality:"+name), "Removed the personality "+name+"."
			if !inUse {
				return tea.Batch(closed, p.hub.Done(said))
			}
			return tea.Batch(closed, p.keepPersonality("", said))
		})
	})
	return nil
}

// Sets the personality of new chats, none for the agent's own tone, through the settings service, saying said once it is kept and telling
// the screens that follow the setting
func (p *agentPage) keepPersonality(name, said string) tea.Cmd {
	value := quoted(name)
	return tui.Async(func() func() tea.Cmd {
		_, err := p.d.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: agentconfig.Personality, Value: value})
		return func() tea.Cmd {
			if err != nil {
				return p.hub.Fail(tui.Reason(err))
			}
			return tea.Batch(p.hub.Done(said), p.kit.Changed(agentconfig.Personality, value), p.options.Load(nil))
		}
	})
}

// Page of one personality: whether new chats use it, the act that makes them, what it tells the agent, then Remove
func (p *agentPage) personality(name string) tea.Cmd {
	var list *cards.List
	list = pageList(p.kit, pageSpec{
		title: name,
		key:   agentScreen + ":personality:" + name,
		intro: func() []string { return []string{p.personalityState(name)} },
		rows: func() []pageRow {
			var rows []pageRow
			if plain(p.options.Setting(agentconfig.Personality).GetValue()) != name {
				rows = append(rows, pageRow{cells: []string{"Use it in new chats", "", ""}, detail: []string{"Make it the personality of new chats."},
					hints: []wm.Hint{{Action: "hub.use", Label: "use"}},
					use: func(*cards.List) tea.Cmd {
						return p.keepPersonality(name, fmt.Sprintf("Personality is now %s.", strs.Capital(name)))
					}})
			}
			everyday, _ := p.options.Entry(agentconfig.Personalities, name)
			rows = append(rows, optionRows(p.options, "", everyday...)...)
			cells, open, ok := p.options.EntryAdvanced(agentconfig.Personalities, name)
			rows = append(rows, advancedRow(cells, open, ok, "Settings of the personality that few people change.")...)
			return append(rows, pageRow{cells: []string{"Remove", "", ""}, detail: []string{"Take the personality " + name + " out of icebath."},
				hints: []wm.Hint{{Action: "hub.use", Label: "remove"}}, use: func(l *cards.List) tea.Cmd { return p.removePersonality(l, name) }})
		},
	})
	p.options.Watch(list)
	return p.hub.Push(list)
}
