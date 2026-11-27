package app

import (
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
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

// Why a list of the provider could not be read, in words: what to do when no account is connected, else the reason
func unread(err error, provider *providerv1.Provider, what string) string {
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		return "Connect an account to " + provider.GetDisplayName() + " to see " + what + "."
	}
	return strings.TrimSuffix(tui.Reason(err), ".") + "."
}

// Models the provider lists, each turned on or off for /model; with none on, /model offers every model listed. ctrl+r reads the list
// from the provider again
func (p *providers) models(pg *page) tea.Cmd {
	t := p.kit.Env.Theme
	var listed []string
	details := map[string]*providerv1.Model{}
	var failed error
	loaded, fallback := false, false
	all := func() []string {
		ids := slices.Clone(pg.provider.GetModels())
		for _, model := range listed {
			if !slices.Contains(ids, model) {
				ids = append(ids, model)
			}
		}
		return ids
	}
	var list *cards.List
	read := func(refresh bool) tea.Cmd {
		if refresh {
			p.hub.Busy("Asking " + pg.provider.GetDisplayName() + " for its models…")
		}
		return tui.Async(func() func() tea.Cmd {
			response, err := p.service.Models(p.kit.Env.Ctx, &providerv1.ModelsRequest{Profile: p.profile, Provider: pg.provider.GetName(), Refresh: refresh})
			return func() tea.Cmd {
				failed, loaded = err, true
				if err == nil {
					listed, fallback = response.GetModels(), response.GetFallback()
					for _, detail := range response.GetDetails() {
						details[detail.GetId()] = detail
					}
				}
				list.Refresh()
				if refresh && err == nil {
					return p.hub.Done("Read the models of " + pg.provider.GetDisplayName() + " again: " + render.Count(len(listed), "model") + ".")
				}
				p.hub.Quiet()
				return nil
			}
		})
	}
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Models",
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + pg.provider.GetName() + ":models",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Bar:     true,
		Search:  "Search models",
		Intro: func() []string {
			name := pg.provider.GetDisplayName()
			if len(pg.provider.GetModels()) == 0 {
				return []string{"Turn on the models that /model offers for " + name + ". While none is on, /model offers every model listed."}
			}
			return []string{"The models that /model offers for " + name + ". Turn them all off and /model offers every model listed."}
		},
		Columns: []render.Column{{}, {}, {}, {}, {Flex: 1}},
		Rows: func() [][]string {
			ids := all()
			rows := make([][]string, len(ids))
			for i, model := range ids {
				box := "[ ]"
				if slices.Contains(pg.provider.GetModels(), model) {
					box = "[x]"
				}
				detail := details[model]
				window := ""
				if detail.GetContext() > 0 {
					window = models.Tokens(detail.GetContext()) + " context"
				}
				rows[i] = []string{box, model, render.Dim(window, t), render.Dim(prices(detail), t), render.Dim(detail.GetNote(), t)}
			}
			return rows
		},
		Words:  func(i int) string { return details[all()[i]].GetAbilities() + " " + details[all()[i]].GetNote() },
		Detail: func(i int) []string { return describeModel(details[all()[i]], t) },
		Banner: func() string {
			switch {
			case failed != nil && len(all()) > 0:
				return render.Warm("The list could not be read: "+unread(failed, pg.provider, "its models"), t)
			case fallback:
				return render.Warm(pg.provider.GetDisplayName()+" gave no list, so these are the models icebath knows for it.", t)
			}
			return ""
		},
		Empty: func() string {
			switch {
			case failed != nil:
				return unread(failed, pg.provider, "its models")
			case !loaded:
				return "Asking " + pg.provider.GetDisplayName() + " for its models…"
			}
			return pg.provider.GetDisplayName() + " lists no models. " + p.kit.Env.Keys.Display("hub.refresh") + " asks again."
		},
		Free: map[string]bool{"hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.refresh", Label: "read again"}}
			}
			if slices.Contains(pg.provider.GetModels(), all()[i]) {
				return []wm.Hint{{Action: "hub.use", Label: "turn off"}, {Action: "hub.refresh", Label: "read again"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "turn on"}, {Action: "hub.refresh", Label: "read again"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				model := all()[i]
				picked := slices.Clone(pg.provider.GetModels())
				on := !slices.Contains(picked, model)
				if on {
					picked = append(picked, model)
				} else {
					picked = slices.DeleteFunc(picked, func(have string) bool { return have == model })
				}
				value, err := yaml.Marshal(picked)
				if err != nil {
					return p.hub.Fail(err.Error())
				}
				return tui.Call(p.hub, func() (*configv1.SetResponse, error) {
					return p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: pg.provider.GetSection() + "." + providerconfig.Models, Value: string(value)})
				}, func(*configv1.SetResponse) tea.Cmd {
					pg.provider.Models = picked
					list.Refresh()
					done := "/model now offers " + model + "."
					switch {
					case !on && len(picked) == 0:
						done = model + " is off. With none on, /model offers every model of " + pg.provider.GetDisplayName() + "."
					case !on:
						done = "/model no longer offers " + model + "."
					}
					return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
				})
			},
			"hub.refresh": func(*cards.List, int) tea.Cmd { return read(true) },
		},
	})
	list.ToggleDetail()
	p.views = append(p.views, list)
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
func (p *providers) usage(pg *page) tea.Cmd {
	t := p.kit.Env.Theme
	var reading *providerv1.AccountUsage
	var failed error
	loaded := false
	var list *cards.List
	name := pg.provider.GetDisplayName()
	read := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := p.service.Usage(p.kit.Env.Ctx, &providerv1.UsageRequest{Profile: p.profile, Provider: pg.provider.GetName()})
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
		Key:     "providers:" + pg.provider.GetName() + ":usage",
		Context: cards.HubContext,
		Intro: func() []string {
			lines := []string{"What " + name + " says the account in use may still spend."}
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
			lines := []string{window.GetLabel()}
			if window.GetModel() {
				lines = append(lines, "A limit on one family of models, not on the whole account.")
			}
			if window.Used != nil {
				used := min(max(window.GetUsed(), 0), 100)
				lines = append(lines, strconv.Itoa(int(100-used))+"% is left.")
			}
			if resets := window.GetResets(); resets != nil {
				lines = append(lines, resetWords(resets.AsTime())+".")
			} else if window.GetDetail() != "" {
				lines = append(lines, window.GetDetail())
			}
			if fetched := reading.GetFetched(); fetched != nil {
				lines = append(lines, render.Dim("Read at "+clock(fetched.AsTime()), t))
			}
			return lines
		},
		Banner: func() string {
			if why := reading.GetUnavailable(); why != "" {
				return render.Warm(name+" can't say what this account may spend: "+strings.TrimSuffix(why, ".")+".", t)
			}
			return ""
		},
		Empty: func() string {
			switch {
			case failed != nil:
				return unread(failed, pg.provider, "what it may spend")
			case !loaded:
				return "Asking " + name + " what the account may still spend…"
			case reading.GetUnavailable() != "":
				return ""
			}
			return name + " said nothing of the limits of this account. " + p.kit.Env.Keys.Display("hub.refresh") + " asks again."
		},
		Free:    map[string]bool{"hub.refresh": true},
		Hints:   func(*cards.List, int) []wm.Hint { return []wm.Hint{{Action: "hub.refresh", Label: "read again"}} },
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.refresh": func(*cards.List, int) tea.Cmd { return read() }},
	})
	list.ToggleDetail()
	return tea.Batch(p.hub.Push(list), read())
}
