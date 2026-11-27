// The autofill vault screen: the logins, cards and addresses the agent may fill in, the password managers it reads and where the vault
// key lives
package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1/vaultv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const about = "Secrets are encrypted and the agent never sees them; it fills them in for you on that site only."

// Words of the kinds and fields of items
var (
	kinds  = map[string]string{"login": "login", "payment": "card", "address": "address"}
	fields = map[string]string{"identifier_type": "signs in with", "identifier": "sign in name", "password": "password", "otp_secret": "authenticator key",
		"card_number": "card number", "cardholder_name": "name on the card", "exp_month": "expiry month", "exp_year": "expiry year", "cvc": "security code",
		"billing_postal_code": "billing postal code", "address_line1": "address line 1", "address_line2": "address line 2", "city": "city",
		"state": "state or region", "postal_code": "postal code", "country": "country"}
)

func word(table map[string]string, key string) string {
	if w, ok := table[key]; ok {
		return w
	}
	return strings.ReplaceAll(key, "_", " ")
}

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := vaultv1connect.NewVaultServiceClient(conn)
	return []tui.Screen{{
		ID:    "vault",
		Title: "Autofill vault",
		Short: "logins, cards and addresses the agent may fill in for you",
		Slash: []string{"vault"},
		Open: func(kit *tui.Kit, _ string) tea.Cmd {
			v := &screen{kit: kit, service: service}
			return v.open()
		},
	}}
}

// One row: an item, a password manager, or the key of the profile's own vault
type row struct {
	item   *vaultv1.Item
	source *vaultv1.Source
}

type screen struct {
	kit     *tui.Kit
	service vaultv1connect.VaultServiceClient
	hub     *cards.Hub
	list    *cards.List
	state   *vaultv1.ListResponse
	rows    []row
	loaded  bool
	err     error
}

func (v *screen) open() tea.Cmd {
	t := v.kit.Env.Theme
	v.list = cards.NewList(v.kit.Env, cards.ListSpec{
		Title:   "Autofill vault",
		Glyph:   t.Glyphs.Keys,
		Key:     "vault",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "kind", Max: 20}, {Title: "name", Max: 32}, {Title: "sign in name", Max: 32}, {Title: "site", Flex: 1},
			{Title: "source", Max: 16}},
		Yield:  []int{4, 3, 2},
		Rows:   v.cells,
		Detail: v.detail,
		Banner: func() string {
			if v.state == nil || len(v.state.GetLocked()) == 0 {
				return ""
			}
			return render.Warm(strings.Join(v.state.GetLocked(), ", ")+" is locked; the agent asks you to unlock it when it needs a login.", t)
		},
		Empty: func() string { return tui.Waiting(v.loaded, v.err, "") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var out []wm.Hint
			if r, ok := v.at(i); ok {
				switch {
				case r.item != nil && r.item.GetLocal():
					out = append(out, wm.Hint{Action: "hub.remove"})
				case r.source != nil && r.source.GetState() != "missing":
					out = append(out, wm.Hint{Action: "hub.use", Label: "open"})
				case r.item == nil && r.source == nil:
					out = append(out, wm.Hint{Action: "hub.use", Label: "open"})
				}
			}
			return append(out, wm.Hint{Action: "hub.add"})
		},
		Free: map[string]bool{"hub.add": true, "hub.refresh": true},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.refresh": func(*cards.List, int) tea.Cmd { return v.load() },
			"hub.use":     func(_ *cards.List, i int) tea.Cmd { return v.use(i) },
			"hub.add":     func(*cards.List, int) tea.Cmd { return v.add() },
			"hub.remove":  func(l *cards.List, i int) tea.Cmd { return v.remove(l, i) },
		},
		OnFocus: func(*cards.List) tea.Cmd { return v.load() },
	})
	v.hub = v.kit.Hub("vault", v.list)
	return tea.Batch(v.kit.WM.ShowOverlay(v.hub), v.load())
}

// Shows what the service answered: the items, then the password managers, then the key
func (v *screen) show(state *vaultv1.ListResponse) {
	v.state, v.loaded, v.err = state, true, nil
	v.rows = v.rows[:0]
	for _, item := range state.GetItems() {
		v.rows = append(v.rows, row{item: item})
	}
	for _, source := range state.GetSources() {
		v.rows = append(v.rows, row{source: source})
	}
	v.rows = append(v.rows, row{})
	v.list.Refresh()
}

func (v *screen) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		state, err := v.service.List(v.kit.Env.Ctx, &vaultv1.ListRequest{})
		return func() tea.Cmd {
			if err != nil {
				v.loaded, v.err = true, err
				v.list.Refresh()
				return nil
			}
			v.show(state)
			return nil
		}
	})
}

// Runs a change in the background and shows the vault after it, back on the list with the note saying what was done
func (v *screen) change(work func() (string, error)) tea.Cmd {
	back := v.hub.PopTo(v.list)
	v.hub.Busy("Saving…")
	return tea.Batch(back, tui.Async(func() func() tea.Cmd {
		note, err := work()
		state, listErr := v.service.List(v.kit.Env.Ctx, &vaultv1.ListRequest{})
		return func() tea.Cmd {
			if listErr == nil {
				v.show(state)
			}
			if err != nil {
				return v.hub.Fail(tui.Reason(err))
			}
			return v.hub.Done(note)
		}
	}))
}

func (v *screen) at(i int) (row, bool) {
	if i < 0 || i >= len(v.rows) {
		return row{}, false
	}
	return v.rows[i], true
}

// Whether the profile uses a password manager, in words
func state(source *vaultv1.Source) string {
	switch source.GetState() {
	case "on":
		if source.GetLocked() {
			return "in use, locked"
		}
		return "in use"
	case "off":
		return "turned off"
	}
	return "not installed"
}

// Where the key of the profile's own vault lives, in words
func (v *screen) keyPlace() string {
	switch v.state.GetKey().GetStorage() {
	case "file":
		return "in a file beside the vault"
	case "keychain":
		return "in the keychain of this computer"
	}
	return "none yet; made with the first item"
}

func (v *screen) cells() [][]string {
	out := make([][]string, len(v.rows))
	for i, r := range v.rows {
		switch {
		case r.item != nil:
			out[i] = []string{word(kinds, r.item.GetKind()), r.item.GetLabel(), r.item.GetIdentifier(), r.item.GetOrigin(), r.item.GetSource()}
		case r.source != nil:
			out[i] = []string{"password manager", r.source.GetDisplayName(), state(r.source), "", ""}
		default:
			out[i] = []string{"vault key", v.keyPlace(), "", "", ""}
		}
	}
	return out
}

func (v *screen) detail(i int) []string {
	r, ok := v.at(i)
	switch {
	case !ok:
		return nil
	case r.item != nil:
		out := []string{r.item.GetLabel() + " · " + word(kinds, r.item.GetKind()), "kept in " + r.item.GetSource(), "handle " + r.item.GetHandle()}
		if r.item.GetOrigin() != "" {
			out = append(out, r.item.GetOrigin())
		}
		return append(out, about)
	case r.source != nil:
		return []string{r.source.GetDisplayName() + " · " + state(r.source),
			"A password manager is used once its tool is installed and signed in; the agent asks you to unlock it when it needs a login."}
	}
	key := v.state.GetKey()
	out := []string{"vault key · " + v.keyPlace()}
	if key.GetPath() != "" {
		out = append(out, key.GetPath())
	}
	if key.GetService() != "" {
		out = append(out, key.GetService()+" · "+key.GetAccount())
	}
	if key.GetKeychainLeft() != "" {
		out = append(out, "The keychain copy ("+key.GetKeychainLeft()+") could not be removed yet; move the key again when the keychain can be reached.")
	}
	return out
}

// Something a row can do, picked from a list of them
type action struct {
	label, does string
	run         func() tea.Cmd
}

func (v *screen) actions(title string, acts []action) tea.Cmd {
	if len(acts) == 0 {
		return nil
	}
	choices := make([]tui.Choice, len(acts))
	for i, act := range acts {
		choices[i] = tui.Choice{ID: act.label, Title: act.label, Detail: act.does}
	}
	return v.kit.Pick(v.hub, title, "run", choices, "", func(choice tui.Choice) tea.Cmd {
		for _, act := range acts {
			if act.label == choice.ID {
				return act.run()
			}
		}
		return nil
	})
}

// Opens a row: what a password manager can do, where the key can move, nothing for an item
func (v *screen) use(i int) tea.Cmd {
	r, ok := v.at(i)
	switch {
	case !ok || r.item != nil:
		return nil
	case r.source != nil:
		return v.source(r.source)
	}
	key := v.state.GetKey()
	var acts []action
	if key.GetStorage() != "keychain" {
		acts = append(acts, action{"Move the key to the keychain", "backups of this profile then hold the vault without its key", func() tea.Cmd { return v.moveKey("keychain") }})
	}
	if key.GetStorage() != "file" {
		acts = append(acts, action{"Move the key to a file", "the key sits beside the vault, so the profile can move to another computer", func() tea.Cmd { return v.moveKey("file") }})
	}
	return v.actions("vault key", acts)
}

// What a password manager can do: be used or not, and unlocked or locked again while it is used
func (v *screen) source(source *vaultv1.Source) tea.Cmd {
	if source.GetState() == "missing" {
		return nil
	}
	name, title := source.GetName(), source.GetDisplayName()
	toggle := func(on bool) func() tea.Cmd {
		return func() tea.Cmd {
			return v.change(func() (string, error) {
				_, err := v.service.SetSource(v.kit.Env.Ctx, &vaultv1.SetSourceRequest{Name: name, On: on})
				return "Saved " + title + ".", err
			})
		}
	}
	if source.GetState() != "on" {
		return toggle(true)()
	}
	acts := []action{{"Stop using it", "the agent no longer reads its logins", toggle(false)}}
	if source.GetLocked() {
		acts = append([]action{{"Unlock", "type the master password, kept for this run of icebath until idle", func() tea.Cmd { return v.unlock(name, title) }}}, acts...)
	} else {
		acts = append([]action{{"Lock", "forget its session; the agent asks again when it needs a login", func() tea.Cmd {
			return v.change(func() (string, error) {
				_, err := v.service.Lock(v.kit.Env.Ctx, &vaultv1.LockRequest{Name: name})
				return title + " is locked.", err
			})
		}}}, acts...)
	}
	return v.actions(title, acts)
}

// Asks for the master password of a password manager and unlocks it
func (v *screen) unlock(name, title string) tea.Cmd {
	return v.kit.Ask(v.hub, title, cards.Field{Label: "master password", Kind: cards.FieldSecret}, "Unlock", func(master string) tea.Cmd {
		if master == "" {
			return nil
		}
		return v.change(func() (string, error) {
			_, err := v.service.Unlock(v.kit.Env.Ctx, &vaultv1.UnlockRequest{Name: name, MasterPassword: master})
			return title + " is unlocked.", err
		})
	})
}

func (v *screen) moveKey(to string) tea.Cmd {
	return v.change(func() (string, error) {
		key, err := v.service.MoveKey(v.kit.Env.Ctx, &vaultv1.MoveKeyRequest{To: to})
		if err != nil {
			return "", err
		}
		if key.GetKeychainLeft() != "" {
			return "The keychain copy (" + key.GetKeychainLeft() + ") could not be removed yet; move the key again when the keychain can be reached.", nil
		}
		return "The vault key moved.", nil
	})
}

// Saves a new item: what it is, then a form of its label, its site and the fields of that kind
func (v *screen) add() tea.Cmd {
	if v.state == nil {
		return nil
	}
	choices := make([]tui.Choice, len(v.state.GetKinds()))
	for i, kind := range v.state.GetKinds() {
		choices[i] = tui.Choice{ID: kind.GetKind(), Title: word(kinds, kind.GetKind())}
	}
	return v.kit.Pick(v.hub, "What to save", "pick", choices, "", func(choice tui.Choice) tea.Cmd {
		var kind *vaultv1.Kind
		for _, k := range v.state.GetKinds() {
			if k.GetKind() == choice.ID {
				kind = k
			}
		}
		formFields := []cards.Field{
			{ID: "label", Label: "label", Help: "A name for you, like GitHub work account.", Required: true},
			{ID: "origin", Label: "site", Help: "Address the item may be filled in on, like https://github.com.", Required: true},
		}
		for _, f := range kind.GetFields() {
			field := cards.Field{ID: "value:" + f.GetKey(), Label: word(fields, f.GetKey()), Required: f.GetRequired()}
			switch {
			case len(f.GetChoices()) > 0:
				field.Kind = cards.FieldChoice
				for _, c := range f.GetChoices() {
					field.Choices = append(field.Choices, cards.Choice{Value: c, Label: c})
				}
				field.Value = f.GetChoices()[0]
			case f.GetSecret():
				field.Kind = cards.FieldSecret
			}
			formFields = append(formFields, field)
		}
		var form *cards.Form
		form = cards.NewForm(v.kit.Env, cards.FormSpec{Title: choice.Title, Intro: []string{about}, Submit: "Save", Fields: formFields,
			Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
				request := &vaultv1.AddRequest{Kind: kind.GetKind(), Label: values["label"], Origin: values["origin"], Values: map[string]string{}}
				for _, f := range kind.GetFields() {
					if value := values["value:"+f.GetKey()]; value != "" {
						request.Values[f.GetKey()] = value
					}
				}
				label := strings.TrimSpace(values["label"])
				return tea.Batch(v.hub.Close(form), v.change(func() (string, error) {
					_, err := v.service.Add(v.kit.Env.Ctx, request)
					return "Added " + label + ".", err
				}))
			}})
		return v.hub.Push(form)
	})
}

// Removes the item of a row from the profile's own vault after a confirmation, an item of a password manager staying where it is kept
func (v *screen) remove(l *cards.List, i int) tea.Cmd {
	r, ok := v.at(i)
	if !ok || r.item == nil {
		return nil
	}
	item := r.item
	if !item.GetLocal() {
		return v.hub.Fail("It is kept in " + item.GetSource() + "; change it there.")
	}
	l.Confirm("Remove "+item.GetLabel()+" from the vault?", func() tea.Cmd {
		return v.change(func() (string, error) {
			_, err := v.service.Remove(v.kit.Env.Ctx, &vaultv1.RemoveRequest{Handle: item.GetHandle()})
			return "Removed " + item.GetLabel() + ".", err
		})
	})
	return nil
}
