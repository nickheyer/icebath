package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type offer struct {
	provider *providerv1.Provider
	model    string
}

type picker struct {
	chat    *chat
	list    *cards.List
	rows    []offer
	failed  []string
	pending int
	loaded  bool
	err     error
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
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search models",
		Columns: []render.Column{{}, {Title: "Model", Flex: 1}, {Title: "Provider"}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, row := range p.rows {
				mark := ""
				if p.current(row) {
					mark = kit.Env.Theme.Glyphs.Live
				}
				rows[i] = []string{mark, row.model, row.provider.GetDisplayName()}
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
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return []wm.Hint{{Action: "hub.add", Label: "providers"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "use"}, {Action: "hub.add", Label: "providers"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				row := p.rows[i]
				return tea.Batch(kit.WM.DismissCard(hub), c.send("/model "+row.model+" --provider "+row.provider.GetName()))
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return kit.Open(providerapp.Screen, "@"+c.on()) },
		},
	})
	hub = kit.Hub("model", p.list)
	return tea.Batch(kit.WM.ShowOverlay(hub), tui.Async(func() func() tea.Cmd {
		response, err := c.desk.providers.List(kit.Env.Ctx, &providerv1.ListRequest{Profile: c.on()})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			var cmds []tea.Cmd
			for _, provider := range response.GetProviders() {
				if provider.GetUsable() && (provider.GetCredentialOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED || provider.GetLocal()) {
					p.pending++
					cmds = append(cmds, p.load(provider))
				}
			}
			return tea.Batch(cmds...)
		}
	}))
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
		var err error
		if len(models) == 0 {
			var response *providerv1.ModelsResponse
			response, err = p.chat.desk.providers.Models(kit.Env.Ctx, &providerv1.ModelsRequest{Provider: provider.GetName(), Profile: p.chat.on()})
			models = response.GetModels()
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
				p.rows = append(p.rows, offer{provider: provider, model: model})
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
