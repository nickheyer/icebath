package app

import (
	"cmp"
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Job in steps over the hub, titled as the job through every step: a list with headings, a form or a sign-in page each. Esc goes back a
// step as it was left, and from the first leaves the job
type job struct {
	*tui.Flow
	title string
}

func newJob(kit *tui.Kit, hub *cards.Hub, title string) *job {
	return &job{Flow: kit.Flow(hub, title), title: title}
}

// Kind of provider as the list to add one shows it: the group it is listed under and its place among them
func kindGroup(spec *providerv1.Spec) (int, string) {
	switch {
	case spec.GetDetectedOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED && len(spec.GetProviders()) == 0:
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

// What a provider being added holds until its last step: its kind, its address, and the provider once it was written
type adding struct {
	spec    *providerv1.Spec
	address string
	made    *providerv1.Provider
}

// Adds a provider as a job: the kind, picked from the kinds under the headings of today in their order, then its address only when the
// kind needs one, then how to connect it unless something on this computer connects it already or it needs no account. The provider is
// written when the last step succeeds
func (p *providers) add() tea.Cmd {
	j := newJob(p.kit, p.hub, "Add a provider")
	p.hub.Busy("Reading the kinds of provider you can add…")
	return tui.Call(p.hub, func() (*providerv1.SpecsResponse, error) {
		return p.service.Specs(p.kit.Env.Ctx, &providerv1.SpecsRequest{Profile: p.profile})
	}, func(response *providerv1.SpecsResponse) tea.Cmd {
		p.hub.Quiet()
		specs := slices.DeleteFunc(response.GetSpecs(), func(spec *providerv1.Spec) bool { return !spec.GetUsable() })
		slices.SortStableFunc(specs, func(a, b *providerv1.Spec) int {
			ra, _ := kindGroup(a)
			rb, _ := kindGroup(b)
			if ra == rb && ra == 1 {
				return cmp.Compare(a.GetRank(), b.GetRank())
			}
			return cmp.Or(cmp.Compare(ra, rb), strings.Compare(strings.ToLower(a.GetDisplayName()), strings.ToLower(b.GetDisplayName())))
		})
		return j.Show(p.kinds(specs, func(spec *providerv1.Spec) tea.Cmd { return p.kindPicked(j, &adding{spec: spec}) }))
	})
}

// Adds a provider of the kind as a job that starts after its kind, as a list that names one kind asks; every kind is listed when the kind is
// none icebath knows
func (p *providers) addOf(kind string) tea.Cmd {
	return tui.Call(p.hub, func() (*providerv1.SpecsResponse, error) {
		return p.service.Specs(p.kit.Env.Ctx, &providerv1.SpecsRequest{Profile: p.profile})
	}, func(response *providerv1.SpecsResponse) tea.Cmd {
		for _, spec := range response.GetSpecs() {
			if spec.GetName() == kind && spec.GetUsable() {
				return p.kindPicked(newJob(p.kit, p.hub, "Add "+spec.GetDisplayName()), &adding{spec: spec})
			}
		}
		return p.add()
	})
}

// List of the kinds to add under the headings of their groups, each saying what it is and what was found of it
func (p *providers) kinds(specs []*providerv1.Spec, pick func(spec *providerv1.Spec) tea.Cmd) *cards.List {
	t := p.kit.Env.Theme
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
		Intro:   func() []string { return []string{"Pick the company or server whose models you want the agent to use."} },
		Columns: []render.Column{{}, {}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(specs))
			for i, spec := range specs {
				state := ""
				switch {
				case len(spec.GetProviders()) > 0:
					state = render.Dim("Added", t)
				case spec.GetDetectedOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED:
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
			return []wm.Hint{{Action: "hub.use", Label: "next"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.use": func(_ *cards.List, i int) tea.Cmd { return pick(specs[i]) }},
	})
	list.ToggleDetail()
	return list
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
			if entry := p.find(name); entry != nil {
				names[i] = entry.GetDisplayName()
			}
		}
		lines = append(lines, "You have it already as "+strs.And(names)+". Add it again for a second setup, such as another address.")
	}
	if url := spec.GetSignupUrl(); url != "" && spec.GetKeyed() && spec.GetDetectedOrigin() == providerv1.Origin_ORIGIN_UNSPECIFIED {
		lines = append(lines, render.Dim("Get an API key at "+url, t))
	}
	return lines
}

// Next step once a kind is picked: its address when it needs one, else how to connect it
func (p *providers) kindPicked(j *job, a *adding) tea.Cmd {
	if !a.spec.GetNeedsAddress() {
		return p.connectStep(j, a)
	}
	fields := []cards.Field{{ID: "address", Label: "Address", Value: a.address,
		Help: "Where requests to the server go, such as http://localhost:8080/v1. It starts with http:// or https://.", Check: checkAddress}}
	return j.Ask("The address of the server that runs the models.", fields, "", func(values map[string]string) tea.Cmd {
		a.address = strings.TrimRight(strings.TrimSpace(values["address"]), "/")
		return p.connectStep(j, a)
	})
}

// Refusal of text that is no address, empty for one that is
func checkAddress(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "Type the address."
	case !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://"):
		return "Start the address with http:// or https://."
	}
	return ""
}

// How to connect the provider being added, unless something on this computer connects it already or it needs no account, which adds it
// at once
func (p *providers) connectStep(j *job, a *adding) tea.Cmd {
	stand := &providerv1.Provider{Name: a.spec.GetName(), Spec: a.spec.GetName(), DisplayName: a.spec.GetDisplayName(), Keyed: a.spec.GetKeyed(),
		SignupUrl: a.spec.GetSignupUrl(), KeyEnv: a.spec.GetKeyEnv(), SignsIn: a.spec.GetSignsIn()}
	ways := p.ways(stand)
	if a.spec.GetDetectedOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED || a.spec.GetLocal() || len(ways) == 0 {
		return p.finishAdd(j, a, nil)
	}
	target := Target{Profile: p.profile, Provider: stand, Hub: p.hub, Next: j.Show,
		Keep: func(ctx context.Context, fields map[string]any, identity string) (*providerv1.Account, error) {
			return p.keepNew(ctx, a, fields, identity)
		},
		Done: func(account *providerv1.Account) tea.Cmd { return p.finishAdd(j, a, account) },
	}
	return p.wayStep(j, "How do you want to connect "+a.spec.GetDisplayName()+"?", ways, target)
}

// Writes the provider being added once, with its address, and keeps the credential in its first account; the provider goes again when
// the account can't be kept, so nothing half made stays
func (p *providers) keepNew(ctx context.Context, a *adding, fields map[string]any, identity string) (*providerv1.Account, error) {
	if a.made == nil {
		added, err := p.service.Add(ctx, &providerv1.AddRequest{Profile: p.profile, Spec: a.spec.GetName(), BaseUrl: a.address})
		if err != nil {
			return nil, err
		}
		a.made = added.GetProvider()
	}
	account, err := connectAccount(ctx, p, a.made.GetName(), "", "", identity, fields)
	if err != nil {
		p.service.Remove(ctx, &providerv1.RemoveRequest{Profile: p.profile, Provider: a.made.GetName()})
		a.made = nil
		return nil, err
	}
	return account, nil
}

// Ends the job of adding a provider: writes it when no account was kept for it, shows it in hand and says what was made
func (p *providers) finishAdd(j *job, a *adding, account *providerv1.Account) tea.Cmd {
	return tui.Call(p.hub, func() (*providerv1.Provider, error) {
		if a.made != nil {
			return a.made, nil
		}
		added, err := p.service.Add(p.kit.Env.Ctx, &providerv1.AddRequest{Profile: p.profile, Spec: a.spec.GetName(), BaseUrl: a.address})
		return added.GetProvider(), err
	}, func(made *providerv1.Provider) tea.Cmd {
		done := "Added " + made.GetDisplayName() + "."
		if account != nil {
			done = "Added " + made.GetDisplayName() + " with the account " + account.GetTitle() + "."
		}
		return tea.Batch(j.End(), p.hub.Done(done), p.load(made.GetName()), p.options.Load(nil))
	})
}

// Adds an account to the provider as a job: how to connect, then a name when another account connects the same way, then that way's own
// step. The account exists only once it is connected
func (p *providers) addAccount(entry *providerv1.Provider) tea.Cmd {
	ways := p.ways(entry)
	if len(ways) == 0 {
		return p.hub.Say(entry.GetDisplayName()+" takes no account of its own.", cards.NoteInfo)
	}
	j := newJob(p.kit, p.hub, "Add an account")
	target := Target{Profile: p.profile, Provider: entry, Hub: p.hub, Next: j.Show}
	target.Done = func(account *providerv1.Account) tea.Cmd {
		return tea.Batch(j.End(), p.hub.Done("Added the account "+account.GetTitle()+" to "+entry.GetDisplayName()+"."),
			p.reload(entry.GetName(), func() tea.Cmd { return p.inHand(entry.GetName(), account.GetName()) }))
	}
	return p.wayStep(j, "How do you want to connect the account?", ways, target)
}

// Step that picks the way to connect, skipped when there is one; then the name of the account when another account of the provider
// connects that way already; then the way's own step
func (p *providers) wayStep(j *job, intro string, ways []way, target Target) tea.Cmd {
	take := func(w way) tea.Cmd {
		if target.Account != nil || !sameWay(target.Provider, w) {
			return w.Open(p.kit, p.kept(target, ""))
		}
		fields := []cards.Field{{ID: "name", Label: "Name", Help: "The name you know this account by, such as Work or Personal.", Check: func(value string) string {
			if config.Truthy(strings.TrimSpace(value)) {
				return ""
			}
			return "Give the account a name."
		}}}
		return j.Ask("Name this account, since "+target.Provider.GetDisplayName()+" has one that connects the same way.", fields, "",
			func(values map[string]string) tea.Cmd {
				return w.Open(p.kit, p.kept(target, strings.TrimSpace(values["name"])))
			})
	}
	if len(ways) == 1 {
		return take(ways[0])
	}
	t := p.kit.Env.Theme
	var list *cards.List
	list = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   j.title,
		Glyph:   t.Glyphs.Models,
		Key:     "providers:ways",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Intro:   func() []string { return []string{intro} },
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			rows := make([][]string, len(ways))
			for i, w := range ways {
				rows[i] = []string{w.Title, render.Dim(w.about, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			if list.Shows(1) || ways[i].about == "" {
				return nil
			}
			return []string{ways[i].about}
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "next"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{"hub.use": func(_ *cards.List, i int) tea.Cmd { return take(ways[i]) }},
	})
	list.ToggleDetail()
	return j.Show(list)
}

// The target with the name the person gave and the keeping of its credential, in an account of the provider unless the job keeps it
func (p *providers) kept(target Target, title string) Target {
	target.Title = title
	if target.Keep == nil {
		target.Keep = func(ctx context.Context, fields map[string]any, identity string) (*providerv1.Account, error) {
			name := ""
			if target.Account != nil {
				name = target.Account.GetName()
			}
			return connectAccount(ctx, p, target.Provider.GetName(), name, title, identity, fields)
		}
	}
	return target
}

// Whether an account of the provider connects the way given already, so a new one asks its name: a key, kept or read from a variable, or
// a sign-in
func sameWay(entry *providerv1.Provider, w way) bool {
	return slices.ContainsFunc(entry.GetAccounts(), func(account *providerv1.Account) bool {
		switch {
		case w.key:
			return account.GetAuth() != "oauth" && account.GetProgram() == "" && account.GetKeyEnv() == ""
		case w.variable:
			return account.GetKeyEnv() != ""
		}
		return account.GetAuth() == "oauth" || account.GetProgram() != ""
	})
}

// Ways to connect an account of the provider: every way a connector registered for its kind, then pasting an API key and naming the
// variable that holds one when its accounts keep a key
func (p *providers) ways(entry *providerv1.Provider) []way {
	var ways []way
	for _, connector := range p.connectors {
		if connector.Spec == entry.GetSpec() {
			for _, w := range connector.Ways {
				about := "Sign in with your browser"
				if !strings.HasPrefix(w.Kind, credentials.OAuth) {
					about = ""
				}
				ways = append(ways, way{Way: w, about: about})
			}
		}
	}
	if entry.GetKeyed() {
		ways = append(ways, way{Way: Way{Title: "API key", Open: func(_ *tui.Kit, target Target) tea.Cmd { return p.key(target) }}, key: true,
			about: "Paste a key from your " + entry.GetDisplayName() + " account"},
			way{Way: Way{Title: "Key from a variable", Open: func(_ *tui.Kit, target Target) tea.Cmd { return p.linked(target) }}, variable: true,
				about: "Read the key from an environment variable you set yourself"})
	}
	return ways
}

// Way to connect an account, what it does, and whether it is the API key or the variable of this page
type way struct {
	Way
	about         string
	key, variable bool
}

// Form of the API key of the account as a step, kept once sent
func (p *providers) key(target Target) tea.Cmd {
	help := "Paste the whole key."
	if url := target.Provider.GetSignupUrl(); url != "" {
		help = "Get one at " + url + "."
	}
	if target.Account != nil && target.Account.GetCredentialOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED {
		help += " The key kept now is replaced."
	}
	field := cards.Field{ID: "key", Label: "API key", Kind: cards.FieldSecret, Help: help, Check: func(value string) string {
		if strings.TrimSpace(value) == "" {
			return "Paste the key."
		}
		return ""
	}}
	return p.keepStep(target, "Paste the API key of your "+target.Provider.GetDisplayName()+" account.", field, func(value string) map[string]any {
		return map[string]any{providerconfig.APIKeyField: value}
	})
}

// Form of the variable an account reads its key from as a step, kept once sent
func (p *providers) linked(target Target) tea.Cmd {
	example := "PROVIDER_API_KEY"
	if names := target.Provider.GetKeyEnv(); len(names) > 0 {
		example = names[0]
	}
	field := cards.Field{ID: "key", Label: "Variable", Help: "The environment variable that holds the key, such as " + example + ".",
		Check: func(value string) string {
			if !config.EnvName(strings.TrimSpace(value)) {
				return "A variable name has only letters, digits and underscores, and starts with a letter."
			}
			return ""
		}}
	return p.keepStep(target, "Name the variable that holds the API key of your "+target.Provider.GetDisplayName()+" account.", field,
		func(value string) map[string]any { return map[string]any{providerconfig.KeyEnv: value} })
}

// Step of one typed field whose value the target keeps, its error said under the field
func (p *providers) keepStep(target Target, intro string, field cards.Field, fields func(value string) map[string]any) tea.Cmd {
	var form *cards.Form
	form = cards.NewForm(p.kit.Env, cards.FormSpec{
		Title:  "Connect",
		Intro:  []string{intro},
		Fields: []cards.Field{field},
		Submit: "Connect",
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			form.SetBusy(true)
			value := strings.TrimSpace(values[field.ID])
			return tui.Async(func() func() tea.Cmd {
				account, err := target.Keep(p.kit.Env.Ctx, fields(value), "")
				return func() tea.Cmd {
					form.SetBusy(false)
					if err != nil {
						form.SetError(field.ID, tui.Reason(err))
						return nil
					}
					return target.Done(account)
				}
			})
		},
	})
	if target.Next == nil {
		return target.Hub.Push(form)
	}
	return target.Next(form)
}

// Keeps a credential in an account of the provider, the one named or a new one the server names after the title, who signed in or the
// way it signs in; each field as YAML, nil ones left out
func connectAccount(ctx context.Context, p *providers, provider, name, title, identity string, fields map[string]any) (*providerv1.Account, error) {
	request := &providerv1.ConnectRequest{Profile: p.profile, Provider: provider, Account: name, Title: title, Identity: identity,
		Fields: map[string]string{}}
	for field, value := range fields {
		if value == nil {
			continue
		}
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		request.Fields[field] = strings.TrimSuffix(string(encoded), "\n")
	}
	response, err := p.service.Connect(ctx, request)
	if err != nil {
		return nil, err
	}
	return response.GetAccount(), nil
}
