package app

import (
	"cmp"
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/delegation"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Provider and model a setting of a model keeps at the place of the key, such as delegation.model beside delegation.reasoning_effort; the
// agent's when that place names none, and a provider that names no provider of the profile, such as Automatic, read as none
func (d *desk) modelAt(ctx context.Context, profile, key string) (string, string) {
	prefix := key[:strings.LastIndex(key, ".")+1]
	get := func(key string) string {
		response, err := d.settings.Get(ctx, &configv1.GetRequest{Key: key, Profile: profile})
		if err != nil {
			return ""
		}
		return plain(response.GetSetting().GetValue())
	}
	name, model := get(prefix+"provider"), get(prefix+"model")
	if name == auxiliaryconfig.Auto || name == auxiliaryconfig.Main || name == auxiliaryconfig.Custom {
		name = ""
	}
	if model == "" && name == "" && prefix != agentconfig.Key+"." {
		name, model = get(agentconfig.Provider), get(agentconfig.Model)
	}
	return name, model
}

// What is known of the model of the provider: the provider in effect for none, the first that is ready, and its default model for no
// model; nil when the provider gives nothing of it
func (d *desk) modelDetail(ctx context.Context, profile, name, model string) *providerv1.Model {
	listed, err := d.providers.List(ctx, &providerv1.ListRequest{Profile: profile})
	if err != nil {
		return nil
	}
	var entry *providerv1.Provider
	for _, each := range listed.GetProviders() {
		if each.GetName() == name || name == "" && entry == nil && providerapp.Ready(each) {
			entry = each
		}
	}
	if entry == nil {
		return nil
	}
	model = cmp.Or(model, entry.GetDefaultModel())
	response, err := d.providers.Models(ctx, &providerv1.ModelsRequest{Provider: entry.GetName(), Profile: profile})
	if err != nil {
		return nil
	}
	for _, detail := range response.GetDetails() {
		if detail.GetId() == model {
			return detail
		}
	}
	return nil
}

// Choices of how hard a model thinks: the choices before them, then the efforts the model has, every effort when it says none, and no
// reasoning where it allows that; only the choices before them for a model known to take no effort
func effortChoices(first []tui.Choice, detail *providerv1.Model) []tui.Choice {
	choices := slices.Clone(first)
	if detail != nil && detail.Reasoning != nil && !detail.GetReasoning() {
		return choices
	}
	offered := detail.GetEfforts()
	if len(offered) == 0 {
		offered = provider.Efforts
	}
	for _, effort := range offered {
		if effort != "none" {
			choices = append(choices, tui.Choice{ID: effort, Title: effortTitle(effort)})
		}
	}
	if !detail.GetMandatoryReasoning() {
		choices = append(choices, tui.Choice{ID: "none", Title: effortTitle("none"), Detail: "Answers without thinking first"})
	}
	return choices
}

// Lists how hard the model can think over the hub, to pick one; done is handed the effort, empty for a choice before the efforts
func (d *desk) chooseEffort(kit *tui.Kit, hub *cards.Hub, profile, name, model string, first []tui.Choice, current string,
	done func(effort string) tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		detail := d.modelDetail(kit.Env.Ctx, profile, name, model)
		return func() tea.Cmd {
			intro := "How hard the model thinks before it answers."
			if model != "" {
				intro = "How hard " + model + " thinks before it answers."
			}
			return pickMarked(kit, hub, "Reasoning effort", []string{intro}, effortChoices(first, detail), current, done)
		}
	})
}

// Id of the choice that keeps nothing, which a list must mark as in use while nothing is kept
const unpicked = ":none"

// Kit.PickFrom of choices where the first, keeping nothing, has no id: it is marked as in use while current is empty, and done is handed
// an empty id for it
func pickMarked(kit *tui.Kit, hub *cards.Hub, title string, intro []string, choices []tui.Choice, current string, done func(id string) tea.Cmd) tea.Cmd {
	choices = slices.Clone(choices)
	for i := range choices {
		if choices[i].ID == "" {
			choices[i].ID = unpicked
		}
	}
	return kit.PickFrom(hub, title, intro, "choose", choices, cmp.Or(current, unpicked), func(choice tui.Choice) tea.Cmd {
		if choice.ID == unpicked {
			return done("")
		}
		return done(choice.ID)
	})
}

// Picker of how hard the model kept with a setting thinks: the efforts that model has, after what applies when none is picked
func (d *desk) effortPicker() tui.Picker {
	return tui.Picker{Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			name, model := d.modelAt(kit.Env.Ctx, ask.Profile, ask.Key)
			return func() tea.Cmd {
				return d.chooseEffort(kit, hub, ask.Profile, name, model, ask.First, plain(ask.Values[ask.Key]), func(effort string) tea.Cmd {
					return picked(ask, quoted(effort))
				})
			}
		})
	}}
}

// Hands the value picked back; the asker keeps it and ends the job
func picked(ask tui.Ask, value string) tea.Cmd {
	return ask.Done(map[string]string{ask.Key: value})
}

// Names providers are shown by in the profile, by their names there, for the rows that name them
func (d *desk) providerNames(ctx context.Context, profile string) map[string]string {
	names := map[string]string{}
	if listed, err := d.providers.List(ctx, &providerv1.ListRequest{Profile: profile}); err == nil {
		for _, entry := range listed.GetProviders() {
			names[entry.GetName()] = entry.GetDisplayName()
		}
	}
	return names
}

// Picker of the method subagents run with: the methods that can run, the one in use marked; one with settings of its own opens its page,
// where it is used and its settings changed, any other is used at once
func (d *desk) methodPicker() tui.Picker {
	return tui.Picker{Open: func(kit *tui.Kit, hub *cards.Hub, ask tui.Ask) tea.Cmd {
		options := configapp.NewRows(kit, hub, d.settings, ask.Profile)
		return options.Load(func() tea.Cmd {
			setting := options.Setting(ask.Key)
			if setting == nil {
				return nil
			}
			current := plain(ask.Values[ask.Key])
			var choices []tui.Choice
			for _, option := range setting.GetOptions() {
				choices = append(choices, tui.Choice{ID: option.GetValue(), Title: option.GetTitle(), Detail: option.GetDescription()})
			}
			return kit.PickFrom(hub, ask.Title, ask.Intro, "choose", choices, current, func(choice tui.Choice) tea.Cmd {
				everyday, advanced := options.Own(delegation.SettingsKey(choice.ID))
				if len(everyday)+len(advanced) == 0 {
					return picked(ask, quoted(choice.ID))
				}
				return hub.Push(d.methodPage(kit, hub, options, ask, choice, choice.ID == current))
			})
		})
	}}
}

// Page of a method of its own settings: whether it is the one in use, its settings, and those few people change
func (d *desk) methodPage(kit *tui.Kit, hub *cards.Hub, options *configapp.Rows, ask tui.Ask, method tui.Choice, inUse bool) *cards.List {
	key := delegation.SettingsKey(method.ID)
	var page *cards.List
	page = pageList(kit, pageSpec{
		title: method.Title,
		key:   "method:" + method.ID,
		intro: func() []string { return []string{cmp.Or(method.Detail, method.Title) + "."} },
		rows: func() []pageRow {
			var rows []pageRow
			if inUse {
				rows = append(rows, pageRow{cells: []string{"In use", "", ""}, detail: []string{"Subagents run with this method."}})
			} else {
				rows = append(rows, pageRow{cells: []string{"Use it", "", ""}, detail: []string{"Run subagents with this method."},
					hints: []wm.Hint{{Action: "hub.use", Label: "use"}},
					use: func(*cards.List) tea.Cmd {
						return tea.Batch(hub.Close(page), picked(ask, quoted(method.ID)))
					}})
			}
			everyday, _ := options.Own(key)
			rows = append(rows, optionRows(options, "Options", everyday...)...)
			cells, open, ok := options.OwnAdvanced(key)
			return append(rows, advancedRow(cells, open, ok, "Settings of "+method.Title+" that few people change.")...)
		},
	})
	options.Watch(page)
	return page
}
