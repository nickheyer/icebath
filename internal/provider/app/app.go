package app

import (
	"cmp"
	"slices"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/profile"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the providers of the profile, the page of the Providers area
const Screen = "providers"

// Screen of the model list, the page of the Model list area
const ModelList = "models"

// Start of the argument of the providers screen that opens the page of the provider named after it, such as logins:anthropic
const LoginsOf = "logins:"

// Start of the argument of the providers screen that opens the page of an account, the provider then the account after it, such as
// account:anthropic:work
const AccountOf = "account:"

// Start of the argument of the providers screen that starts adding a provider of the kind named after it, such as add:openrouter
const AddOf = "add:"

// Start of the argument of the model list that opens the limits of one model, the provider then the model after it, such as
// limit:openrouter:anthropic/claude-sonnet-4
const LimitOf = "limit:"

// What the Providers area is, the first line of its page
const intro = "A provider runs the models the agent works with."

type screens struct {
	connectors *Connectors
	// Names providers are shown by, by profile and then by their names there, read with every listing
	names *names
}

func Register(host *app.App, connectors *Connectors) {
	host.Register(screens{connectors: connectors, names: &names{shown: map[string]map[string]string{}}})
}

// Names providers are shown by, kept from the listings so a row that names one can say it at once
type names struct {
	mu    sync.Mutex
	shown map[string]map[string]string
}

// Keeps the names of the providers of a listing of the profile
func (n *names) keep(profile string, providers []*providerv1.Provider) {
	kept := map[string]string{}
	for _, entry := range providers {
		kept[entry.GetName()] = entry.GetDisplayName()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.shown[profile] = kept
}

// Name the provider is shown by, its own name while no listing named it, and whether a listing of the profile was kept
func (n *names) of(profile, id string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	kept, ok := n.shown[profile]
	return cmp.Or(kept[id], id), ok
}

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	service := providerv1connect.NewProviderServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	read := func(kit *tui.Kit, profile string) tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := service.List(kit.Env.Ctx, &providerv1.ListRequest{Profile: profile})
			if err == nil {
				s.names.keep(profile, response.GetProviders())
			}
			return func() tea.Cmd { return nil }
		})
	}
	return []tui.Screen{{
		ID:      Screen,
		Group:   tui.GroupModels,
		Order:   20,
		Title:   "Providers",
		Short:   "Add providers and connect your accounts",
		Slash:   []string{"providers"},
		Aliases: []string{"connect", "logins"},
		Usage:   "[@profile] [words to search]",
		Actions: slices.Concat(signinKeys, pickerKeys, loginKeys),
		Where:   map[string]string{signinContext: "provider sign-in", pickerContext: "model list", loginsContext: "accounts of a provider"},
		Area:    providerconfig.Key,
		Pickers: pickers(service, s.names, read),
		Pages:   func(kit *tui.Kit, name string) []tui.Page { return providerPages(kit, service, name) },
		Start:   func(kit *tui.Kit) tea.Cmd { return read(kit, "") },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, search := profile.Split(argument)
			p := &providers{kit: kit, service: service, settings: settings, connectors: s.connectors.Connectors(conn), profile: name, names: s.names}
			// The page of one provider opens straight away, as the question of an account that ran out asks; so does the page of one of its
			// accounts, or adding a provider of one kind
			search = strings.TrimSpace(search)
			if id, ok := strings.CutPrefix(search, LoginsOf); ok {
				p.focus, search = strings.TrimSpace(id), ""
			}
			if names, ok := strings.CutPrefix(search, AccountOf); ok {
				id, account, _ := strings.Cut(names, ":")
				p.focus, p.account, search = strings.TrimSpace(id), strings.TrimSpace(account), ""
			}
			if kind, ok := strings.CutPrefix(search, AddOf); ok {
				p.adding, search = strings.TrimSpace(kind), ""
			}
			return p.open(search)
		},
	}, {
		ID:    ModelList,
		Group: tui.GroupModels,
		Order: 40,
		Title: "Model list",
		Short: "Set where models come from and their limits",
		Area:  modelsconfig.Key,
		Pages: func(kit *tui.Kit, name string) []tui.Page { return limitPages(kit, service, name) },
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, rest := profile.Split(argument)
			m := &modelList{kit: kit, service: service, settings: settings, profile: name, names: s.names}
			// The limits of one model open over the page, the provider then the model after LimitOf
			if names, ok := strings.CutPrefix(strings.TrimSpace(rest), LimitOf); ok {
				m.focus, m.model, _ = strings.Cut(names, ":")
			}
			return m.open()
		},
	}}
}

type providers struct {
	kit        *tui.Kit
	service    providerv1connect.ProviderServiceClient
	settings   configv1connect.ConfigServiceClient
	connectors []Connector
	names      *names
	// Profile the providers are of, the one in use when empty
	profile string
	// Provider whose page opens once the providers are read, empty for none, and the account of it whose page opens over it
	focus, account string
	// Kind of provider whose adding starts once the providers are read, empty for none
	adding string
	hub    *cards.Hub
	list   *cards.List
	rows   []*providerv1.Provider
	// Option rows of the settings, as Settings draws and changes them
	options *configapp.Rows
	// Lists over the first one, refreshed whenever the providers are read
	views  cards.Watched
	loaded bool
	err    error
	// Whether the first reading was handled, which opens the kinds to add on a profile with no provider
	settled bool
}

// Row of the first view: a provider of the profile, or the row that adds one when provider is nil
type root struct {
	provider *providerv1.Provider
}

// Rows of the first view: the providers, then the row that adds one. None before the providers were read or when no reading of them
// succeeded, so the page says that it is reading or why it could not; what was read before stays listed when a later reading fails
func (p *providers) roots() []root {
	if !p.loaded || p.err != nil && len(p.rows) == 0 {
		return nil
	}
	rows := make([]root, 0, len(p.rows)+1)
	for _, entry := range p.rows {
		rows = append(rows, root{provider: entry})
	}
	return append(rows, root{})
}

// Page of the area: what a provider is, a warm line while none is ready, the providers under the headings of their state, and the row
// that adds one, always last
func (p *providers) open(search string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []root
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Providers",
		Glyph:   t.Glyphs.Models,
		Key:     "providers",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search providers",
		Intro: func() []string {
			lines := []string{intro}
			if warm := p.warning(); warm != "" {
				lines = append(lines, render.Warm(warm, t))
			}
			return lines
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = p.roots()
			cells := make([][]string, len(shown))
			for i, row := range shown {
				if row.provider == nil {
					cells[i] = []string{"Add a provider", render.Dim(t.Glyphs.Forward, t), ""}
					continue
				}
				cells[i] = []string{row.provider.GetDisplayName(), "", p.stateWords(row.provider)}
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].provider == nil {
				return ""
			}
			return heading(rank(shown[i].provider))
		},
		Words: func(i int) string {
			if entry := shown[i].provider; entry != nil {
				return entry.GetDescription() + " " + entry.GetName() + " " + strings.Join(entry.GetAliases(), " ")
			}
			return "add new connect"
		},
		Detail: func(i int) []string {
			if shown[i].provider == nil {
				return []string{"A company that runs models, such as Anthropic or OpenAI, or a model server of your own."}
			}
			return p.describe(shown[i].provider)
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, "") },
		Free:  map[string]bool{"hub.refresh": true, "hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0 || i >= len(shown):
				return []wm.Hint{{Action: "hub.add", Label: "add"}}
			case shown[i].provider == nil:
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.remove", Label: "remove"}, {Action: "hub.add", Label: "add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if shown[i].provider == nil {
					return p.add()
				}
				return p.show(shown[i].provider.GetName())
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if entry := shown[i].provider; entry != nil {
					return p.remove(l, entry)
				}
				return nil
			},
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load("") },
		},
	})
	p.list.ToggleDetail()
	hub, cmd := p.kit.Show(Screen, p.list)
	p.hub = hub
	p.options = configapp.NewRows(p.kit, hub, p.settings, p.profile)
	if search != "" {
		p.list.Filter(search)
	}
	return tea.Batch(cmd, p.load(""), p.options.Load(nil))
}

// Line in the warm color while the profile has providers and none of them is ready, saying what to do
func (p *providers) warning() string {
	if !p.loaded || len(p.rows) == 0 || slices.ContainsFunc(p.rows, Ready) {
		return ""
	}
	return "None of your providers is ready yet. Open one to see what it needs."
}

// Asks before a provider is removed, naming what goes with it, then removes it and says what was cleared
func (p *providers) remove(l *cards.List, entry *providerv1.Provider) tea.Cmd {
	text := "Remove " + entry.GetDisplayName() + "? Every setting and session that names it is cleared."
	if n := len(entry.GetAccounts()); n > 0 {
		text = "Remove " + entry.GetDisplayName() + " and its " + render.Count(n, "account") + "? Every setting and session that names it is cleared."
	}
	l.Confirm(cards.Question{Text: text, Verb: "Remove", Deletes: true}, func() tea.Cmd {
		return tui.Call(p.hub, func() (*providerv1.RemoveResponse, error) {
			return p.service.Remove(p.kit.Env.Ctx, &providerv1.RemoveRequest{Profile: p.profile, Provider: entry.GetName()})
		}, func(removed *providerv1.RemoveResponse) tea.Cmd {
			return tea.Batch(p.hub.PopTo(p.list), p.hub.Done(removed.GetSaid()), p.load(""), p.options.Load(nil))
		})
	})
	return nil
}

// Reads the providers again, those ready to use first, then those not connected, then those that can't be used, each by name. The one
// named selected is in hand, the first provider when none is. A profile with no provider opens the kinds to add on the first reading
func (p *providers) load(selected string) tea.Cmd { return p.reload(selected, nil) }

// Reads the providers as load does, then runs then once they were read. A reading that fails says why and leaves what was read before
// listed
func (p *providers) reload(selected string, then func() tea.Cmd) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &providerv1.ListRequest{Profile: p.profile})
		rows := response.GetProviders()
		if err == nil {
			p.names.keep(p.profile, rows)
		}
		slices.SortStableFunc(rows, func(a, b *providerv1.Provider) int {
			return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(strings.ToLower(a.GetDisplayName()), strings.ToLower(b.GetDisplayName())))
		})
		return func() tea.Cmd {
			first := !p.settled
			p.settled = true
			p.err, p.loaded = err, true
			if err == nil {
				p.rows = rows
			}
			p.list.Refresh()
			var opened tea.Cmd
			placed := false
			for i, entry := range p.rows {
				if entry.GetName() == selected && !p.list.Filtered() {
					placed = p.list.Select(i)
				}
				if p.focus != "" && entry.GetName() == p.focus {
					placed = p.list.Select(i)
					opened = p.show(entry.GetName())
					if p.account != "" {
						opened = tea.Batch(opened, p.accountPage(entry.GetName(), p.account))
					}
				}
			}
			p.views.Refresh()
			switch {
			case err != nil:
				// The first view with nothing to list says why in place of its rows; anywhere else the reason goes under the view
				if p.hub.Top() != p.list || len(p.rows) > 0 {
					opened = p.hub.Fail(tui.Reason(err))
				}
			case p.focus != "" && opened == nil:
				opened = p.hub.Fail("This profile has no provider named " + p.focus + ".")
			case p.adding != "":
				opened = p.addOf(p.adding)
			case first && len(p.rows) == 0 && !p.list.Filtered():
				opened = p.add()
			case first && !placed && len(p.rows) > 0 && !p.list.Filtered():
				p.list.Select(0)
			}
			p.focus, p.account, p.adding = "", "", ""
			if then != nil && err == nil {
				return tea.Batch(opened, then())
			}
			return opened
		}
	})
}

// Provider of the profile with the name as last read, nil when it is gone
func (p *providers) find(name string) *providerv1.Provider {
	for _, entry := range p.rows {
		if entry.GetName() == name {
			return entry
		}
	}
	return nil
}

// Whether the agent can use the provider now, by the one test the server makes for this page, the model picker and the agent alike
func Ready(entry *providerv1.Provider) bool {
	return entry.GetReady()
}

// Place of a provider in the list: ready to use, then not connected, then those that can't be used
func rank(entry *providerv1.Provider) int {
	switch {
	case Ready(entry):
		return 0
	case !entry.GetUsable():
		return 2
	}
	return 1
}

// Heading of the providers of a rank
func heading(rank int) string {
	switch rank {
	case 0:
		return "Ready to use"
	case 2:
		return "Can't be used yet"
	}
	return "Not connected"
}

// State of a provider as its row says it, by the one function of the server, warm while it is not ready
func (p *providers) stateWords(entry *providerv1.Provider) string {
	if Ready(entry) {
		return render.Good(entry.GetState(), p.kit.Env.Theme)
	}
	return render.Warm(entry.GetState(), p.kit.Env.Theme)
}

// What the provider is and the sentence of its state, the facts a person acts on, and what its account may still spend; nothing the row
// says already
func (p *providers) describe(entry *providerv1.Provider) []string {
	t := p.kit.Env.Theme
	var lines []string
	if about := entry.GetDescription(); about != "" {
		lines = append(lines, about)
	}
	if standing := entry.GetStanding(); standing != "" {
		lines = append(lines, standing)
	}
	var facts []string
	if model := entry.GetDefaultModel(); model != "" {
		facts = append(facts, "Default model: "+model)
	}
	if base := entry.GetBaseUrl(); base != "" && entry.GetOwn() {
		facts = append(facts, "Address: "+base)
	}
	if entry.GetHidden() {
		facts = append(facts, "Left out of the model list")
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	if said := glance(entry.GetAccountUsage()); said != "" {
		lines = append(lines, render.Dim("Account use: "+said, t))
	}
	return lines
}

// Format of requests of a provider in words, as the Request format setting names it
func modeWords(mode string) string {
	for _, option := range providerconfig.Modes() {
		if option.Value == mode && option.Value != "" {
			return option.Title
		}
	}
	return strings.ReplaceAll(mode, "_", " ")
}
