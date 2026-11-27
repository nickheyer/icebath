package app

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/app"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Screen of the providers of the profile
const Screen = "providers"

// Start of the argument of the providers screen that opens the logins of the provider named after it, such as logins:anthropic
const LoginsOf = "logins:"

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
		Title:   "Providers",
		Short:   "Add providers to a profile and connect their accounts",
		Slash:   []string{"providers", "connect", "logins"},
		Usage:   "[@profile] [search]",
		Actions: append(slices.Clone(signinKeys), loginKeys...),
		Where:   map[string]string{signinContext: "a sign in in progress", loginsContext: "the accounts of a provider"},
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
	page   *page
	loaded bool
	err    error
}

func (p *providers) open(search string) tea.Cmd {
	p.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   title(p.profile),
		Glyph:   p.kit.Env.Theme.Glyphs.Models,
		Key:     "providers",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search providers",
		Columns: []render.Column{{Max: 48}, {Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(p.rows))
			for i, entry := range p.rows {
				rows[i] = []string{entry.GetDisplayName(), p.status(entry)}
			}
			return rows
		},
		Detail: func(i int) []string { return p.describe(p.rows[i]) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "This profile has no providers. Add one.") },
		Free:   map[string]bool{"hub.refresh": true, "hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			hints := []wm.Hint{}
			if i >= 0 {
				hints = append(hints, wm.Hint{Action: "hub.use", Label: "open"}, wm.Hint{Action: "hub.edit", Label: "settings"}, wm.Hint{Action: "hub.remove"})
			}
			return append(hints, wm.Hint{Action: "hub.add"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use":  func(_ *cards.List, i int) tea.Cmd { return p.show(p.rows[i]) },
			"hub.edit": func(_ *cards.List, i int) tea.Cmd { return p.configure(p.rows[i]) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				entry := p.rows[i]
				l.Confirm("Remove "+entry.GetDisplayName()+" and its accounts from the profile?", func() tea.Cmd {
					return tui.Call(p.hub, func() (*providerv1.RemoveResponse, error) {
						return p.service.Remove(p.kit.Env.Ctx, &providerv1.RemoveRequest{Profile: p.profile, Provider: entry.GetName()})
					}, func(*providerv1.RemoveResponse) tea.Cmd {
						return tea.Batch(p.hub.Done("Removed "+entry.GetDisplayName()), p.load(""))
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

// Reads the providers again, those that work first: connected, then local, then the rest, each by name. The one named selected is
// highlighted
func (p *providers) load(selected string) tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		response, err := p.service.List(p.kit.Env.Ctx, &providerv1.ListRequest{Profile: p.profile})
		rows := response.GetProviders()
		slices.SortStableFunc(rows, func(a, b *providerv1.Provider) int {
			return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(strings.ToLower(a.GetDisplayName()), strings.ToLower(b.GetDisplayName())))
		})
		return func() tea.Cmd {
			p.rows, p.err, p.loaded = rows, err, true
			p.list.Refresh()
			var opened tea.Cmd
			for i, entry := range p.rows {
				if entry.GetName() == selected && !p.list.Filtered() {
					p.list.Select(i)
				}
				if p.page != nil && entry.GetName() == p.page.provider.GetName() {
					p.page.provider = entry
					p.page.list.Refresh()
				}
				if p.focus != "" && entry.GetName() == p.focus {
					p.list.Select(i)
					opened = p.show(entry)
				}
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

// Status of a credential in a few words
func (p *providers) credential(origin providerv1.Origin, source string) string {
	t := p.kit.Env.Theme
	switch origin {
	case providerv1.Origin_ORIGIN_SAVED:
		return render.Good("Connected", t)
	case providerv1.Origin_ORIGIN_ENVIRONMENT:
		return render.Good("Connected", t) + render.Dim("  from "+source, t)
	case providerv1.Origin_ORIGIN_EXTERNAL:
		return render.Good("Connected", t) + render.Dim("  via "+source, t)
	case providerv1.Origin_ORIGIN_UNNEEDED:
		return render.Good("Local", t)
	}
	return ""
}

// Status of the provider in a few words
func (p *providers) status(entry *providerv1.Provider) string {
	t := p.kit.Env.Theme
	if status := p.credential(entry.GetCredentialOrigin(), entry.GetCredentialSource()); status != "" {
		if n := len(entry.GetAccounts()); n > 1 {
			status += render.Dim("  "+render.Count(n, "account"), t)
		}
		return status
	}
	switch {
	case !entry.GetKnown():
		return render.Warm("Unknown kind "+entry.GetSpec(), t)
	case !entry.GetUsable():
		return render.Dim("Not supported", t)
	case entry.GetLocal():
		return "Local"
	}
	return render.Dim("Not connected", t)
}

// What the provider is, how it is connected, and where it runs
func (p *providers) describe(entry *providerv1.Provider) []string {
	t := p.kit.Env.Theme
	var lines []string
	if about := entry.GetDescription(); about != "" {
		lines = append(lines, about)
	}
	switch {
	case !entry.GetKnown():
		lines = append(lines, render.Warm("No plugin registers the kind "+entry.GetSpec(), t))
	case !entry.GetUsable() && entry.GetBaseUrl() == "":
		lines = append(lines, render.Warm("Has no base URL. Set one in its settings", t))
	case !entry.GetUsable():
		lines = append(lines, render.Warm("Uses the "+strings.ReplaceAll(entry.GetApiMode(), "_", " ")+" API, which icebath doesn't support yet", t))
	case entry.GetAccount() != "":
		lines = append(lines, "Account "+accountTitle(entry, entry.GetAccount()))
	case connected(entry.GetCredentialOrigin()):
	case len(entry.GetKeyEnv()) > 0:
		lines = append(lines, "Add an account, or set "+strings.Join(entry.GetKeyEnv(), " or "))
	case entry.GetSignupUrl() != "":
		lines = append(lines, "Add an account. Get a key at "+entry.GetSignupUrl())
	}
	if model := entry.GetDefaultModel(); model != "" {
		lines = append(lines, render.Dim("Default model: "+model, t))
	}
	if base := entry.GetBaseUrl(); base != "" {
		lines = append(lines, render.Dim(base, t))
	}
	if reading := entry.GetAccountUsage(); reading != nil {
		lines = append(lines, "")
		lines = append(lines, provider.UsageOf(reading).Lines(false)...)
	}
	return append(lines, render.Dim(entry.GetName()+" · "+entry.GetSpec(), t))
}

func accountTitle(entry *providerv1.Provider, name string) string {
	for _, account := range entry.GetAccounts() {
		if account.GetName() == name {
			return account.GetTitle()
		}
	}
	return name
}

// Opens the settings of the provider
func (p *providers) configure(entry *providerv1.Provider) tea.Cmd {
	return tea.Batch(p.kit.WM.DismissCard(p.hub), p.kit.Open(configapp.Settings, profile.Join(p.profile, entry.GetSection())))
}

// Title of the screen, naming the profile when it was opened on one
func title(profile string) string {
	if profile == "" {
		return "Providers"
	}
	return "Providers of " + profile
}

// Picks a kind of provider to add to the profile, those found on this machine first
func (p *providers) add() tea.Cmd {
	return tui.Call(p.hub, func() (*providerv1.SpecsResponse, error) {
		return p.service.Specs(p.kit.Env.Ctx, &providerv1.SpecsRequest{Profile: p.profile})
	}, func(response *providerv1.SpecsResponse) tea.Cmd {
		specs := slices.DeleteFunc(response.GetSpecs(), func(spec *providerv1.Spec) bool { return !spec.GetUsable() })
		slices.SortStableFunc(specs, func(a, b *providerv1.Spec) int {
			found := func(spec *providerv1.Spec) int {
				if connected(spec.GetDetectedOrigin()) && len(spec.GetProviders()) == 0 {
					return 0
				}
				return 1
			}
			return cmp.Or(cmp.Compare(found(a), found(b)), strings.Compare(strings.ToLower(a.GetDisplayName()), strings.ToLower(b.GetDisplayName())))
		})
		choices := make([]tui.Choice, len(specs))
		for i, spec := range specs {
			detail := spec.GetDescription()
			if status := p.credential(spec.GetDetectedOrigin(), spec.GetDetectedSource()); status != "" {
				detail = strings.TrimSpace(status + "  " + detail)
			}
			if have := spec.GetProviders(); len(have) > 0 {
				detail = strings.TrimSpace(detail + "  " + render.Dim("in profile: "+strings.Join(have, ", "), p.kit.Env.Theme))
			}
			choices[i] = tui.Choice{ID: spec.GetName(), Title: spec.GetDisplayName(), Detail: detail}
		}
		return p.kit.Pick(p.hub, "Add provider", "add", choices, "", func(choice tui.Choice) tea.Cmd {
			return tui.Call(p.hub, func() (*providerv1.AddResponse, error) {
				return p.service.Add(p.kit.Env.Ctx, &providerv1.AddRequest{Profile: p.profile, Spec: choice.ID})
			}, func(response *providerv1.AddResponse) tea.Cmd {
				added := response.GetProvider()
				if !added.GetUsable() && added.GetBaseUrl() == "" {
					return tea.Batch(p.kit.WM.Toast("Added "+added.GetDisplayName()+". Set its base URL", wm.Success, wm.DefaultToastTTL), p.configure(added))
				}
				return tea.Batch(p.hub.Done("Added "+added.GetDisplayName()), p.load(added.GetName()), p.show(added))
			})
		})
	})
}

// Ways to connect an account of the provider: pasting an API key when its accounts keep one, then every way a connector registered for
// its kind
func (p *providers) ways(entry *providerv1.Provider) []way {
	var ways []way
	if entry.GetKeyed() {
		ways = append(ways, way{Way: Way{Title: "API key", Open: func(_ *tui.Kit, target Target) tea.Cmd { return p.key(target) }}, inline: true},
			way{Way: Way{Title: "Key from an environment variable", Open: func(_ *tui.Kit, target Target) tea.Cmd { return p.linked(target) }}, inline: true})
	}
	for _, connector := range p.connectors {
		if connector.Spec == entry.GetSpec() {
			for _, w := range connector.Ways {
				ways = append(ways, way{Way: w})
			}
		}
	}
	return ways
}

// Way to connect an account, and whether it opens over the hub rather than in place of it, as the API key field does
type way struct {
	Way
	inline bool
}

// Opens the one way to connect the account, or a choice of the ways
func (p *providers) connect(target Target) tea.Cmd {
	ways := p.ways(target.Provider)
	switch len(ways) {
	case 0:
		return p.hub.Say(target.Provider.GetDisplayName()+" connects through its own sign-in on this machine", cards.NoteInfo)
	case 1:
		return p.take(ways[0], target)
	}
	choices := make([]tui.Choice, len(ways))
	for i, w := range ways {
		choices[i] = tui.Choice{ID: strconv.Itoa(i), Title: w.Title}
	}
	return p.kit.Pick(p.hub, "Connect "+target.Account.GetTitle(), "connect", choices, "", func(choice tui.Choice) tea.Cmd {
		i, _ := strconv.Atoi(choice.ID)
		return p.take(ways[i], target)
	})
}

func (p *providers) take(w way, target Target) tea.Cmd {
	if w.inline {
		return w.Open(p.kit, target)
	}
	return tea.Batch(p.kit.WM.DismissCard(p.hub), w.Open(p.kit, target))
}

// Adds an account to the provider under a name asked for, then connects it
func (p *providers) account(entry *providerv1.Provider) tea.Cmd {
	field := cards.Field{Label: "Name", Placeholder: "Work", Help: "Name the account is shown by"}
	return p.kit.Ask(p.hub, "Add account to "+entry.GetDisplayName(), field, "Add", func(name string) tea.Cmd {
		return tui.Call(p.hub, func() (*providerv1.AddAccountResponse, error) {
			return p.service.AddAccount(p.kit.Env.Ctx, &providerv1.AddAccountRequest{Profile: p.profile, Provider: entry.GetName(), Name: slug(name), Title: strings.TrimSpace(name)})
		}, func(response *providerv1.AddAccountResponse) tea.Cmd {
			return tea.Batch(p.load(entry.GetName()), p.connect(Target{Profile: p.profile, Provider: entry, Account: response.GetAccount()}))
		})
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

func (p *providers) key(target Target) tea.Cmd {
	field := cards.Field{ID: "key", Label: "API key", Kind: cards.FieldSecret, Required: true}
	if url := target.Provider.GetSignupUrl(); url != "" {
		field.Help = "Get one at " + url
	}
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Connect " + target.Account.GetTitle(),
		Fields: []cards.Field{field},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			return tui.Call(p.hub, func() (struct{}, error) {
				return struct{}{}, Store(p.kit.Env.Ctx, p.settings, target, map[string]any{providerconfig.APIKeyField: strings.TrimSpace(values["key"]), providerconfig.KeyEnv: nil})
			}, func(struct{}) tea.Cmd {
				return tea.Batch(p.hub.Close(form), p.hub.Done("Connected "+target.Provider.GetDisplayName()), p.load(target.Provider.GetName()))
			})
		},
	})
	return p.hub.Push(form)
}

// Page of one provider: its accounts, then its models and settings
type page struct {
	provider *providerv1.Provider
	list     *cards.List
}

// Row of a page: an account, or one of the views of the provider
type entry struct {
	account *providerv1.Account
	view    string
}

const (
	viewModels   = "models"
	viewUsage    = "usage"
	viewRotation = "rotation"
	viewSettings = "settings"
)

func (p *page) entries() []entry {
	var rows []entry
	for _, account := range p.provider.GetAccounts() {
		rows = append(rows, entry{account: account})
	}
	rows = append(rows, entry{view: viewModels})
	if p.provider.GetUsage() {
		rows = append(rows, entry{view: viewUsage})
	}
	if len(p.provider.GetAccounts()) > 1 {
		rows = append(rows, entry{view: viewRotation})
	}
	return append(rows, entry{view: viewSettings})
}

// First window of a reading as one line: its label, its bar and its share used
func glance(reading *providerv1.AccountUsage) string {
	for _, window := range reading.GetWindows() {
		if window.Used != nil {
			used := min(max(window.GetUsed(), 0), 100) / 100
			return window.GetLabel() + " " + provider.Bar(used, 10) + " " + provider.PercentUsed(used) + " used"
		}
	}
	return ""
}

// Opens the page of the provider over the list
func (p *providers) show(provider *providerv1.Provider) tea.Cmd {
	t := p.kit.Env.Theme
	pg := &page{provider: provider}
	var rows []entry
	pg.list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   provider.GetDisplayName(),
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + provider.GetName(),
		Context: loginsContext,
		Shared:  []string{cards.HubContext},
		Primary: "hub.use",
		Pick:    true,
		Columns: []render.Column{{}, {Max: 40}, {Flex: 1}},
		Rows: func() [][]string {
			rows = pg.entries()
			cells := make([][]string, len(rows))
			for i, row := range rows {
				switch {
				case row.account != nil:
					status := cmp.Or(p.credential(row.account.GetCredentialOrigin(), row.account.GetCredentialSource()), render.Dim("Not connected", t))
					if state := p.state(row.account); state != "" {
						status = state
					}
					cells[i] = []string{marks(pg.provider, row.account, t), row.account.GetTitle(), status}
				case row.view == viewModels:
					about := "Every model it lists"
					if n := len(pg.provider.GetModels()); n > 0 {
						about = render.Count(n, "model") + " picked"
					}
					cells[i] = []string{"", "Models " + render.Dim(t.Glyphs.Forward, t), render.Dim(about, t)}
				case row.view == viewUsage:
					cells[i] = []string{"", "Usage " + render.Dim(t.Glyphs.Forward, t), render.Dim(cmp.Or(glance(pg.provider.GetAccountUsage()), "What the account may still spend"), t)}
				case row.view == viewRotation:
					cells[i] = []string{"", "Rotation " + render.Dim(t.Glyphs.Forward, t), render.Dim(choice(pg.provider), t)}
				default:
					cells[i] = []string{"", "Settings " + render.Dim(t.Glyphs.Forward, t), render.Dim("Name, base URL, default model and more", t)}
				}
			}
			return cells
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(rows) || rows[i].account == nil {
				return nil
			}
			return p.detail(pg.provider, rows[i].account)
		},
		Banner: func() string {
			if len(pg.provider.GetAccounts()) > 0 || !pg.provider.GetKnown() {
				return ""
			}
			if connected(pg.provider.GetCredentialOrigin()) {
				return "Connected without an account: " + strings.TrimSpace(p.credential(pg.provider.GetCredentialOrigin(), pg.provider.GetCredentialSource()))
			}
			return "No account yet. Add one to connect"
		},
		Status: func() string { return "● in use  ★ default  ↻ in rotation" },
		Free:   map[string]bool{"hub.add": true, "hub.refresh": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var hints []wm.Hint
			switch {
			case i >= 0 && rows[i].account != nil:
				account := rows[i].account
				if !pg.provider.GetPinned() || pg.provider.GetAccount() != account.GetName() {
					hints = append(hints, wm.Hint{Action: "hub.use", Label: "use"})
				}
				label := "connect"
				if account.GetAuth() == "oauth" && account.GetStatus() != "unavailable" {
					label = "sign in again"
				}
				hints = append(hints, wm.Hint{Action: "hub.edit", Label: label}, wm.Hint{Action: "providers.logins.rotate"})
				if len(pg.provider.GetAccounts()) > 1 {
					hints = append(hints, wm.Hint{Action: "providers.logins.up"}, wm.Hint{Action: "providers.logins.down"})
				}
				if account.GetResets() != nil || account.GetStatus() == "dead" || account.GetStatus() == "needs_sign_in" || account.GetStatus() == "limited" {
					hints = append(hints, wm.Hint{Action: "providers.logins.reset"})
				}
				if account.GetRenewable() {
					hints = append(hints, wm.Hint{Action: "providers.logins.renew"})
				}
				if account.GetKeyEnv() != "" {
					hints = append(hints, wm.Hint{Action: "providers.logins.keep"})
				}
				hints = append(hints, wm.Hint{Action: "hub.rename"}, wm.Hint{Action: "hub.remove"})
			case i >= 0:
				hints = append(hints, wm.Hint{Action: "hub.use", Label: "open"})
			}
			return append(hints, wm.Hint{Action: "hub.add", Label: "add account"})
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				row := rows[i]
				switch {
				case row.account != nil && pg.provider.GetPinned() && pg.provider.GetAccount() == row.account.GetName():
					return nil
				case row.account != nil:
					return tui.Call(p.hub, func() (*providerv1.UseAccountResponse, error) {
						return p.service.UseAccount(p.kit.Env.Ctx, &providerv1.UseAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: row.account.GetName()})
					}, func(*providerv1.UseAccountResponse) tea.Cmd {
						return tea.Batch(p.hub.Done("Using "+row.account.GetTitle()), p.load(pg.provider.GetName()))
					})
				case row.view == viewModels:
					return p.models(pg)
				case row.view == viewUsage:
					return p.usage(pg)
				case row.view == viewRotation:
					return p.rotation(pg)
				}
				return p.configure(pg.provider)
			},
			"hub.edit": func(_ *cards.List, i int) tea.Cmd {
				if account := rows[i].account; account != nil {
					return p.connect(Target{Profile: p.profile, Provider: pg.provider, Account: account})
				}
				return nil
			},
			"hub.rename": func(_ *cards.List, i int) tea.Cmd {
				if account := rows[i].account; account != nil {
					return p.rename(pg, account)
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				account := rows[i].account
				if account == nil {
					return nil
				}
				l.Confirm("Remove the account "+account.GetTitle()+" and what it keeps?", func() tea.Cmd {
					return tui.Call(p.hub, func() (*providerv1.RemoveAccountResponse, error) {
						return p.service.RemoveAccount(p.kit.Env.Ctx, &providerv1.RemoveAccountRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
					}, func(removed *providerv1.RemoveAccountResponse) tea.Cmd {
						done := "Removed " + account.GetTitle()
						if cleared := removed.GetCleared(); len(cleared) > 0 {
							done += " and cleared " + strings.Join(cleared, ", ") + " from .env"
						}
						return tea.Batch(p.hub.Done(done), p.load(pg.provider.GetName()))
					})
				})
				return nil
			},
			"providers.logins.rotate": func(_ *cards.List, i int) tea.Cmd { return p.toggle(pg, rows[i].account) },
			"providers.logins.up":     func(_ *cards.List, i int) tea.Cmd { return p.move(pg, rows[i].account, -1) },
			"providers.logins.down":   func(_ *cards.List, i int) tea.Cmd { return p.move(pg, rows[i].account, 1) },
			"providers.logins.reset":  func(_ *cards.List, i int) tea.Cmd { return p.reset(pg, rows[i].account) },
			"providers.logins.renew":  func(_ *cards.List, i int) tea.Cmd { return p.renew(pg, rows[i].account) },
			"providers.logins.keep": func(l *cards.List, i int) tea.Cmd {
				account := rows[i].account
				if account == nil || account.GetKeyEnv() == "" {
					return nil
				}
				l.Confirm("Keep the key of "+account.GetKeyEnv()+" in icebath, so the account no longer reads it from the variable?", func() tea.Cmd {
					return tui.Call(p.hub, func() (*providerv1.KeepKeyResponse, error) {
						return p.service.KeepKey(p.kit.Env.Ctx, &providerv1.KeepKeyRequest{Profile: p.profile, Provider: pg.provider.GetName(), Account: account.GetName()})
					}, func(*providerv1.KeepKeyResponse) tea.Cmd {
						return tea.Batch(p.hub.Done("Kept the key of "+account.GetTitle()+" in icebath"), p.load(pg.provider.GetName()))
					})
				})
				return nil
			},
			"hub.add":     func(*cards.List, int) tea.Cmd { return p.account(pg.provider) },
			"hub.refresh": func(*cards.List, int) tea.Cmd { return p.load(pg.provider.GetName()) },
		},
	})
	pg.list.Refresh()
	p.page = pg
	return p.hub.Push(pg.list)
}

// Models the provider lists, those picked for /model marked; picking one adds it or takes it away
func (p *providers) models(pg *page) tea.Cmd {
	t := p.kit.Env.Theme
	var listed []string
	details := map[string]*providerv1.Model{}
	var failed error
	loaded := false
	all := func() []string {
		models := slices.Clone(pg.provider.GetModels())
		for _, model := range listed {
			if !slices.Contains(models, model) {
				models = append(models, model)
			}
		}
		return models
	}
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Models of " + pg.provider.GetDisplayName(),
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + pg.provider.GetName() + ":models",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search models",
		Columns: []render.Column{{}, {Flex: 1}, {Title: "Context"}, {Title: "$/M in·out"}, {Title: ""}},
		Rows: func() [][]string {
			ids := all()
			rows := make([][]string, len(ids))
			for i, model := range ids {
				mark := ""
				if slices.Contains(pg.provider.GetModels(), model) {
					mark = t.Glyphs.Live
				}
				detail := details[model]
				rows[i] = []string{mark, model, render.Dim(models.Tokens(detail.GetContext()), t), render.Dim(prices(detail), t), render.Dim(detail.GetNote(), t)}
			}
			return rows
		},
		Detail: func(i int) []string { return describeModel(details[all()[i]], t) },
		Banner: func() string {
			if failed != nil {
				return "Couldn't list models: " + tui.Reason(failed)
			}
			if len(pg.provider.GetModels()) == 0 {
				return "None picked: /model offers every model listed"
			}
			return ""
		},
		Empty: func() string { return tui.Waiting(loaded, failed, "No models listed") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			if slices.Contains(pg.provider.GetModels(), all()[i]) {
				return []wm.Hint{{Action: "hub.use", Label: "unpick"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "pick"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				model := all()[i]
				picked := slices.Clone(pg.provider.GetModels())
				if slices.Contains(picked, model) {
					picked = slices.DeleteFunc(picked, func(have string) bool { return have == model })
				} else {
					picked = append(picked, model)
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
					return p.load(pg.provider.GetName())
				})
			},
		},
	})
	return tea.Batch(p.hub.Push(list), tui.Async(func() func() tea.Cmd {
		response, err := p.service.Models(p.kit.Env.Ctx, &providerv1.ModelsRequest{Profile: p.profile, Provider: pg.provider.GetName()})
		return func() tea.Cmd {
			listed, failed, loaded = response.GetModels(), err, true
			for _, detail := range response.GetDetails() {
				details[detail.GetId()] = detail
			}
			list.Refresh()
			return nil
		}
	}))
}

// Prices of a model's input and output for a million tokens, empty when not known
func prices(detail *providerv1.Model) string {
	if detail.GetInputPrice() == "" && detail.GetOutputPrice() == "" {
		return ""
	}
	shown := func(price string) string {
		if price == "" {
			return "?"
		}
		return models.PerMillion(price)
	}
	return shown(detail.GetInputPrice()) + " · " + shown(detail.GetOutputPrice())
}

// What is known of a model: its window, its most output, its prices and what it can do
func describeModel(detail *providerv1.Model, t theme.Theme) []string {
	if detail == nil {
		return nil
	}
	var lines []string
	if detail.GetContext() > 0 {
		lines = append(lines, "Context: "+strs.Thousands(detail.GetContext())+" tokens")
	}
	if detail.GetMaxOutput() > 0 {
		lines = append(lines, "Max output: "+strs.Thousands(detail.GetMaxOutput())+" tokens")
	}
	if detail.GetInputPrice() != "" || detail.GetOutputPrice() != "" {
		line := "Price for a million tokens: " + prices(detail)
		if detail.GetCacheReadPrice() != "" {
			line += ", " + models.PerMillion(detail.GetCacheReadPrice()) + " read from cache"
		}
		lines = append(lines, line)
	}
	if detail.GetAbilities() != "" {
		lines = append(lines, "Capabilities: "+detail.GetAbilities())
	}
	if detail.Reasoning != nil && !detail.GetReasoning() {
		lines = append(lines, render.Dim("Takes no reasoning effort", t))
	}
	if detail.GetNote() != "" {
		lines = append(lines, render.Dim(detail.GetNote(), t))
	}
	return lines
}

// What the provider says its account in use may still spend, read now; ctrl+r reads it again
func (p *providers) usage(pg *page) tea.Cmd {
	t := p.kit.Env.Theme
	var lines []string
	var failed error
	loaded := false
	var list *cards.List
	read := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			response, err := p.service.Usage(p.kit.Env.Ctx, &providerv1.UsageRequest{Profile: p.profile, Provider: pg.provider.GetName()})
			return func() tea.Cmd {
				failed, loaded, lines = err, true, nil
				if reading := response.GetUsage(); reading != nil {
					lines = provider.UsageOf(reading).Lines(false)
				}
				list.Refresh()
				return nil
			}
		})
	}
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Usage of " + pg.provider.GetDisplayName(),
		Glyph:   t.Glyphs.Models,
		Key:     "providers:" + pg.provider.GetName() + ":usage",
		Context: cards.HubContext,
		Columns: []render.Column{{Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(lines))
			for i, line := range lines {
				rows[i] = []string{line}
			}
			return rows
		},
		Empty: func() string {
			return tui.Waiting(loaded, failed, "No account usage available: no account is connected, or the provider couldn't be asked")
		},
		Free:    map[string]bool{"hub.refresh": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.refresh": func(*cards.List, int) tea.Cmd { return read() }},
	})
	return tea.Batch(p.hub.Push(list), read())
}
