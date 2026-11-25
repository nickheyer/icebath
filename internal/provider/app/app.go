package app

import (
	"cmp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := providerv1connect.NewProviderServiceClient(conn)
	return []tui.Screen{{
		ID:    "providers",
		Title: "Providers",
		Short: "Inference providers, which have a credential and the models they serve",
		Open: func(kit *tui.Kit) tea.Cmd {
			return (&providers{kit: kit, service: service}).open()
		},
	}}
}

type providers struct {
	kit     *tui.Kit
	service providerv1connect.ProviderServiceClient
	hub     *cards.Hub
	rows    []*providerv1.Provider
	loaded  bool
	err     error
}

func (p *providers) open() tea.Cmd {
	list := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Providers",
		Key:     "providers",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Provider"}, {Title: "Name"}, {Title: "Credential"}, {Title: "Format"}, {Title: "Endpoint", Flex: 1}},
		Yield:   []int{3, 4, 1},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, entry := range p.rows {
				rows[i] = []string{entry.GetDisplayName(), entry.GetName(), credential(entry), entry.GetApiMode(), entry.GetBaseUrl()}
			}
			return rows
		},
		Detail: func(i int) []string { return describe(p.rows[i]) },
		Empty: func() string {
			switch {
			case p.err != nil:
				return p.err.Error()
			case !p.loaded:
				return "Loading…"
			}
			return "No providers"
		},
		Free: map[string]bool{"hub.refresh": true},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "models"}, {Action: "hub.toggle", Label: "details"}, {Action: "hub.refresh"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return p.models(p.rows[i]) },
			"hub.toggle":  func(l *cards.List, _ int) tea.Cmd { return l.ToggleDetail() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load() },
		},
	})
	list.ToggleDetail()
	p.hub = p.kit.Hub("providers", list)
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load())
}

func (p *providers) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &providerv1.ListRequest{})
		return func() tea.Cmd {
			p.rows, p.err, p.loaded = response.GetProviders(), err, true
			return nil
		}
	})
}

func (p *providers) models(entry *providerv1.Provider) tea.Cmd {
	var ids []string
	note := "Loading…"
	page := cards.NewPage(p.kit.Env, cards.PageSpec{
		Title: entry.GetDisplayName(),
		Key:   "providers:" + entry.GetName(),
		Lines: func(int) []string {
			lines := append(describe(entry), "")
			if note != "" {
				return append(lines, render.Dim(note, p.kit.Env.Theme))
			}
			return append(append(lines, render.Heading("Models", p.kit.Env.Theme)), ids...)
		},
	})
	return tea.Batch(p.hub.Push(page), tui.Async(func() func() tea.Cmd {
		response, err := p.service.Models(p.kit.Env.Ctx, &providerv1.ModelsRequest{Provider: entry.GetName()})
		return func() tea.Cmd {
			switch ids = response.GetModels(); {
			case err != nil:
				note = err.Error()
			case len(ids) == 0:
				note = "No models listed"
			default:
				note = ""
			}
			return nil
		}
	}))
}

func credential(entry *providerv1.Provider) string {
	if entry.GetLocal() && entry.GetCredentialSource() == "" {
		return "not needed"
	}
	return cmp.Or(entry.GetCredentialSource(), "-")
}

func describe(entry *providerv1.Provider) []string {
	lines := []string{}
	if entry.GetDescription() != "" {
		lines = append(lines, entry.GetDescription(), "")
	}
	lines = append(lines, "name: "+entry.GetName())
	if len(entry.GetAliases()) > 0 {
		lines = append(lines, "aliases: "+strings.Join(entry.GetAliases(), ", "))
	}
	if entry.GetBaseUrl() != "" {
		lines = append(lines, "endpoint: "+entry.GetBaseUrl())
	}
	lines = append(lines, "request format: "+entry.GetApiMode())
	if entry.GetDefaultModel() != "" {
		lines = append(lines, "default model: "+entry.GetDefaultModel())
	}
	switch {
	case entry.GetCredentialSource() != "":
		lines = append(lines, "credential: "+entry.GetCredentialSource())
	case entry.GetLocal():
		lines = append(lines, "credential: none needed")
	default:
		line := "credential: none"
		if len(entry.GetKeyEnv()) > 0 {
			line += ", set " + strings.Join(entry.GetKeyEnv(), " or ")
		}
		if entry.GetSignupUrl() != "" {
			line += ", get one at " + entry.GetSignupUrl()
		}
		lines = append(lines, line)
	}
	return lines
}
