package app

import (
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Cells of the bar of a usage window
const usageBar = 10

// Why a list of the provider could not be read, in words
func unread(err error) string {
	return strings.TrimSuffix(tui.Reason(err), ".") + "."
}

// What a page of a provider's models knows: what the provider lists and of each model, and why it gave no list of its own
type listing struct {
	listed   []string
	details  map[string]*providerv1.Model
	failed   error
	loaded   bool
	fallback bool
	unlisted string
}

// Models the provider lists, each ticked or not for the model list; with none ticked it offers every model listed. A provider whose own
// list is missing takes a model name typed on purpose. ctrl+r reads the list from the provider again
func (p *providers) models(name string) tea.Cmd {
	t := p.kit.Env.Theme
	if p.find(name) == nil {
		return nil
	}
	current := func() *providerv1.Provider {
		if entry := p.find(name); entry != nil {
			return entry
		}
		return &providerv1.Provider{Name: name}
	}
	l := &listing{details: map[string]*providerv1.Model{}}
	// Models in the order they show, then the row that asks for a name while the provider's own list is missing or names no model
	all := func() []string {
		ids := slices.Clone(current().GetModels())
		for _, model := range l.listed {
			if !slices.Contains(ids, model) {
				ids = append(ids, model)
			}
		}
		if l.loaded && (l.failed != nil || l.unlisted != "" || len(ids) == 0) {
			ids = append(ids, "")
		}
		return ids
	}
	var list *cards.List
	read := func(refresh bool) tea.Cmd {
		if refresh {
			p.hub.Busy("Asking " + current().GetDisplayName() + " for its models…")
		}
		return tui.Async(func() func() tea.Cmd {
			response, err := p.service.Models(p.kit.Env.Ctx, &providerv1.ModelsRequest{Profile: p.profile, Provider: name, Refresh: refresh})
			return func() tea.Cmd {
				l.failed, l.loaded = err, true
				if err == nil {
					l.listed, l.fallback, l.unlisted = response.GetModels(), response.GetFallback(), response.GetUnlisted()
					for _, detail := range response.GetDetails() {
						l.details[detail.GetId()] = detail
					}
				}
				list.Refresh()
				switch {
				case refresh && err == nil && l.unlisted != "" && len(l.listed) == 0:
					return p.hub.Fail(l.unlisted)
				case refresh && err == nil:
					return p.hub.Done("Read the models of " + current().GetDisplayName() + " again: " + render.Count(len(l.listed), "model") + ".")
				}
				p.hub.Quiet()
				return nil
			}
		})
	}
	tick := func(model string, on bool) tea.Cmd {
		picked := slices.Clone(current().GetModels())
		if on && !slices.Contains(picked, model) {
			picked = append(picked, model)
		} else if !on {
			picked = slices.DeleteFunc(picked, func(have string) bool { return have == model })
		}
		value, err := yaml.Marshal(picked)
		if err != nil {
			return p.hub.Fail(err.Error())
		}
		return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
			return p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: current().GetSection() + "." + providerconfig.Models,
				Value: string(value)})
		}, func(*configv1.SetResponse) tea.Cmd {
			done := "The model list offers " + model + "."
			switch {
			case !on && len(picked) == 0:
				done = "The model list offers every model of " + current().GetDisplayName() + " again."
			case !on:
				done = "The model list no longer offers " + model + "."
			}
			return tea.Batch(p.hub.Done(done), p.load(name), p.options.Load(nil))
		})
	}
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Models offered",
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + name + ":models",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search models",
		Intro: func() []string {
			return []string{"The models of " + current().GetDisplayName() + " that the model list offers. While none is ticked, it offers them all."}
		},
		Columns: []render.Column{{}, {}, {}, {}, {Flex: 1}},
		Rows: func() [][]string {
			ids := all()
			rows := make([][]string, len(ids))
			for i, model := range ids {
				if model == "" {
					rows[i] = []string{"", render.Dim("Type a model name", t), "", "", ""}
					continue
				}
				box := "[ ]"
				if slices.Contains(current().GetModels(), model) {
					box = "[x]"
				}
				detail := l.details[model]
				window := ""
				if detail.GetContext() > 0 {
					window = models.Tokens(detail.GetContext()) + " context"
				}
				rows[i] = []string{box, model, render.Dim(window, t), render.Dim(prices(detail), t), render.Dim(detail.GetNote(), t)}
			}
			return rows
		},
		Words: func(i int) string { return l.details[all()[i]].GetAbilities() + " " + l.details[all()[i]].GetNote() },
		Detail: func(i int) []string {
			if all()[i] == "" {
				return []string{"Type the name of a model that " + current().GetDisplayName() + " serves, as it names it."}
			}
			return describeModel(l.details[all()[i]], t)
		},
		Banner: func() string {
			switch {
			case l.failed != nil:
				return render.Warm("The list could not be read: "+unread(l.failed), t)
			case l.fallback && l.unlisted != "":
				return render.Warm(l.unlisted+" These are the models icebath knows for it.", t)
			case l.fallback:
				return render.Warm(current().GetDisplayName()+" gave no list, so these are the models icebath knows for it.", t)
			case l.unlisted != "":
				return render.Warm(l.unlisted, t)
			case l.loaded && len(l.listed) == 0:
				return render.Warm(current().GetDisplayName()+" lists no model.", t)
			}
			return ""
		},
		Empty: func() string { return "Asking " + current().GetDisplayName() + " for its models…" },
		Free:  map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.refresh", Label: "read again"}}
			case all()[i] == "":
				return []wm.Hint{{Action: "hub.use", Label: "type"}, {Action: "hub.refresh", Label: "read again"}}
			case slices.Contains(current().GetModels(), all()[i]):
				return []wm.Hint{{Action: "hub.use", Label: "untick"}, {Action: "hub.refresh", Label: "read again"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "tick"}, {Action: "hub.refresh", Label: "read again"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				model := all()[i]
				if model != "" {
					return tick(model, !slices.Contains(current().GetModels(), model))
				}
				field := cards.Field{Label: "Model", Help: "The name of a model that " + current().GetDisplayName() + " serves, such as llama3.1:8b."}
				return p.kit.Ask(p.hub, "Type a model name", field, "Add", func(typed string) tea.Cmd { return tick(typed, true) })
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return read(true) },
		},
	})
	list.ToggleDetail()
	p.views.Add(list)
	return tea.Batch(p.hub.Push(list), read(false))
}

// Prices of a model's input and output for a million tokens, empty when not known
func prices(detail *providerv1.Model) string {
	if detail.GetInputPrice() == "" && detail.GetOutputPrice() == "" {
		return ""
	}
	shown := func(price string) string {
		if price == "" {
			return "unknown"
		}
		return models.PerMillion(price)
	}
	return shown(detail.GetInputPrice()) + " in · " + shown(detail.GetOutputPrice()) + " out"
}

// What is known of a model: how much it reads and writes at once, its prices and what it can do
func describeModel(detail *providerv1.Model, t theme.Theme) []string {
	if detail == nil {
		return nil
	}
	var lines []string
	if detail.GetContext() > 0 {
		lines = append(lines, "Reads up to "+strs.Thousands(detail.GetContext())+" tokens at once.")
	}
	if detail.GetMaxOutput() > 0 {
		lines = append(lines, "Writes up to "+strs.Thousands(detail.GetMaxOutput())+" tokens in one reply.")
	}
	if detail.GetInputPrice() != "" || detail.GetOutputPrice() != "" {
		line := "Price for a million tokens: " + prices(detail)
		if detail.GetCacheReadPrice() != "" {
			line += ", " + models.PerMillion(detail.GetCacheReadPrice()) + " read from the cache"
		}
		lines = append(lines, line+".")
	}
	if detail.GetAbilities() != "" {
		lines = append(lines, "Can do: "+detail.GetAbilities()+".")
	}
	switch {
	case detail.GetMandatoryReasoning():
		lines = append(lines, render.Dim("It always reasons before it answers.", t))
	case detail.Reasoning != nil && !detail.GetReasoning():
		lines = append(lines, render.Dim("It takes no reasoning effort.", t))
	case len(detail.GetEfforts()) > 0:
		lines = append(lines, render.Dim("Reasoning efforts: "+strings.Join(detail.GetEfforts(), ", ")+".", t))
	}
	if detail.GetNote() != "" {
		lines = append(lines, render.Dim("Marked "+detail.GetNote()+".", t))
	}
	return lines
}

// First window of a reading as a few words: its name, its bar and its share used
func glance(reading *providerv1.AccountUsage) string {
	for _, window := range reading.GetWindows() {
		if window.Used != nil {
			used := min(max(window.GetUsed(), 0), 100) / 100
			return window.GetLabel() + " " + provider.Bar(used, usageBar) + " " + provider.PercentUsed(used) + " used"
		}
	}
	return ""
}

// When a usage window starts over, in words
func resetWords(at time.Time) string {
	if time.Until(at) <= time.Minute {
		return "Starts over soon"
	}
	return "Starts over " + left(at) + ", " + clock(at)
}

// What the provider says the account in use may still spend, read now, each window a row with its bar and when it starts over. ctrl+r
// reads it again
func (p *providers) usage(name string) tea.Cmd {
	t := p.kit.Env.Theme
	entry := p.find(name)
	if entry == nil {
		return nil
	}
	var reading *providerv1.AccountUsage
	var failed error
	loaded := false
	var list *cards.List
	shown := entry.GetDisplayName()
	read := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := p.service.Usage(p.kit.Env.Ctx, &providerv1.UsageRequest{Profile: p.profile, Provider: name})
			return func() tea.Cmd {
				failed, loaded, reading = err, true, response.GetUsage()
				list.Refresh()
				return nil
			}
		})
	}
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Usage",
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + name + ":usage",
		Context: cards.HubContext,
		Intro: func() []string {
			lines := []string{"What " + shown + " says the account in use may still spend."}
			if plan := reading.GetPlan(); plan != "" {
				lines = append(lines, "Plan: "+plan)
			}
			return append(lines, reading.GetDetails()...)
		},
		Columns: []render.Column{{}, {}, {}, {Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(reading.GetWindows()))
			for i, window := range reading.GetWindows() {
				bar, share := "", render.Dim("Not known", t)
				if window.Used != nil {
					used := min(max(window.GetUsed(), 0), 100) / 100
					bar, share = provider.Bar(used, usageBar), provider.PercentUsed(used)+" used"
				}
				when := window.GetDetail()
				if resets := window.GetResets(); resets != nil {
					when = resetWords(resets.AsTime())
				}
				rows[i] = []string{window.GetLabel(), bar, share, render.Dim(when, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			window := reading.GetWindows()[i]
			var lines []string
			if window.GetModel() {
				lines = append(lines, "A limit on one family of models, not on the whole account.")
			}
			if window.Used != nil {
				lines = append(lines, strconv.Itoa(int(100-min(max(window.GetUsed(), 0), 100)))+"% is left.")
			}
			if fetched := reading.GetFetched(); fetched != nil {
				lines = append(lines, render.Dim("Read at "+clock(fetched.AsTime()), t))
			}
			return lines
		},
		Banner: func() string {
			if why := reading.GetUnavailable(); why != "" {
				return render.Warm(shown+" can't say what this account may spend: "+strings.TrimSuffix(why, ".")+".", t)
			}
			return ""
		},
		Empty: func() string {
			switch {
			case failed != nil:
				return unread(failed)
			case !loaded:
				return "Asking " + shown + " what the account may still spend…"
			case reading.GetUnavailable() != "":
				return ""
			}
			return shown + " said nothing of the limits of this account. " + p.kit.Env.Keys.Display("hub.refresh") + " asks again."
		},
		Free:    map[string]bool{"hub.refresh": true},
		Hints:   func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.refresh", Label: "read again"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.refresh": func(*cards.List, int) tea.Cmd { return read() }},
	})
	list.ToggleDetail()
	return tea.Batch(p.hub.Push(list), read())
}
