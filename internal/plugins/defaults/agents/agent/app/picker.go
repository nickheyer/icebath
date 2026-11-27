package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type offer struct {
	provider *providerv1.Provider
	model    string
	// What is known of the model, nil when nothing is
	detail *providerv1.Model
}

type picker struct {
	chat    *chat
	list    *cards.List
	rows    []offer
	failed  []string
	pending int
	loaded  bool
	err     error
	// The pick is saved for every session rather than this one alone
	global bool
}

const pickerContext = "agent.model"

var pickerKeys = []keys.Action{
	{ID: "agent.model.scope", Context: pickerContext, Label: "scope", Doc: "save the pick for every session or for this session only", Default: []string{"ctrl+g"}},
	{ID: "agent.model.logins", Context: pickerContext, Label: "logins", Doc: "open the logins of the provider of the highlighted row", Default: []string{"ctrl+d"}},
}

func (c *chat) pick() tea.Cmd {
	p := &picker{chat: c}
	kit := c.kit
	var hub *cards.Hub
	p.list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Model",
		Glyph:   kit.Env.Theme.Glyphs.Models,
		Key:     "model",
		Context: cards.HubContext,
		Shared:  []string{pickerContext},
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search models",
		Columns: []render.Column{{}, {Title: "Model", Flex: 1}, {Title: "Context"}, {Title: "Provider"}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, row := range p.rows {
				mark := ""
				if p.current(row) {
					mark = kit.Env.Theme.Glyphs.Live
				}
				model := row.model
				if note := row.detail.GetNote(); note != "" {
					model += " " + render.Dim(note, kit.Env.Theme)
				}
				rows[i] = []string{mark, model, render.Dim(models.Tokens(row.detail.GetContext()), kit.Env.Theme), row.provider.GetDisplayName()}
			}
			return rows
		},
		Banner: func() string {
			if len(p.failed) == 0 {
				return ""
			}
			return "Couldn't list models for " + strings.Join(p.failed, ", ")
		},
		Empty: func() string {
			return tui.Waiting(p.loaded && p.pending == 0, p.err, "No provider of this profile is connected")
		},
		Status: p.scope,
		Free:   map[string]bool{"hub.add": true, "agent.model.scope": true, "agent.model.logins": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add", Label: "providers"}, {Action: "agent.model.scope"}, {Action: "agent.model.logins"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.add", Label: "providers"}, {Action: "agent.model.scope"},
				{Action: "agent.model.logins"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				row := p.rows[i]
				// A model that takes no effort is switched to at once
				if reasoning := row.detail.GetReasoning(); row.detail != nil && row.detail.Reasoning != nil && !reasoning {
					return tea.Batch(kit.WM.DismissCard(hub), c.send(p.command(row, "")))
				}
				return hub.Push(p.efforts(hub, row))
			},
			"hub.add":           func(*cards.List, int) tea.Cmd { return kit.Open(providerapp.Screen, profile.Join(c.on(), "")) },
			"agent.model.scope": func(*cards.List, int) tea.Cmd { p.global = !p.global; return nil },
			"agent.model.logins": func(_ *cards.List, i int) tea.Cmd {
				// The logins of the highlighted row's provider, else of the provider the chat runs on
				name := c.log.state.GetProvider()
				if i >= 0 && i < len(p.rows) {
					name = p.rows[i].provider.GetName()
				}
				if name == "" {
					return kit.Open(providerapp.Screen, profile.Join(c.on(), ""))
				}
				return kit.Open(providerapp.Screen, profile.Join(c.on(), providerapp.LoginsOf+name))
			},
		},
	})
	hub = kit.Hub("model", p.list)
	return tea.Batch(kit.WM.ShowOverlay(hub), tui.Async(func() func() tea.Cmd {
		response, err := c.desk.providers.List(kit.Env.Ctx, &providerv1.ListRequest{Profile: c.on()})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			var cmds []tea.Cmd
			for _, provider := range response.GetProviders() {
				if provider.GetUsable() && !provider.GetHidden() && (provider.GetCredentialOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED || provider.GetLocal()) {
					p.pending++
					cmds = append(cmds, p.load(provider))
				}
			}
			return tea.Batch(cmds...)
		}
	}))
}

// Where the pick is saved, as the frame says
func (p *picker) scope() string {
	if p.global {
		return "persist: global"
	}
	return "persist: session"
}

// The /model command of the pick: the model, its provider, the effort when one was picked, and the scope
func (p *picker) command(row offer, effort string) string {
	command := "/model " + row.model + " --provider " + row.provider.GetName()
	if effort != "" {
		command += " --reasoning " + effort
	}
	if p.global {
		return command + " --global"
	}
	return command + " --session"
}

// Second step of a pick: how hard the model is told to think, reasoning off, or the effort left as it is; going back returns to the models
func (p *picker) efforts(hub *cards.Hub, row offer) *cards.List {
	kit := p.chat.kit
	type level struct{ title, value string }
	// The efforts the model takes, as its kind or its provider's listing names them, else every effort a wire knows; none is offered only
	// for a model whose reasoning can be turned off
	offered := row.detail.GetEfforts()
	if len(offered) == 0 {
		offered = provider.Efforts
	}
	var levels []level
	for _, effort := range offered {
		if effort != "none" {
			levels = append(levels, level{effort, effort})
		}
	}
	if !row.detail.GetMandatoryReasoning() {
		levels = append(levels, level{"none (disable reasoning)", "none"})
	}
	levels = append(levels, level{"Keep current effort", ""})
	return cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Reasoning effort",
		Glyph:   kit.Env.Theme.Glyphs.Models,
		Key:     "model:effort",
		Context: cards.HubContext,
		Shared:  []string{pickerContext},
		Primary: "hub.use",
		Pick:    true,
		Columns: []render.Column{{Title: "Effort", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(levels))
			for i, l := range levels {
				rows[i] = []string{l.title}
			}
			return rows
		},
		Banner: func() string {
			return row.model + " · applies with the switch (same scope) · " + kit.WM.Keys().Display("hub.back") + " back"
		},
		Status: p.scope,
		Free:   map[string]bool{"agent.model.scope": true},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.back"}, {Action: "agent.model.scope"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(kit.WM.DismissCard(hub), p.chat.send(p.command(row, levels[i].value)))
			},
			"agent.model.scope": func(*cards.List, int) tea.Cmd { p.global = !p.global; return nil },
		},
	})
}

func (p *picker) current(row offer) bool {
	state := p.chat.log.state
	return state.GetProvider() == row.provider.GetName() && state.GetModel() == row.model
}

// Adds the models of the provider: those picked for it, else every one it lists
func (p *picker) load(provider *providerv1.Provider) tea.Cmd {
	kit := p.chat.kit
	return tui.Async(func() func() tea.Cmd {
		models := provider.GetModels()
		details := map[string]*providerv1.Model{}
		response, err := p.chat.desk.providers.Models(kit.Env.Ctx, &providerv1.ModelsRequest{Provider: provider.GetName(), Profile: p.chat.on()})
		for _, detail := range response.GetDetails() {
			details[detail.GetId()] = detail
		}
		if len(models) == 0 {
			models = response.GetModels()
		} else {
			err = nil
		}
		return func() tea.Cmd {
			p.pending--
			if err != nil && !provider.GetLocal() {
				p.failed = append(p.failed, provider.GetDisplayName())
			}
			if provider.GetDefaultModel() != "" && !slices.Contains(models, provider.GetDefaultModel()) && (err == nil || !provider.GetLocal()) {
				models = append(models, provider.GetDefaultModel())
			}
			for _, model := range models {
				p.rows = append(p.rows, offer{provider: provider, model: model, detail: details[model]})
			}
			slices.SortStableFunc(p.rows, func(a, b offer) int {
				return cmp.Or(strings.Compare(a.provider.GetDisplayName(), b.provider.GetDisplayName()), strings.Compare(a.model, b.model))
			})
			if !p.list.Filtered() {
				p.list.Refresh()
				for i, row := range p.rows {
					if p.current(row) {
						p.list.Select(i)
					}
				}
			}
			return nil
		}
	})
}
