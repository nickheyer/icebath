package app

import (
	"cmp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
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
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:    "providers",
		Title: "Providers",
		Short: "Inference providers, which have a credential and the models they serve",
		Open: func(kit *tui.Kit) tea.Cmd {
			return (&providers{kit: kit, service: service, settings: settings}).open()
		},
	}}
}

type providers struct {
	kit      *tui.Kit
	service  providerv1connect.ProviderServiceClient
	settings configv1connect.ConfigServiceClient
	hub      *cards.Hub
	rows     []*providerv1.Provider
	// Names of the providers the user removed, left out of the rows
	removed []string
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
		Status: func() string {
			if len(p.removed) == 0 {
				return ""
			}
			return render.Count(len(p.removed), "removed") + ", a brings one back"
		},
		Free: map[string]bool{"hub.refresh": true, "hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{{Action: "hub.use", Label: "models"}, {Action: "hub.toggle", Label: "details"}}
			if i >= 0 {
				hints = append(hints, wm.Hint{Action: "hub.remove"})
			}
			if len(p.removed) > 0 {
				hints = append(hints, wm.Hint{Action: "hub.add", Label: "bring back"})
			}
			return append(hints, wm.Hint{Action: "hub.refresh"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":    func(_ *cards.List, i int) tea.Cmd { return p.models(p.rows[i]) },
			"hub.toggle": func(l *cards.List, _ int) tea.Cmd { return l.ToggleDetail() },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := p.rows[i]
				l.Confirm("Remove "+entry.GetDisplayName()+" from every list?", func() tea.Cmd {
					return p.store(append(slices.Clone(p.removed), entry.GetName()), entry.GetDisplayName()+" removed")
				})
				return nil
			},
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.restore() },
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
		removed, _ := p.fetchRemoved()
		return func() tea.Cmd {
			p.rows, p.err, p.loaded, p.removed = response.GetProviders(), err, true, removed
			return nil
		}
	})
}

// Names the user removed as the settings hold them now
func (p *providers) fetchRemoved() ([]string, error) {
	response, err := p.settings.Get(p.kit.Env.Ctx, &configv1.GetRequest{Key: providerconfig.Removed})
	if err != nil {
		return nil, err
	}
	var names []string
	if text := response.GetSetting().GetValue(); text != "" {
		if err := yaml.Unmarshal([]byte(text), &names); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// Saves the names as the removed providers, then reads the rows again
func (p *providers) store(names []string, done string) tea.Cmd {
	slices.Sort(names)
	names = slices.Compact(names)
	return tui.Async(func() func() tea.Cmd {
		text, err := yaml.Marshal(names)
		if err == nil {
			_, err = p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Key: providerconfig.Removed, Value: string(text)})
		}
		return func() tea.Cmd {
			if err != nil {
				return p.hub.Fail(err.Error())
			}
			return tea.Batch(p.hub.Done(done), p.load())
		}
	})
}

// Lists the removed providers to bring one back
func (p *providers) restore() tea.Cmd {
	if len(p.removed) == 0 {
		return p.hub.Done("No provider is removed")
	}
	names := slices.Clone(p.removed)
	page := cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Removed providers",
		Key:     "providers:removed",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Provider", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(names))
			for i, name := range names {
				rows[i] = []string{name}
			}
			return rows
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "bring back"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				name := names[i]
				kept := slices.DeleteFunc(slices.Clone(p.removed), func(n string) bool { return n == name })
				return tea.Batch(p.hub.Pop(), p.store(kept, name+" is back"))
			},
		},
	})
	return p.hub.Push(page)
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
