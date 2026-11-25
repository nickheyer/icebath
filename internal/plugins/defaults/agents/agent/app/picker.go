package app

import (
	tea "charm.land/bubbletea/v2"

	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type picker struct {
	kit       *tui.Kit
	providers providerv1connect.ProviderServiceClient
	chat      *chat
	hub       *cards.Hub
	rows      []*providerv1.Provider
	loaded    bool
	err       error
}

func (p *picker) open() tea.Cmd {
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Model",
		Key:     "model",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Provider"}, {Title: "Credential"}, {Title: "Default model", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, entry := range p.rows {
				rows[i] = []string{entry.GetDisplayName(), entry.GetCredentialSource(), entry.GetDefaultModel()}
			}
			return rows
		},
		Empty: func() string {
			switch {
			case p.err != nil:
				return p.err.Error()
			case !p.loaded:
				return "Loading…"
			}
			return "No provider has a credential yet"
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "models"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return p.models(p.rows[i]) },
		},
	})
	p.hub = p.kit.Hub("model", list)
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), tui.Async(func() func() tea.Cmd {
		response, err := p.providers.List(p.kit.Env.Ctx, &providerv1.ListRequest{})
		return func() tea.Cmd {
			p.err, p.loaded = err, true
			for _, entry := range response.GetProviders() {
				if entry.GetCredentialSource() != "" || entry.GetLocal() {
					p.rows = append(p.rows, entry)
				}
			}
			return nil
		}
	}))
}

func (p *picker) models(entry *providerv1.Provider) tea.Cmd {
	var ids []string
	note := "Loading…"
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   entry.GetDisplayName(),
		Key:     "model:" + entry.GetName(),
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Columns: []render.Column{{Title: "Model", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(ids))
			for i, id := range ids {
				rows[i] = []string{id}
			}
			return rows
		},
		Empty: func() string { return note },
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "use"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(p.kit.WM.DismissCard(p.hub), p.chat.send("/model "+ids[i]+" --provider "+entry.GetName()))
			},
		},
	})
	return tea.Batch(p.hub.Push(list), tui.Async(func() func() tea.Cmd {
		response, err := p.providers.Models(p.kit.Env.Ctx, &providerv1.ModelsRequest{Provider: entry.GetName()})
		return func() tea.Cmd {
			switch ids = response.GetModels(); {
			case err != nil:
				note = err.Error()
			case len(ids) == 0:
				note = "No models listed"
			}
			return nil
		}
	}))
}
