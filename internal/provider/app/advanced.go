package app

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// How many settings the Advanced page of a provider holds: those few people change, and the limits by model
func (p *providers) rare(entry *providerv1.Provider) int {
	_, rare := p.options.Section(entry.GetSection())
	return len(rare) + 1
}

// Opens the Advanced page of a provider: its settings that few people change, as Settings draws them, then its time limits by model
func (p *providers) advanced(name string) tea.Cmd {
	t := p.kit.Env.Theme
	entry := p.find(name)
	if entry == nil {
		return nil
	}
	var rows []*configv1.Setting
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Advanced",
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + name + ":advanced",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Settings of " + entry.GetDisplayName() + " that few people change."} },
		Columns: pageColumns,
		Rows: func() [][]string {
			_, rows = p.options.Section(entry.GetSection())
			cells := make([][]string, 0, len(rows)+1)
			for _, setting := range rows {
				cells = append(cells, p.options.Cells(setting))
			}
			return append(cells, []string{"Limits by model", render.Dim(t.Glyphs.Forward, t), render.Dim(timeoutsWords(p.timeouts(entry)), t)})
		},
		Detail: func(i int) []string {
			if i >= 0 && i < len(rows) {
				return p.options.Detail(rows[i], false)
			}
			return []string{"Time limits for single models of " + entry.GetDisplayName() + "."}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i >= 0 && i < len(rows) {
				return p.options.Hints(rows[i])
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i < len(rows) {
					return p.options.Change(rows[i])
				}
				return p.limitsByModel(name)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i < len(rows) {
					return p.options.Reset(l, rows[i])
				}
				return nil
			},
		},
	})
	list.ToggleDetail()
	list.Refresh()
	p.views.Add(list)
	p.options.Watch(list)
	return p.hub.Push(list)
}

// Time limits of single models of the provider as kept, by model
func (p *providers) timeouts(entry *providerv1.Provider) map[string]map[string]any {
	setting := p.options.Setting(entry.GetSection() + "." + providerconfig.ModelTimeouts)
	var table map[string]map[string]any
	if setting != nil {
		yaml.Unmarshal([]byte(setting.GetValue()), &table)
	}
	return table
}

// The models with time limits of their own, in words
func timeoutsWords(table map[string]map[string]any) string {
	if len(table) == 0 {
		return "None"
	}
	return render.Count(len(table), "model")
}

// Seconds a limit of a model holds in words
func seconds(value any) string {
	n, _ := value.(int)
	if n <= 0 {
		return "As the provider"
	}
	return render.Count(n, "second")
}

// Opens the time limits of single models of the provider: one row per model with its limits, then the row that adds one
func (p *providers) limitsByModel(name string) tea.Cmd {
	t := p.kit.Env.Theme
	entry := p.find(name)
	if entry == nil {
		return nil
	}
	var models []string
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Limits by model",
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + name + ":timeouts",
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return []string{"Time limits for single models of " + entry.GetDisplayName() + "."} },
		Columns: pageColumns,
		Rows: func() [][]string {
			table := p.timeouts(entry)
			models = slices.Sorted(maps.Keys(table))
			cells := make([][]string, 0, len(models)+1)
			for _, model := range models {
				limits := table[model]
				cells = append(cells, []string{model, "", render.Dim(seconds(limits[providerconfig.ModelRequestTimeout])+" · silence "+
					strings.ToLower(seconds(limits[providerconfig.ModelStaleTimeout])), t)})
			}
			return append(cells, []string{"Add a limit for a model", render.Dim(t.Glyphs.Forward, t), ""})
		},
		Detail: func(i int) []string {
			if i < len(models) {
				return []string{"The time limits of " + models[i] + " over those of " + entry.GetDisplayName() + "."}
			}
			return []string{"Pick a model of " + entry.GetDisplayName() + " and give it time limits of its own."}
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i >= 0 && i < len(models) {
				return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i < len(models) {
					return p.modelLimits(entry, models[i])
				}
				return p.addModelLimit(entry)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(models) {
					return nil
				}
				model := models[i]
				l.Confirm(cards.Question{Text: "Remove the time limits of " + model + "? It takes those of " + entry.GetDisplayName() + " again.",
					Verb: "Remove"}, func() tea.Cmd {
					return p.keepLimits(entry, model, nil, model+" takes the time limits of "+entry.GetDisplayName()+" again.")
				})
				return nil
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return p.addModelLimit(entry) },
		},
	})
	list.ToggleDetail()
	list.Refresh()
	p.views.Add(list)
	p.options.Watch(list)
	return p.hub.Push(list)
}

// Fields of the time limits of one model, as typed numbers of seconds
func limitFields(limits map[string]any) []cards.Field {
	value := func(name string) string {
		if n, _ := limits[name].(int); n > 0 {
			return strconv.Itoa(n)
		}
		return ""
	}
	check := func(text string) string {
		if text = strings.TrimSpace(text); text == "" {
			return ""
		}
		if n, err := strconv.Atoi(text); err != nil || n < 0 {
			return "Type a whole number of seconds."
		}
		return ""
	}
	return []cards.Field{
		{ID: providerconfig.ModelRequestTimeout, Label: "Request time limit", Value: value(providerconfig.ModelRequestTimeout), Check: check,
			Help: "Seconds one request to this model may take, such as 600. Left empty, the provider's limit applies."},
		{ID: providerconfig.ModelStaleTimeout, Label: "Silence limit", Value: value(providerconfig.ModelStaleTimeout), Check: check,
			Help: "Seconds a reply of this model may stay silent, such as 120. Left empty, the provider's limit applies."},
	}
}

// Picks a model of the provider, then asks its time limits; written once they are given
func (p *providers) addModelLimit(entry *providerv1.Provider) tea.Cmd {
	key := entry.GetSection() + "." + providerconfig.ModelTimeouts
	ask := tui.Ask{Title: "Add a limit for a model", Intro: []string{"Pick the model of " + entry.GetDisplayName() + " to give time limits of its own."},
		Key: key, Values: map[string]string{}, Profile: p.profile}
	ask.Done = func(values map[string]string) tea.Cmd {
		var model string
		yaml.Unmarshal([]byte(values[key]), &model)
		if model == "" {
			return nil
		}
		return p.modelLimits(entry, model)
	}
	cmd, _ := p.kit.Choose(p.hub, providerconfig.PickModel, ask)
	return cmd
}

// Asks the time limits of one model of the provider and keeps them; none given removes them
func (p *providers) modelLimits(entry *providerv1.Provider, model string) tea.Cmd {
	j := newJob(p.kit, p.hub, model)
	return j.Ask("The time limits of "+model+" over those of "+entry.GetDisplayName()+".", limitFields(p.timeouts(entry)[model]), "Save",
		func(values map[string]string) tea.Cmd {
			limits := map[string]any{}
			for _, name := range []string{providerconfig.ModelRequestTimeout, providerconfig.ModelStaleTimeout} {
				if n, err := strconv.Atoi(strings.TrimSpace(values[name])); err == nil && n > 0 {
					limits[name] = n
				}
			}
			said := model + " takes the time limits of " + entry.GetDisplayName() + " again."
			if len(limits) > 0 {
				said = "Kept the time limits of " + model + "."
			}
			return tea.Batch(j.End(), p.keepLimits(entry, model, limits, said))
		})
}

// Keeps the time limits of one model of the provider, none taking them away, and says what came of it. A model named with a dot, as many
// are, can't be one part of a key, which is split on its dots, so the limits by model are written whole with that model among them
func (p *providers) keepLimits(entry *providerv1.Provider, model string, limits map[string]any, said string) tea.Cmd {
	key := entry.GetSection() + "." + providerconfig.ModelTimeouts
	var kept any
	if len(limits) > 0 {
		kept = limits
	}
	if strings.Contains(model, ".") {
		table := map[string]map[string]any{}
		maps.Copy(table, p.timeouts(entry))
		delete(table, model)
		if len(limits) > 0 {
			table[model] = limits
		}
		kept = nil
		if len(table) > 0 {
			kept = table
		}
	} else {
		key += "." + model
	}
	value := "null"
	if kept != nil {
		encoded, err := yaml.Marshal(kept)
		if err != nil {
			return p.hub.Fail(err.Error())
		}
		value = strings.TrimSpace(string(encoded))
	}
	return p.store(entry, key, value, said)
}
