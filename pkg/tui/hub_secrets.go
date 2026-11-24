package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the secret stores screen
const secretsContext = "tui.secrets"

// Longest a change to a secret store may take, its helper program installed and the store asked over the network
const secretsWait = 5 * time.Minute

// Address of the Bitwarden cloud in the European Union
const bitwardenEU = "https://vault.bitwarden.eu"

// Keys of the secret stores screen, the shared hub keys and none of its own
func registerSecretsKeys(km *keys.Map) {
	if keys.ContextDocs[secretsContext] == "" {
		if err := keys.AddContext(secretsContext, "secret stores screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "secrets", order: 87, title: txt.HubsTitleSecrets, short: txt.HubsShortSecrets, slash: "secrets",
		open: func(a *app, _ string) tea.Cmd { return a.openSecrets() }, keys: registerSecretsKeys})
}

// State of the open secret stores screen: both stores as last read and the hub they show in
type secretsHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	state   tuigateway.SecretsStateResult
	loading bool
}

// Secret stores screen: Bitwarden and 1Password as sources of keys, then every variable 1Password fills
func (a *app) openSecrets() tea.Cmd {
	s := &secretsHub{a: a, loading: true}
	s.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleSecrets), Glyph: a.toolkit.Glyphs.Settings, Key: "tui:secrets", Context: secretsContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColName), Max: 36}, {Title: tr(txt.HubsColStatus), Max: 24}, {Title: tr(txt.HubsSecretsColFrom), Flex: 1}},
		Yield:   []int{2, 1},
		Rows:    s.cells,
		Detail:  s.detail,
		Empty:   func() string { return tr(txt.HubsLoading) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			switch {
			case i == 0 || i == 1:
				out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)})
			case i > 1:
				out = append(out, wm.Hint{Action: "hub.remove"})
			}
			return append(out, wm.Hint{Action: "hub.add", Label: strings.ToLower(tr(txt.HubsSecretsMap))})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return s.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return s.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return s.mapReference() },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return s.unmap(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return s.load() },
	})
	s.h = a.newHub("tui:secrets", s.list)
	return a.wm.ShowOverlay(s.h)
}

// Reads the stores again, a failure staying in the hub
func (s *secretsHub) load() tea.Cmd {
	return hubCall(s.a, s.h, "", tuigateway.SecretsState, tuigateway.SecretsStateParams{}, func(r *tuigateway.SecretsStateResult) tea.Cmd {
		s.loading, s.state = false, *r
		s.list.Refresh()
		return nil
	})
}

// Sends one change, which may install a helper program and ask the store over the network, and shows the stores after it, back on the list
func (s *secretsHub) change(params tuigateway.SecretsChangeParams, then func(r *tuigateway.SecretsChangeResult) tea.Cmd) tea.Cmd {
	back := s.h.PopTo(s.list)
	s.h.Busy(tr(txt.HubsProfilesSaving))
	return tea.Batch(back, callWithin(s.a.gw, tuigateway.SecretsChange, params, secretsWait, func(r *tuigateway.SecretsChangeResult, err error) tea.Cmd {
		if err != nil {
			return s.h.Fail(rpcErrorMessage(err))
		}
		s.h.Quiet()
		s.loading, s.state = false, r.State
		s.list.Refresh()
		return then(r)
	}))
}

// Whether a store is on, in a word
func secretsStoreState(enabled bool) string {
	if enabled {
		return tr(txt.HubsVaultStateOn)
	}
	return tr(txt.HubsVaultStateOff)
}

// Cells of every row: the two stores, then each variable 1Password fills with its reference
func (s *secretsHub) cells() [][]string {
	if s.loading {
		return nil
	}
	bitwarden, onePassword := s.state.Bitwarden, s.state.OnePassword
	out := [][]string{
		{"Bitwarden", secretsStoreState(bitwarden.Enabled), bitwarden.ProjectID},
		{"1Password", secretsStoreState(onePassword.Enabled), onePassword.Account},
	}
	for _, ref := range onePassword.References {
		out = append(out, []string{ref.Env, "", ref.Reference})
	}
	return out
}

// Words for a token variable and whether it holds a token
func secretsTokenLine(name string, set bool) string {
	if set {
		return tr(txt.HubsSecretsTokenSet, name)
	}
	return tr(txt.HubsSecretsTokenMissing, name)
}

// Words for a helper program and its version, or that it is not installed
func secretsHelperLine(binary, version string) string {
	if binary == "" {
		return tr(txt.HubsSecretsHelperMissing)
	}
	return strings.TrimSpace(binary + " " + version)
}

// Lines of a row's detail pane
func (s *secretsHub) detail(i int) []string {
	bitwarden, onePassword := s.state.Bitwarden, s.state.OnePassword
	switch {
	case s.loading || i < 0:
		return nil
	case i == 0:
		out := []string{"Bitwarden · " + secretsStoreState(bitwarden.Enabled), tr(txt.HubsSecretsBitwardenAbout), secretsTokenLine(bitwarden.TokenEnv, bitwarden.TokenSet)}
		if bitwarden.ProjectID != "" {
			out = append(out, tr(txt.HubsSecretsProject, bitwarden.ProjectID))
		}
		if bitwarden.ServerURL != "" {
			out = append(out, bitwarden.ServerURL)
		}
		return append(out, secretsHelperLine(bitwarden.Binary, bitwarden.Version))
	case i == 1:
		out := []string{"1Password · " + secretsStoreState(onePassword.Enabled), tr(txt.HubsSecretsOnePasswordAbout), secretsTokenLine(onePassword.TokenEnv, onePassword.TokenSet)}
		if onePassword.Account != "" {
			out = append(out, onePassword.Account)
		}
		return append(out, secretsHelperLine(onePassword.Binary, onePassword.Version))
	case i-2 < len(onePassword.References):
		ref := onePassword.References[i-2]
		return []string{ref.Env, ref.Reference}
	}
	return nil
}

// Page of what the next start would do with each secret a store hands over
func (s *secretsHub) preview(title string, r *tuigateway.SecretsChangeResult) tea.Cmd {
	t := s.a.toolkit
	lines := make([]string, 0, len(r.Rows)+len(r.Warnings)+1)
	for _, row := range r.Rows {
		switch row.Action {
		case "export":
			lines = append(lines, render.Good(tr(txt.HubsSecretsRowExport, row.Name), t))
		case "override":
			lines = append(lines, render.Good(tr(txt.HubsSecretsRowOverride, row.Name), t))
		case "skip_set":
			lines = append(lines, render.Dim(tr(txt.HubsSecretsRowSkipSet, row.Name), t))
		case "skip_token":
			lines = append(lines, render.Dim(tr(txt.HubsSecretsRowSkipToken, row.Name), t))
		default:
			lines = append(lines, render.Bad(tr(txt.HubsSecretsRowUnresolved, row.Name), t))
		}
	}
	if len(r.Rows) == 0 {
		lines = append(lines, tr(txt.HubsSecretsNothing))
	}
	for _, warning := range r.Warnings {
		lines = append(lines, warningLine(warning))
	}
	return s.a.hubLinesPage(s.h, title, lines)
}

// Opens a row: the actions of a store, nothing for a variable
func (s *secretsHub) open(i int) tea.Cmd {
	switch i {
	case 0:
		return s.a.hubActions(s.h, "Bitwarden", func() []string { return s.detail(0) }, func() []hubAction {
			acts := []hubAction{{label: tr(txt.HubsSecretsSetUp), does: tr(txt.HubsSecretsBitwardenSetUpDoes), run: s.setUpBitwarden}}
			if s.state.Bitwarden.Enabled {
				acts = append(acts,
					hubAction{label: tr(txt.HubsSecretsNewToken), does: tr(txt.HubsSecretsNewTokenDoes), run: func() tea.Cmd { return s.newToken("bitwarden", "Bitwarden") }},
					hubAction{label: tr(txt.HubsSecretsCheck), does: tr(txt.HubsSecretsCheckDoes), run: func() tea.Cmd { return s.check("bitwarden", "Bitwarden") }},
					hubAction{label: tr(txt.HubsSecretsTurnOff), does: tr(txt.HubsSecretsTurnOffDoes), run: func() tea.Cmd { return s.turnOff("bitwarden", "Bitwarden") }})
			}
			return acts
		})
	case 1:
		return s.a.hubActions(s.h, "1Password", func() []string { return s.detail(1) }, func() []hubAction {
			acts := []hubAction{{label: tr(txt.HubsSecretsSetUp), does: tr(txt.HubsSecretsOnePasswordSetUpDoes), run: s.setUpOnePassword},
				{key: "hub.add", label: tr(txt.HubsSecretsMap), does: tr(txt.HubsSecretsMapDoes), run: s.mapReference}}
			if s.state.OnePassword.Enabled {
				acts = append(acts,
					hubAction{label: tr(txt.HubsSecretsNewToken), does: tr(txt.HubsSecretsNewTokenDoes), run: func() tea.Cmd { return s.newToken("onepassword", "1Password") }},
					hubAction{label: tr(txt.HubsSecretsCheck), does: tr(txt.HubsSecretsCheckDoes), run: func() tea.Cmd { return s.check("onepassword", "1Password") }},
					hubAction{label: tr(txt.HubsSecretsTurnOff), does: tr(txt.HubsSecretsTurnOffDoes), run: func() tea.Cmd { return s.turnOff("onepassword", "1Password") }})
			}
			return acts
		})
	}
	return nil
}

// Sets Bitwarden up: a form of the access token and the region, then the projects that token sees to pick one from, then one pull of its secrets
func (s *secretsHub) setUpBitwarden() tea.Cmd {
	current := s.state.Bitwarden
	region := cards.Field{ID: "region", Label: tr(txt.HubsSecretsRegion), Kind: cards.FieldChoice, Value: "us", Choices: []cards.Choice{
		{Value: "us", Label: tr(txt.HubsSecretsRegionUS)}, {Value: "eu", Label: tr(txt.HubsSecretsRegionEU)}, {Value: "own", Label: tr(txt.HubsSecretsRegionOwn)}}}
	own := cards.Field{ID: "server", Label: tr(txt.HubsSecretsServer), Help: tr(txt.HubsSecretsServerHelp)}
	switch current.ServerURL {
	case "":
	case bitwardenEU:
		region.Value = "eu"
	default:
		region.Value, own.Value = "own", current.ServerURL
	}
	token := cards.Field{ID: "token", Label: tr(txt.HubsSecretsToken), Kind: cards.FieldSecret, Help: tr(txt.HubsSecretsBitwardenTokenHelp), Required: !current.TokenSet}
	if current.TokenSet {
		token.Placeholder = tr(txt.HubsMemorySecretKeep)
	}
	spec := cards.FormSpec{Title: "Bitwarden", Intro: []string{tr(txt.HubsSecretsBitwardenAbout)}, Submit: tr(txt.HubsSecretsNext), Fields: []cards.Field{token, region, own}}
	return s.a.hubForm(s.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		server := ""
		switch values["region"] {
		case "eu":
			server = bitwardenEU
		case "own":
			server = strings.TrimSpace(values["server"])
		}
		newToken := strings.TrimSpace(values["token"])
		s.h.Busy(tr(txt.HubsLoading))
		params := tuigateway.SecretsProjectsParams{Token: newToken, ServerURL: server}
		return callWithin(s.a.gw, tuigateway.SecretsProjects, params, secretsWait, func(r *tuigateway.SecretsProjectsResult, err error) tea.Cmd {
			switch {
			case err != nil:
				return s.h.Fail(rpcErrorMessage(err))
			case len(r.Projects) == 0:
				return s.h.Fail(tr(txt.HubsSecretsNoProjects))
			}
			s.h.Quiet()
			choices := make([]hubChoice, len(r.Projects))
			for i, project := range r.Projects {
				choices[i] = hubChoice{id: project.ID, title: project.Name, detail: project.ID}
			}
			return s.a.hubPick(s.h, tr(txt.HubsSecretsPickProject), choices, current.ProjectID, func(c hubChoice) tea.Cmd {
				change := tuigateway.SecretsChangeParams{Source: "bitwarden", Verb: "setup", Token: newToken, ServerURL: server, ProjectID: c.id}
				return s.change(change, func(r *tuigateway.SecretsChangeResult) tea.Cmd { return s.preview("Bitwarden", r) })
			})
		})
	})
}

// Sets 1Password up: a form of the account, a service account token, the variable that holds it and where the op program lives
func (s *secretsHub) setUpOnePassword() tea.Cmd {
	current := s.state.OnePassword
	token := cards.Field{ID: "token", Label: tr(txt.HubsSecretsToken), Kind: cards.FieldSecret, Help: tr(txt.HubsSecretsOnePasswordTokenHelp)}
	if current.TokenSet {
		token.Placeholder = tr(txt.HubsMemorySecretKeep)
	}
	spec := cards.FormSpec{Title: "1Password", Intro: []string{tr(txt.HubsSecretsOnePasswordAbout)}, Submit: tr(txt.HubsSecretsSetUp), Fields: []cards.Field{
		{ID: "account", Label: tr(txt.HubsColAccount), Help: tr(txt.HubsSecretsAccountHelp), Value: current.Account}, token,
		{ID: "token_env", Label: tr(txt.HubsSecretsTokenEnv), Value: current.TokenEnv, More: true},
		{ID: "binary", Label: tr(txt.HubsSecretsOpPath), Help: tr(txt.HubsSecretsOpPathHelp), More: true},
	}}
	return s.a.hubForm(s.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		change := tuigateway.SecretsChangeParams{Source: "onepassword", Verb: "setup", Account: strings.TrimSpace(values["account"]), Token: strings.TrimSpace(values["token"]),
			TokenEnv: strings.TrimSpace(values["token_env"]), BinaryPath: strings.TrimSpace(values["binary"])}
		return s.change(change, func(r *tuigateway.SecretsChangeResult) tea.Cmd {
			if r.Identity == "" {
				return s.h.Done(tr(txt.HubsSecretsOnNotSignedIn))
			}
			return s.h.Done(tr(txt.HubsSecretsOnSignedIn, r.Identity))
		})
	})
}

// Asks for a new token of a store, kept once the store accepts it
func (s *secretsHub) newToken(source, name string) tea.Cmd {
	return s.a.hubAsk(s.h, name, tr(txt.HubsSecretsToken), "", true, func(token string) tea.Cmd {
		if token == "" {
			return nil
		}
		return s.change(tuigateway.SecretsChangeParams{Source: source, Verb: "token", Token: token}, func(*tuigateway.SecretsChangeResult) tea.Cmd {
			return s.h.Done(tr(txt.HubsSecretsTokenKept, name))
		})
	})
}

// Asks a store for its secrets now and shows what the next start would do with each
func (s *secretsHub) check(source, name string) tea.Cmd {
	return s.change(tuigateway.SecretsChangeParams{Source: source, Verb: "check"}, func(r *tuigateway.SecretsChangeResult) tea.Cmd { return s.preview(name, r) })
}

// Turns a store off, its token and references staying
func (s *secretsHub) turnOff(source, name string) tea.Cmd {
	return s.change(tuigateway.SecretsChangeParams{Source: source, Verb: "off"}, func(*tuigateway.SecretsChangeResult) tea.Cmd {
		return s.h.Done(tr(txt.HubsCronSaved, name))
	})
}

// Form binding an environment variable to the 1Password reference it resolves from
func (s *secretsHub) mapReference() tea.Cmd {
	spec := cards.FormSpec{Title: tr(txt.HubsSecretsMap), Fields: []cards.Field{
		{ID: "env", Label: tr(txt.HubsSecretsVariable), Placeholder: "OPENAI_API_KEY", Required: true},
		{ID: "reference", Label: tr(txt.HubsSecretsReference), Placeholder: "op://Private/OpenAI/api key", Required: true},
	}}
	return s.a.hubForm(s.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
		env := strings.TrimSpace(values["env"])
		change := tuigateway.SecretsChangeParams{Source: "onepassword", Verb: "map", Env: env, Reference: strings.TrimSpace(values["reference"])}
		return s.change(change, func(*tuigateway.SecretsChangeResult) tea.Cmd { return s.h.Done(tr(txt.HubsCronSaved, env)) })
	})
}

// Takes the reference of the variable of a row away after a confirmation
func (s *secretsHub) unmap(l *cards.List, i int) tea.Cmd {
	refs := s.state.OnePassword.References
	if i < 2 || i-2 >= len(refs) {
		return nil
	}
	env := refs[i-2].Env
	l.Confirm(tr(txt.HubsSecretsUnmapConfirm, env), func() tea.Cmd {
		return s.change(tuigateway.SecretsChangeParams{Source: "onepassword", Verb: "unmap", Env: env}, func(*tuigateway.SecretsChangeResult) tea.Cmd {
			return s.h.Done(tr(txt.HubsFallbackRemoved, env))
		})
	})
	return nil
}
