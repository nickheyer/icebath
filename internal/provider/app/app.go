package app

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/profile"
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

// Screen of the providers of the profile
const Screen = "providers"

// Start of the argument of the providers screen that opens the logins of the provider named after it, such as logins:anthropic
const LoginsOf = "logins:"

// What the first screen says it is for, to someone who has never connected a provider
const intro = "A provider runs the models the agent works with. Connect your account at one and you can start chatting."

// Name and title of the first account of a provider, which connecting it makes without asking
const (
	firstName  = "main"
	firstTitle = "Main"
)

type screens struct {
	connectors *Connectors
}

func Register(host *app.App, connectors *Connectors) {
	host.Register(screens{connectors: connectors})
}

func (s screens) Screens(conn *connect.Client) []tui.Screen {
	service := providerv1connect.NewProviderServiceClient(conn)
	settings := configv1connect.NewConfigServiceClient(conn)
	return []tui.Screen{{
		ID:      Screen,
		Group:   tui.GroupModels,
		Order:   20,
		Title:   "Providers",
		Short:   "Add providers and connect your accounts",
		Slash:   []string{"providers", "connect", "logins"},
		Usage:   "[@profile] [words to search]",
		Actions: append(slices.Clone(signinKeys), loginKeys...),
		Where:   map[string]string{signinContext: "provider sign-in", loginsContext: "accounts of a provider"},
		Open: func(kit *tui.Kit, argument string) tea.Cmd {
			name, search := profile.Split(argument)
			p := &providers{kit: kit, service: service, settings: settings, connectors: s.connectors.Connectors(conn), profile: name}
			// The logins of one provider open straight away, as the model picker and the prompt of a login that ran out ask
			if id, ok := strings.CutPrefix(strings.TrimSpace(search), LoginsOf); ok {
				p.focus, search = strings.TrimSpace(id), ""
			}
			return p.open(search)
		},
	}}
}

type providers struct {
	kit        *tui.Kit
	service    providerv1connect.ProviderServiceClient
	settings   configv1connect.ConfigServiceClient
	connectors []Connector
	// Profile the providers are of, the one in use when empty
	profile string
	// Provider whose logins open once the providers are read, empty for none
	focus string
	hub   *cards.Hub
	list  *cards.List
	rows  []*providerv1.Provider
	// Page of one provider while it is open, read again with the list
	page *page
	// Lists stacked over the first one, refreshed whenever the providers are read
	views  []*cards.List
	loaded bool
	err    error
	// Whether the first reading was handled, which opens the kinds to add on a profile with no provider
	settled bool
}

// Row of the first screen: a provider of the profile, or the row that adds one when provider is nil
type root struct {
	provider *providerv1.Provider
}

// Rows of the first screen: the row that adds a provider, then the providers, once they were read
func (p *providers) roots() []root {
	if !p.loaded || p.err != nil {
		return nil
	}
	rows := []root{{}}
	for _, entry := range p.rows {
		rows = append(rows, root{provider: entry})
	}
	return rows
}

// Providers screen: what a provider is, the row that adds one, then the providers of the profile under the headings of their state, the
// highlighted one explained under the list
func (p *providers) open(search string) tea.Cmd {
	t := p.kit.Env.Theme
	var shown []root
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title(p.profile),
		Glyph:   t.Glyphs.Models,
		Key:     "providers",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search providers",
		Intro:   func() []string { return []string{intro} },
		Columns: []render.Column{{}, {}, {Flex: 1}},
		Rows: func() [][]string {
			shown = p.roots()
			cells := make([][]string, len(shown))
			for i, row := range shown {
				if row.provider == nil {
					cells[i] = []string{"Add a provider", render.Dim(t.Glyphs.Forward, t), render.Dim("Anthropic, OpenAI, a server on this computer and more", t)}
					continue
				}
				cells[i] = []string{row.provider.GetDisplayName(), "", p.status(row.provider)}
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
			return "new connect"
		},
		Detail: func(i int) []string {
			if shown[i].provider == nil {
				return []string{"Connect an account at a company that runs models, such as Anthropic or OpenAI, or a model server on this computer."}
			}
			return p.describe(shown[i].provider)
		},
		Empty: func() string {
			switch {
			case p.err != nil:
				return "The providers could not be read: " + strings.TrimSuffix(tui.Reason(p.err), ".") + ". " + p.kit.Env.Keys.Display("hub.refresh") +
					" tries again."
			case !p.loaded:
				return "Reading the providers of this profile…"
			}
			return ""
		},
		Free: map[string]bool{"hub.refresh": true, "hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0 || i >= len(shown):
				return []wm.Hint{{Action: "hub.add"}}
			case shown[i].provider == nil:
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}, {Action: "hub.edit", Label: "edit settings"}, {Action: "hub.remove"}, {Action: "hub.add"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if shown[i].provider == nil {
					return p.add()
				}
				return p.show(shown[i].provider)
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if shown[i].provider == nil {
					return nil
				}
				return p.configure(shown[i].provider)
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := shown[i].provider
				if entry == nil {
					return nil
				}
				l.Confirm(removal(entry), func() tea.Cmd {
					return tui.Call(p.hub, func() (*providerv1.RemoveResponse, error) {
						return p.service.Remove(p.kit.Env.Ctx, &providerv1.RemoveRequest{Profile: p.profile, Provider: entry.GetName()})
					}, func(*providerv1.RemoveResponse) tea.Cmd {
						return tea.Batch(p.hub.Done("Removed "+entry.GetDisplayName()+"."), p.load(""))
					})
				})
				return nil
			},
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.add() },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load("") },
		},
	})
	p.list.ToggleDetail()
	p.hub = p.kit.Hub(Screen, p.list)
	if search != "" {
		p.list.Filter(search)
	}
	return tea.Batch(p.kit.WM.ShowOverlay(p.hub), p.load(""))
}

// Question before a provider is removed, naming what goes with it
func removal(entry *providerv1.Provider) string {
	if n := len(entry.GetAccounts()); n > 0 {
		return "Remove " + entry.GetDisplayName() + " and its " + render.Count(n, "account") + "? The agent can no longer use its models."
	}
	return "Remove " + entry.GetDisplayName() + "? The agent can no longer use its models."
}

// Reads the providers again, those ready to use first, then those not connected, then those that can't be used, each by name. The one
// named selected is highlighted, the first provider when none is. A profile with no provider opens the kinds to add on the first reading
func (p *providers) load(selected string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &providerv1.ListRequest{Profile: p.profile})
		rows := response.GetProviders()
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
				// The first row of the list adds a provider, so a provider's row is one past its place
				if entry.GetName() == selected && !p.list.Filtered() {
					placed = p.list.Select(i + 1)
				}
				if p.page != nil && entry.GetName() == p.page.provider.GetName() {
					p.page.provider = entry
				}
				if p.focus != "" && entry.GetName() == p.focus {
					placed = p.list.Select(i + 1)
					opened = p.show(entry)
				}
			}
			for _, view := range p.views {
				view.Refresh()
			}
			switch {
			case p.focus != "" && opened == nil && err == nil:
				opened = p.hub.Fail("This profile has no provider named " + p.focus + ".")
			case first && err == nil && len(p.rows) == 0 && !p.list.Filtered():
				opened = p.add()
			case first && !placed && len(p.rows) > 0 && !p.list.Filtered():
				p.list.Select(1)
			}
			p.focus = ""
			return opened
		}
	})
}

// Whether the agent can use the provider with what it has now
func connected(origin providerv1.Origin) bool {
	return origin != providerv1.Origin_ORIGIN_UNSPECIFIED
}

// Place of a provider in the list: ready to use, then not connected, local ones first, then those that can't be used
func rank(entry *providerv1.Provider) int {
	switch {
	case !entry.GetUsable():
		return 3
	case connected(entry.GetCredentialOrigin()):
		return 0
	case entry.GetLocal():
		return 1
	}
	return 2
}

// Heading of the providers of a rank
func heading(rank int) string {
	switch rank {
	case 0:
		return "Ready to use"
	case 3:
		return "Can't be used yet"
	}
	return "Not connected"
}

// How a credential connects, in a few words: connected, from a variable or another program, or needing none
func (p *providers) connection(origin providerv1.Origin, source string) string {
	t := p.kit.Env.Theme
	switch origin {
	case providerv1.Origin_ORIGIN_SAVED:
		return render.Good("Connected", t)
	case providerv1.Origin_ORIGIN_ENVIRONMENT:
		return render.Good("Connected", t) + render.Dim(" with "+source, t)
	case providerv1.Origin_ORIGIN_EXTERNAL:
		return render.Good("Connected", t) + render.Dim(" through "+source, t)
	case providerv1.Origin_ORIGIN_UNNEEDED:
		return render.Good("Ready", t) + render.Dim(", no account needed", t)
	}
	return ""
}

// State of the provider in a few words
func (p *providers) status(entry *providerv1.Provider) string {
	t := p.kit.Env.Theme
	switch {
	case !entry.GetKnown():
		return render.Warm("Unknown kind", t)
	case !entry.GetUsable() && entry.GetBaseUrl() == "":
		return render.Warm("Needs an address", t)
	case !entry.GetUsable():
		return render.Warm("Can't be used", t)
	}
	state := p.connection(entry.GetCredentialOrigin(), entry.GetCredentialSource())
	switch {
	case state != "":
	case len(entry.GetAccounts()) > 0:
		state = render.Warm("No account connected", t)
	default:
		state = render.Dim("Not connected", t)
	}
	if n := len(entry.GetAccounts()); n > 1 {
		state += render.Dim(", "+render.Count(n, "account"), t)
	}
	return state
}

// One sentence on whether the agent can use the provider and what to do when it can't
func (p *providers) standing(entry *providerv1.Provider) string {
	t := p.kit.Env.Theme
	name := entry.GetDisplayName()
	switch {
	case !entry.GetKnown():
		return render.Warm("icebath doesn't know the kind of provider "+entry.GetSpec()+", so the agent can't use it.", t)
	case !entry.GetUsable() && entry.GetBaseUrl() == "":
		return render.Warm(name+" needs the address of your server. Open it and set the address.", t)
	case !entry.GetUsable():
		return render.Warm(name+" takes requests in the "+modeWords(entry.GetApiMode())+" format, which icebath can't send yet.", t)
	}
	source := entry.GetCredentialSource()
	switch entry.GetCredentialOrigin() {
	case providerv1.Origin_ORIGIN_UNNEEDED:
		return render.Good(name+" runs without an account, so the agent can use it now.", t)
	case providerv1.Origin_ORIGIN_SAVED, providerv1.Origin_ORIGIN_ENVIRONMENT, providerv1.Origin_ORIGIN_EXTERNAL:
		switch {
		case entry.GetAccount() != "":
			return render.Good("Connected with the account "+accountTitle(entry, entry.GetAccount())+".", t)
		case entry.GetCredentialOrigin() == providerv1.Origin_ORIGIN_ENVIRONMENT:
			return render.Good("Connected with the API key in "+source+".", t)
		case entry.GetCredentialOrigin() == providerv1.Origin_ORIGIN_EXTERNAL:
			return render.Good("Connected through "+source+".", t)
		}
		return render.Good("Connected.", t)
	}
	if len(entry.GetAccounts()) > 0 {
		return render.Warm("None of its accounts is connected. Open it and connect one.", t)
	}
	return "Not connected. Open it and connect your account."
}

// What the provider is, whether the agent can use it, the facts a person acts on, and what its account may still spend
func (p *providers) describe(entry *providerv1.Provider) []string {
	t := p.kit.Env.Theme
	var lines []string
	if about := entry.GetDescription(); about != "" {
		lines = append(lines, about)
	}
	lines = append(lines, p.standing(entry))
	var facts []string
	if model := entry.GetDefaultModel(); model != "" {
		facts = append(facts, "Default model: "+model)
	}
	if base := entry.GetBaseUrl(); base != "" && entry.GetLocal() {
		facts = append(facts, "Address: "+base)
	}
	if !connected(entry.GetCredentialOrigin()) && entry.GetUsable() && entry.GetSignupUrl() != "" {
		facts = append(facts, "Get an API key at "+entry.GetSignupUrl())
	}
	if len(facts) > 0 {
		lines = append(lines, render.Dim(strings.Join(facts, " · "), t))
	}
	if entry.GetHidden() {
		lines = append(lines, render.Dim("Left out of /model by Settings › Model list › Online list › Hidden providers.", t))
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

func accountTitle(entry *providerv1.Provider, name string) string {
	for _, account := range entry.GetAccounts() {
		if account.GetName() == name {
			return account.GetTitle()
		}
	}
	return name
}

// Opens the settings of the provider, the providers screen closing
func (p *providers) configure(entry *providerv1.Provider) tea.Cmd {
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, entry.GetSection())))
}

// Title of the screen, naming the profile when it was opened on one
func title(profile string) string {
	if profile == "" {
		return "Providers"
	}
	return "Providers · " + profile
}

// Kind of provider a profile can add: what group it is listed under and in what order
func kindGroup(spec *providerv1.Spec) (int, string) {
	switch {
	case connected(spec.GetDetectedOrigin()) && len(spec.GetProviders()) == 0:
		return 0, "Found on this computer"
	case spec.GetRank() > 0:
		return 1, "Well known"
	case spec.GetLocal():
		return 4, "On this computer"
	case spec.GetAggregator():
		return 3, "One key, many models"
	}
	return 2, "Online"
}

// Kinds of provider to add to the profile under the headings of their group: those found on this computer first, then the well-known
// ones in the order a person looks for them, the other companies online by name, those that sell the models of many in one, and the
// servers that run on this computer. Each says what it is beside it or under the list, and what was found of it
func (p *providers) add() tea.Cmd {
	p.hub.Busy("Reading the kinds of provider you can add…")
	return tui.Call(p.hub, func() (*providerv1.SpecsResponse, error) {
		return p.service.Specs(p.kit.Env.Ctx, &providerv1.SpecsRequest{Profile: p.profile})
	}, func(response *providerv1.SpecsResponse) tea.Cmd {
		p.hub.Quiet()
		t := p.kit.Env.Theme
		specs := slices.DeleteFunc(response.GetSpecs(), func(spec *providerv1.Spec) bool { return !spec.GetUsable() })
		slices.SortStableFunc(specs, func(a, b *providerv1.Spec) int {
			ra, _ := kindGroup(a)
			rb, _ := kindGroup(b)
			if ra == rb && ra == 1 {
				return cmp.Compare(a.GetRank(), b.GetRank())
			}
			return cmp.Or(cmp.Compare(ra, rb), strings.Compare(strings.ToLower(a.GetDisplayName()), strings.ToLower(b.GetDisplayName())))
		})
		var list *cards.List
		list = cards.NewList(p.kit.Env, cards.ListSpec{
			Title:   "Add a provider",
			Glyph:   t.Glyphs.Models,
			Key:     "providers:add",
			Context: cards.HubContext,
			Primary: "hub.use",
			Pick:    true,
			Bar:     len(specs) > 8,
			Search:  "Search providers",
			Intro: func() []string {
				return []string{"Pick the company or server whose models you want the agent to use. You need an account there, unless it runs on this computer."}
			},
			Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
			Rows: func() [][]string {
				rows := make([][]string, len(specs))
				for i, spec := range specs {
					state := ""
					switch {
					case len(spec.GetProviders()) > 0:
						state = render.Dim("Added", t)
					case connected(spec.GetDetectedOrigin()):
						state = render.Good("Found", t)
					}
					rows[i] = []string{spec.GetDisplayName(), state, render.Dim(spec.GetDescription(), t)}
				}
				return rows
			},
			Group: func(i int) string {
				_, group := kindGroup(specs[i])
				return group
			},
			Words: func(i int) string {
				return specs[i].GetDescription() + " " + specs[i].GetName() + " " + strings.Join(specs[i].GetAliases(), " ")
			},
			Detail: func(i int) []string {
				var lines []string
				if !list.Shows(2) && specs[i].GetDescription() != "" {
					lines = append(lines, specs[i].GetDescription())
				}
				return append(lines, p.kindFacts(specs[i])...)
			},
			Empty: func() string { return "There is no kind of provider to add." },
			Hints: func(_ *cards.List, i int) []wm.Hint {
				if i < 0 {
					return nil
				}
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			},
			Actions: map[string]func(*cards.List, int) tea.Cmd{
				"hub.use": func(_ *cards.List, i int) tea.Cmd { return tea.Batch(p.hub.Close(list), p.create(specs[i])) },
			},
		})
		list.ToggleDetail()
		return p.hub.Push(list)
	})
}

// What a kind to add brings with it: a credential found for it, the providers of the profile of that kind, and where to get a key
func (p *providers) kindFacts(spec *providerv1.Spec) []string {
	t := p.kit.Env.Theme
	var lines []string
	switch source := spec.GetDetectedSource(); spec.GetDetectedOrigin() {
	case providerv1.Origin_ORIGIN_ENVIRONMENT:
		lines = append(lines, render.Good("Found an API key in "+source+", so it is connected as soon as you add it.", t))
	case providerv1.Origin_ORIGIN_EXTERNAL:
		lines = append(lines, render.Good("Found the sign-in of "+source+" on this computer, so it is connected as soon as you add it.", t))
	case providerv1.Origin_ORIGIN_UNNEEDED:
		lines = append(lines, "Needs no account.")
	}
	if have := spec.GetProviders(); len(have) > 0 {
		names := make([]string, len(have))
		for i, name := range have {
			names[i] = name
			for _, entry := range p.rows {
				if entry.GetName() == name {
					names[i] = entry.GetDisplayName()
				}
			}
		}
		lines = append(lines, "You have it already as "+strings.Join(names, " and ")+". Add it again only for a second setup, such as another address. "+
			"Another account goes on the page of the one you have.")
	}
	if url := spec.GetSignupUrl(); url != "" && spec.GetKeyed() && !connected(spec.GetDetectedOrigin()) {
		lines = append(lines, render.Dim("Get an API key at "+url, t))
	}
	return lines
}

// Adds a provider of the kind, opens its page and takes the next step: the address of a server of your own, else connecting the first
// account, unless it is connected already
func (p *providers) create(spec *providerv1.Spec) tea.Cmd {
	p.hub.Busy("Adding " + spec.GetDisplayName() + "…")
	return tui.Call(p.hub, func() (*providerv1.AddResponse, error) {
		return p.service.Add(p.kit.Env.Ctx, &providerv1.AddRequest{Profile: p.profile, Spec: spec.GetName()})
	}, func(response *providerv1.AddResponse) tea.Cmd {
		p.hub.Quiet()
		added := response.GetProvider()
		cmds := []tea.Cmd{p.load(added.GetName()), p.show(added)}
		switch {
		case !added.GetUsable() && added.GetBaseUrl() == "":
			cmds = append(cmds, p.hub.Done("Added "+added.GetDisplayName()+". Now type the address of your server."), p.address(added, true))
		case connected(added.GetCredentialOrigin()):
			cmds = append(cmds, p.hub.Done("Added "+added.GetDisplayName()+". It is connected already: close this and send a message to start."))
		default:
			cmds = append(cmds, p.hub.Done("Added "+added.GetDisplayName()+". Now connect your account."), p.connect(p.first(added)))
		}
		return tea.Batch(cmds...)
	})
}

// Asks the address of the provider's server, saved as its Base URL. Then connects its first account when it has none and asked
func (p *providers) address(entry *providerv1.Provider, then bool) tea.Cmd {
	field := cards.Field{ID: "address", Label: "Address", Value: entry.GetBaseUrl(), Placeholder: "http://localhost:11434/v1", Required: true,
		Help: "Where requests to " + entry.GetDisplayName() + " go, starting with http:// or https://.\nThe server's own documentation calls it the Base URL.",
		Check: func(value string) string {
			if value = strings.TrimSpace(value); !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
				return "Start the address with http:// or https://."
			}
			return ""
		}}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Address",
		Intro:  []string{"The address of the server that runs the models of " + entry.GetDisplayName() + "."},
		Fields: []cards.Field{field},
		Submit: "Save",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			address := strings.TrimRight(strings.TrimSpace(values["address"]), "/")
			form.SetBusy(true)
			return tui.Async(func() func() tea.Cmd {
				err := p.set(entry.GetSection()+"."+providerconfig.BaseURL, strconv.Quote(address))
				var now *providerv1.Provider
				if err == nil {
					now = p.find(entry.GetName())
				}
				return func() tea.Cmd {
					form.SetBusy(false)
					if err != nil {
						form.SetError("address", tui.Reason(err))
						return nil
					}
					if now == nil {
						now = entry
					}
					cmds := []tea.Cmd{p.hub.Close(form), p.load(entry.GetName())}
					switch {
					case !then || connected(now.GetCredentialOrigin()) || len(p.ways(now)) == 0:
						cmds = append(cmds, p.hub.Done(entry.GetDisplayName()+" now sends requests to "+address+"."))
					default:
						cmds = append(cmds, p.hub.Done(entry.GetDisplayName()+" now sends requests to "+address+". Now connect your account."),
							p.connect(p.first(now)))
					}
					return tea.Batch(cmds...)
				}
			})
		},
	})
	return p.hub.Push(form)
}

// Provider of the profile with the name, read now, nil when it is gone or the read failed
func (p *providers) find(name string) *providerv1.Provider {
	response, err := p.service.List(p.kit.Env.Ctx, &providerv1.ListRequest{Profile: p.profile})
	if err != nil {
		return nil
	}
	for _, entry := range response.GetProviders() {
		if entry.GetName() == name {
			return entry
		}
	}
	return nil
}

// Ways to connect an account of the provider: every way a connector registered for its kind, then pasting an API key and naming the
// variable that holds one when its accounts keep a key
func (p *providers) ways(entry *providerv1.Provider) []way {
	var ways []way
	for _, connector := range p.connectors {
		if connector.Spec == entry.GetSpec() {
			for _, w := range connector.Ways {
				about := ""
				if strings.HasPrefix(w.Kind, credentials.OAuth) {
					about = "Sign in with your browser"
				}
				ways = append(ways, way{Way: w, about: about})
			}
		}
	}
	if entry.GetKeyed() {
		key := "Paste a key from your " + entry.GetDisplayName() + " account"
		ways = append(ways, way{Way: Way{Title: "API key", Open: func(_ *tui.Kit, target Target) tea.Cmd { return p.key(target) }}, inline: true, about: key},
			way{Way: Way{Title: "Key from an environment variable", Open: func(_ *tui.Kit, target Target) tea.Cmd { return p.linked(target) }}, inline: true,
				about: "Read the key from a variable you set yourself, such as in your shell"})
	}
	return ways
}

// Way to connect an account, what it does, and whether it opens over the hub rather than in place of it, as the API key field does
type way struct {
	Way
	about  string
	inline bool
}

// Target of the first account of a provider, made only once a way to connect it succeeds
func (p *providers) first(entry *providerv1.Provider) Target {
	return p.fresh(entry, firstName, firstTitle, false)
}

// Target of a new account of the provider under the name and title, made only once a way to connect it succeeds
func (p *providers) fresh(entry *providerv1.Provider, name, title string, titled bool) Target {
	return Target{Profile: p.profile, Provider: entry, Name: name, Title: title, Titled: titled,
		make: func(ctx context.Context, name, title string) (*providerv1.Account, error) {
			response, err := p.service.AddAccount(ctx, &providerv1.AddAccountRequest{Profile: p.profile, Provider: entry.GetName(), Name: name, Title: title})
			return response.GetAccount(), err
		}}
}

// Opens the one way to connect the account, or a choice of the ways, each saying what it does
func (p *providers) connect(target Target) tea.Cmd {
	ways := p.ways(target.Provider)
	switch len(ways) {
	case 0:
		return p.hub.Say(target.Provider.GetDisplayName()+" connects through its own sign-in on this computer, so there is nothing to connect here.", cards.NoteInfo)
	case 1:
		return p.take(ways[0], target)
	}
	acts := make([]action, len(ways))
	for i, w := range ways {
		acts[i] = action{title: w.Title, about: w.about, verb: "connect", run: func() tea.Cmd { return p.take(w, target) }, closes: true}
	}
	return p.actions("Connect", []string{"Pick how to connect your " + target.Provider.GetDisplayName() + " account."}, nil, func() []action { return acts })
}

// Starts a way to connect, its page shown in this dialog over the provider it connects, which is read again once it is connected
func (p *providers) take(w way, target Target) tea.Cmd {
	target.Hub = p.hub
	target.Done = func() tea.Cmd { return p.load(target.Provider.GetName()) }
	return w.Open(p.kit, target)
}

// Asks the name of a new account of the provider, then connects it, the account made once it is connected
func (p *providers) account(entry *providerv1.Provider) tea.Cmd {
	field := cards.Field{Label: "Name", Placeholder: "Work", Help: "The name you know this account by, such as Work or Personal.",
		Check: func(value string) string {
			if slug(value) == "" {
				return "Use at least one letter or digit."
			}
			for _, account := range entry.GetAccounts() {
				if account.GetName() == slug(value) {
					return entry.GetDisplayName() + " has an account by that name already."
				}
			}
			return ""
		}}
	return p.kit.Ask(p.hub, "New account", field, "Next", func(name string) tea.Cmd {
		return p.connect(p.fresh(entry, slug(name), strings.TrimSpace(name), true))
	})
}

// Name of an account from its title: lower case, the rest of what a key can't hold made dashes
func slug(title string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			out.WriteRune(r)
		case out.Len() > 0 && !strings.HasSuffix(out.String(), "-"):
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-_")
}

// Form of the API key of the account, kept once sent, the account made first when it waits for one
func (p *providers) key(target Target) tea.Cmd {
	name := target.Provider.GetDisplayName()
	var help []string
	if url := target.Provider.GetSignupUrl(); url != "" {
		help = append(help, "Get one at "+url+".")
	}
	if target.Account != nil && connected(target.Account.GetCredentialOrigin()) {
		help = append(help, "The key kept now is replaced.")
	}
	field := cards.Field{ID: "key", Label: "API key", Kind: cards.FieldSecret, Required: true, Help: strings.Join(help, "\n")}
	was := connected(target.Provider.GetCredentialOrigin())
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "API key",
		Intro:  []string{"Paste the API key of your " + name + " account" + named(target) + "."},
		Fields: []cards.Field{field},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			form.SetBusy(true)
			return tui.Async(func() func() tea.Cmd {
				err := Store(p.kit.Env.Ctx, p.settings, target, map[string]any{providerconfig.APIKeyField: strings.TrimSpace(values["key"]), providerconfig.KeyEnv: nil})
				return func() tea.Cmd {
					form.SetBusy(false)
					if err != nil {
						form.SetError("key", tui.Reason(err))
						return nil
					}
					return tea.Batch(p.hub.Close(form), p.hub.Done(connectedNote(target, was, false)), p.load(target.Provider.GetName()))
				}
			})
		},
	})
	return p.hub.Push(form)
}

// Name of the account of a target as a sentence says it after "account", nothing for a first account no name was asked for
func named(target Target) string {
	if target.Account == nil && !target.Titled {
		return ""
	}
	return " " + target.Label()
}

// Sets one setting of the profile from YAML text
func (p *providers) set(key, value string) error {
	_, err := p.settings.Set(p.kit.Env.Ctx, &configv1.SetRequest{Profile: p.profile, Key: key, Value: value})
	return err
}
