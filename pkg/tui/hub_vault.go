package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the autofill vault screen
const vaultContext = "tui.vault"

// Keys of the autofill vault screen, the shared hub keys and none of its own
func registerVaultKeys(km *keys.Map) {
	if keys.ContextDocs[vaultContext] == "" {
		if err := keys.AddContext(vaultContext, "autofill vault screen"); err != nil {
			panic(err)
		}
	}
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "vault", order: 88, title: txt.HubsTitleVault, short: txt.HubsShortVault, slash: "vault",
		open: func(a *app, _ string) tea.Cmd { return a.openVault() }, keys: registerVaultKeys})
}

// One row of the autofill vault screen: an item, a password manager, or the key of the local vault
type vaultRow struct {
	item   *tuigateway.VaultItemRow
	source *tuigateway.VaultSourceRow
}

// State of the open autofill vault screen: what the service last answered and the hub it shows in
type vaultHub struct {
	a       *app
	h       *cards.Hub
	list    *cards.List
	rows    []vaultRow
	locked  []string
	key     tuigateway.VaultKeyInfo
	kinds   []tuigateway.VaultKind
	loading bool
}

// Autofill vault screen: the logins, cards and addresses the agent may fill in, the password managers it reads and where the vault key lives
func (a *app) openVault() tea.Cmd {
	if line, absent := linktier.Refusal("/vault", linktier.Extras); absent {
		return wm.Notify(line, wm.Warn)
	}
	v := &vaultHub{a: a, loading: true}
	v.list = cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleVault), Glyph: a.toolkit.Glyphs.Settings, Key: "tui:vault", Context: vaultContext, Shared: []string{hubContext}, Primary: "hub.use",
		Columns: []render.Column{{Title: tr(txt.HubsColKind), Max: 20}, {Title: tr(txt.HubsColName), Max: 32}, {Title: tr(txt.HubsVaultColIdentifier), Max: 32},
			{Title: tr(txt.HubsVaultColSite), Flex: 1}, {Title: tr(txt.HubsColSource), Max: 16}},
		Yield:  []int{4, 3, 2},
		Rows:   v.cells,
		Detail: v.detail,
		Banner: func() string {
			if len(v.locked) == 0 {
				return ""
			}
			return tr(txt.HubsVaultLocked, strings.Join(v.locked, ", "))
		},
		Empty: func() string { return tr(txt.HubsLoading) },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			out := []wm.Hint{}
			if row, ok := v.at(i); ok {
				switch {
				case row.item != nil && row.item.Local:
					out = append(out, wm.Hint{Action: "hub.remove"})
				case row.source != nil && row.source.State != "missing":
					out = append(out, wm.Hint{Action: "hub.toggle", Label: strings.ToLower(vaultSourceLabel(*row.source))})
				case row.item == nil && row.source == nil:
					out = append(out, wm.Hint{Action: "hub.use", Label: tr(txt.HintsLabelOpen)})
				}
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return v.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return v.open(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return v.add() },
			"hub.toggle":  func(_ *cards.List, i int) tea.Cmd { return v.toggle(i) },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return v.remove(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return v.load() },
	})
	v.h = a.newHub("tui:vault", v.list)
	return a.wm.ShowOverlay(v.h)
}

// Shows what the service answered: the items, then the password managers, then the key
func (v *vaultHub) show(r *tuigateway.VaultListResult) {
	v.loading, v.locked, v.key, v.kinds = false, r.Locked, r.Key, r.Kinds
	v.rows = v.rows[:0]
	for i := range r.Items {
		v.rows = append(v.rows, vaultRow{item: &r.Items[i]})
	}
	for i := range r.Sources {
		v.rows = append(v.rows, vaultRow{source: &r.Sources[i]})
	}
	v.rows = append(v.rows, vaultRow{})
	v.list.Refresh()
}

// Reads the vault again, a failure staying in the hub
func (v *vaultHub) load() tea.Cmd {
	return hubCall(v.a, v.h, "", tuigateway.VaultList, tuigateway.VaultListParams{}, func(r *tuigateway.VaultListResult) tea.Cmd {
		v.show(r)
		return nil
	})
}

// Sends one change and shows the vault it answers, back on the list, the note saying what was done
func (v *vaultHub) change(params tuigateway.VaultChangeParams, done func(r *tuigateway.VaultListResult) string) tea.Cmd {
	back := v.h.PopTo(v.list)
	return tea.Batch(back, hubCall(v.a, v.h, tr(txt.HubsProfilesSaving), tuigateway.VaultChange, params, func(r *tuigateway.VaultListResult) tea.Cmd {
		v.show(r)
		return v.h.Done(done(r))
	}))
}

// Row at i, false past the rows
func (v *vaultHub) at(i int) (vaultRow, bool) {
	if i < 0 || i >= len(v.rows) {
		return vaultRow{}, false
	}
	return v.rows[i], true
}

// Word for a kind of item
func vaultKindLabel(kind string) string { return trOr("hubs.vault.kind."+kind, kind) }

// Label of a field of an item
func vaultFieldLabel(key string) string {
	return trOr("hubs.vault.field."+key, strings.ReplaceAll(key, "_", " "))
}

// Whether the agent uses a password manager, in words
func vaultSourceState(source tuigateway.VaultSourceRow) string {
	switch source.State {
	case "on":
		return tr(txt.HubsVaultStateOn)
	case "off":
		return tr(txt.HubsVaultStateOff)
	}
	return tr(txt.HubsVaultStateMissing)
}

// Word for the action that starts or stops using a password manager
func vaultSourceLabel(source tuigateway.VaultSourceRow) string {
	if source.State == "on" {
		return tr(txt.HubsVaultSourceOff)
	}
	return tr(txt.HubsVaultSourceOn)
}

// Where the key of the local vault lives, in words
func (v *vaultHub) keyPlace() string {
	switch v.key.Storage {
	case "file":
		return tr(txt.HubsVaultKeyFile)
	case "keychain":
		return tr(txt.HubsVaultKeyKeychain)
	}
	return tr(txt.HubsVaultKeyNone)
}

// Cells of every row
func (v *vaultHub) cells() [][]string {
	out := make([][]string, len(v.rows))
	for i, row := range v.rows {
		switch {
		case row.item != nil:
			out[i] = []string{vaultKindLabel(row.item.Kind), row.item.Label, row.item.Identifier, row.item.Origin, row.item.Source}
		case row.source != nil:
			out[i] = []string{tr(txt.HubsVaultKindManager), row.source.DisplayName, vaultSourceState(*row.source), "", ""}
		default:
			out[i] = []string{tr(txt.HubsVaultKindKey), v.keyPlace(), "", "", ""}
		}
	}
	return out
}

// Lines of a row's detail pane
func (v *vaultHub) detail(i int) []string {
	row, ok := v.at(i)
	switch {
	case !ok:
		return nil
	case row.item != nil:
		out := []string{row.item.Label + " · " + vaultKindLabel(row.item.Kind), tr(txt.HubsVaultDetailSource, row.item.Source), tr(txt.HubsVaultDetailHandle, row.item.Handle)}
		if row.item.Origin != "" {
			out = append(out, row.item.Origin)
		}
		return append(out, tr(txt.HubsVaultAbout))
	case row.source != nil:
		return []string{row.source.DisplayName + " · " + vaultSourceState(*row.source), tr(txt.HubsVaultSourceAbout)}
	}
	out := []string{tr(txt.HubsVaultKindKey) + " · " + v.keyPlace()}
	if v.key.Path != "" {
		out = append(out, v.key.Path)
	}
	if v.key.Service != "" {
		out = append(out, v.key.Service+" · "+v.key.Account)
	}
	if v.key.KeychainLeft != "" {
		out = append(out, tr(txt.HubsVaultKeyLeft, v.key.KeychainLeft))
	}
	return out
}

// Opens a row: the actions that move the key for the key row, on or off for a password manager, nothing for an item
func (v *vaultHub) open(i int) tea.Cmd {
	row, ok := v.at(i)
	switch {
	case !ok || row.item != nil:
		return nil
	case row.source != nil:
		return v.toggle(i)
	}
	return v.a.hubActions(v.h, tr(txt.HubsVaultKindKey), func() []string { return v.detail(i) }, func() []hubAction {
		var acts []hubAction
		if v.key.Storage != "keychain" {
			acts = append(acts, hubAction{label: tr(txt.HubsVaultKeyToKeychain), does: tr(txt.HubsVaultKeyToKeychainDoes), run: func() tea.Cmd { return v.moveKey("keychain") }})
		}
		if v.key.Storage != "file" {
			acts = append(acts, hubAction{label: tr(txt.HubsVaultKeyToFile), does: tr(txt.HubsVaultKeyToFileDoes), run: func() tea.Cmd { return v.moveKey("file") }})
		}
		return acts
	})
}

// Moves the vault key to the keychain or back to its file
func (v *vaultHub) moveKey(to string) tea.Cmd {
	return v.change(tuigateway.VaultChangeParams{Verb: "key", To: to}, func(r *tuigateway.VaultListResult) string {
		if r.Key.KeychainLeft != "" {
			return tr(txt.HubsVaultKeyLeft, r.Key.KeychainLeft)
		}
		return tr(txt.HubsVaultKeyMoved)
	})
}

// Saves a new item: what it is, then a form of its label, its site and the fields of that kind
func (v *vaultHub) add() tea.Cmd {
	choices := make([]hubChoice, len(v.kinds))
	for i, kind := range v.kinds {
		choices[i] = hubChoice{id: kind.Kind, title: vaultKindLabel(kind.Kind)}
	}
	return v.a.hubPick(v.h, tr(txt.HubsVaultPickKind), choices, "", func(c hubChoice) tea.Cmd {
		var kind tuigateway.VaultKind
		for _, k := range v.kinds {
			if k.Kind == c.id {
				kind = k
			}
		}
		fields := []cards.Field{
			{ID: "label", Label: tr(txt.HubsVaultFieldLabel), Help: tr(txt.HubsVaultLabelHelp), Required: true},
			{ID: "origin", Label: tr(txt.HubsVaultColSite), Help: tr(txt.HubsVaultSiteHelp), Required: true},
		}
		for _, f := range kind.Fields {
			field := cards.Field{ID: "value:" + f.Key, Label: vaultFieldLabel(f.Key), Required: f.Required}
			switch {
			case len(f.Choices) > 0:
				field.Kind = cards.FieldChoice
				for _, choice := range f.Choices {
					field.Choices = append(field.Choices, cards.Choice{Value: choice, Label: trOr("hubs.vault.id."+choice, choice)})
				}
				field.Value = f.Choices[0]
			case f.Secret:
				field.Kind = cards.FieldSecret
			}
			fields = append(fields, field)
		}
		spec := cards.FormSpec{Title: c.title, Intro: []string{tr(txt.HubsVaultAbout)}, Submit: tr(txt.HubsProfilesCreateSubmit), Fields: fields}
		return v.a.hubForm(v.h, spec, func(_ *cards.Form, values map[string]string) tea.Cmd {
			params := tuigateway.VaultChangeParams{Verb: "add", Kind: kind.Kind, Label: values["label"], Origin: values["origin"], Values: map[string]string{}}
			for _, f := range kind.Fields {
				if value := values["value:"+f.Key]; value != "" {
					params.Values[f.Key] = value
				}
			}
			label := strings.TrimSpace(values["label"])
			return v.change(params, func(*tuigateway.VaultListResult) string { return tr(txt.HubsFallbackAdded, label) })
		})
	})
}

// Starts or stops using the password manager of a row
func (v *vaultHub) toggle(i int) tea.Cmd {
	row, ok := v.at(i)
	if !ok || row.source == nil || row.source.State == "missing" {
		return nil
	}
	source := *row.source
	params := tuigateway.VaultChangeParams{Verb: "source", Source: source.Name, On: source.State != "on"}
	return v.change(params, func(*tuigateway.VaultListResult) string { return tr(txt.HubsCronSaved, source.DisplayName) })
}

// Removes the item of a row from the local vault after a confirmation, an item of a password manager staying where it is kept
func (v *vaultHub) remove(l *cards.List, i int) tea.Cmd {
	row, ok := v.at(i)
	if !ok || row.item == nil {
		return nil
	}
	item := *row.item
	if !item.Local {
		return v.h.Fail(tr(txt.HubsVaultNotLocal, item.Source))
	}
	l.Confirm(tr(txt.HubsVaultRemoveConfirm, item.Label), func() tea.Cmd {
		return v.change(tuigateway.VaultChangeParams{Verb: "remove", Handle: item.Handle}, func(*tuigateway.VaultListResult) string {
			return tr(txt.HubsFallbackRemoved, item.Label)
		})
	})
	return nil
}
