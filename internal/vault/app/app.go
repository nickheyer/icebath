// The autofill vault screen: the logins, cards and addresses the agent may fill in, the password managers it reads and where the vault
// key lives
package app

import (
	"cmp"
	"slices"
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

// What the vault is and why it is safe, said at the top of the screen and of the form of a new item
const about = "The agent never sees what you keep here. It fills each item in for you, and only on its own site."

// Words of the kinds and fields of items and the values a field takes
var (
	kinds    = map[string]string{"login": "login", "payment": "card", "address": "address"}
	headings = map[string]string{"login": "Logins", "payment": "Cards", "address": "Addresses"}
	purposes = map[string]string{"login": "A sign-in name and password for a site", "payment": "A card the agent may pay with on a site",
		"address": "An address the agent may type into forms"}
	fields = map[string]string{"identifier_type": "Signs in with", "identifier": "Sign-in name", "password": "Password", "otp_secret": "Authenticator key",
		"card_number": "Card number", "cardholder_name": "Name on the card", "exp_month": "Expiry month", "exp_year": "Expiry year", "cvc": "Security code",
		"billing_postal_code": "Billing postal code", "address_line1": "Address line 1", "address_line2": "Address line 2", "city": "City",
		"state": "State or region", "postal_code": "Postal code", "country": "Country"}
	values = map[string]string{"email": "Email address", "username": "Username", "phone": "Phone number"}
)

// Order of the kinds of items on the screen
var order = []string{"login", "payment", "address"}

// Heading of the password managers and of the vault's own key
const (
	managersHeading = "Password managers"
	keyHeading      = "This vault"
)

// Word of a key in a table, the key with its underscores as spaces and a capital first letter when the table has none
func word(table map[string]string, key string) string {
	if w, ok := table[key]; ok {
		return w
	}
	text := strings.ReplaceAll(key, "_", " ")
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

type screens struct{}

func Register(host *app.App) {
	host.Register(screens{})
}

func (screens) Screens(conn *connect.Client) []tui.Screen {
	service := vaultv1connect.NewVaultServiceClient(conn)
	return []tui.Screen{{
		ID:    "vault",
		Group: tui.GroupSecrets,
		Order: 20,
		Title: "Autofill vault",
		Short: "Keep the logins and cards the agent may fill in",
		Slash: []string{"vault"},
		Open: func(kit *tui.Kit, _ string) tea.Cmd {
			v := &screen{kit: kit, service: service}
			return v.open()
		},
	}}
}

// One row: an item, a password manager, the row that locks every password manager, or the key of the profile's own vault
type row struct {
	item   *vaultv1.Item
	source *vaultv1.Source
	lock   bool
}

type screen struct {
	kit     *tui.Kit
	service vaultv1connect.VaultServiceClient
	hub     *cards.Hub
	list    *cards.List
	state   *vaultv1.ListResponse
	rows    []row
	// Lists stacked over the first one, refreshed whenever the vault is read
	views  []*cards.List
	loaded bool
	err    error
}

func (v *screen) open() tea.Cmd {
	t := v.kit.Env.Theme
	v.list = cards.NewList(v.kit.Env, cards.ListSpec{
		Title:   "Autofill vault",
		Glyph:   t.Glyphs.Keys,
		Key:     "vault",
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search the vault",
		Intro: func() []string {
			return []string{"Logins, cards and addresses the agent may fill into web pages. " + about}
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows:    v.cells,
		Group:   v.group,
		Words: func(i int) string {
			if item := v.rows[i].item; item != nil {
				return word(kinds, item.GetKind()) + " " + item.GetSource()
			}
			return ""
		},
		Detail: v.detail,
		Banner: func() string {
			if v.state == nil || len(v.state.GetLocked()) == 0 {
				return ""
			}
			locked := v.state.GetLocked()
			verb := " is"
			if len(locked) > 1 {
				verb = " are"
			}
			return render.Warm(join(locked)+verb+" locked. The agent asks you to unlock it when it needs a login.", t)
		},
		Empty: func() string {
			switch {
			case v.err != nil:
				return "The vault could not be read: " + strings.TrimSuffix(tui.Reason(v.err), ".") + ". " + v.kit.Env.Keys.Display("hub.refresh") + " tries again."
			case !v.loaded:
				return "Reading the vault…"
			}
			return ""
		},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			var out []wm.Hint
			if r, ok := v.at(i); ok {
				switch {
				case r.item != nil && r.item.GetLocal():
					out = append(out, wm.Hint{Action: "hub.remove"})
				case r.lock:
					out = append(out, wm.Hint{Action: "hub.use", Label: "lock all"})
				case r.source != nil && r.source.GetState() == "on":
					out = append(out, wm.Hint{Action: "hub.use", Label: "open"})
				case r.source != nil && r.source.GetState() == "off":
					out = append(out, wm.Hint{Action: "hub.use", Label: "turn on"})
				case r.item == nil && r.source == nil:
					out = append(out, wm.Hint{Action: "hub.use", Label: "open"})
				}
			}
			return append(out, wm.Hint{Action: "hub.add", Label: "add item"})
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
	v.list.ToggleDetail()
	v.hub = v.kit.Hub("vault", v.list)
	return tea.Batch(v.kit.WM.ShowOverlay(v.hub), v.load())
}

// Names joined as a sentence says them: A, B and C
func join(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// Shows what the service answered: the items by kind, then the password managers with the row that locks them all when several are open,
// then the key
func (v *screen) show(state *vaultv1.ListResponse) {
	v.state, v.loaded, v.err = state, true, nil
	v.rows = v.rows[:0]
	items := slices.Clone(state.GetItems())
	slices.SortStableFunc(items, func(a, b *vaultv1.Item) int {
		place := func(kind string) int {
			if at := slices.Index(order, kind); at >= 0 {
				return at
			}
			return len(order)
		}
		return cmp.Compare(place(a.GetKind()), place(b.GetKind()))
	})
	for _, item := range items {
		v.rows = append(v.rows, row{item: item})
	}
	open := 0
	for _, source := range state.GetSources() {
		v.rows = append(v.rows, row{source: source})
		if source.GetState() == "on" && !source.GetLocked() {
			open++
		}
	}
	if open > 1 {
		v.rows = append(v.rows, row{lock: true})
	}
	v.rows = append(v.rows, row{})
	v.list.Refresh()
	for _, view := range v.views {
		view.Refresh()
	}
}

func (v *screen) load() tea.Cmd {
	return tui.Async(func() func() tea.Cmd {
		state, err := v.service.List(v.kit.Env.Ctx, &vaultv1.ListRequest{})
		return func() tea.Cmd {
			if err != nil {
				v.loaded, v.err = true, err
				v.rows = nil
				v.list.Refresh()
				return nil
			}
			v.show(state)
			return nil
		}
	})
}

// Runs a change in the background and shows the vault after it, back on the list with the note saying what was done; a note that warns
// stays until the next action
func (v *screen) change(busy string, work func() (string, bool, error)) tea.Cmd {
	back := v.hub.PopTo(v.list)
	v.hub.Busy(busy)
	return tea.Batch(back, tui.Async(func() func() tea.Cmd {
		note, warn, err := work()
		state, listErr := v.service.List(v.kit.Env.Ctx, &vaultv1.ListRequest{})
		return func() tea.Cmd {
			if listErr == nil {
				v.show(state)
			}
			switch {
			case err != nil:
				return v.hub.Fail(strings.TrimSuffix(tui.Reason(err), ".") + ".")
			case warn:
				return v.hub.Fail(note)
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

// Heading of the group a row is listed under
func (v *screen) group(i int) string {
	r := v.rows[i]
	switch {
	case r.item != nil:
		return word(headings, r.item.GetKind())
	case r.source != nil, r.lock:
		return managersHeading
	}
	return keyHeading
}

// Whether the profile uses a password manager, in words
func state(source *vaultv1.Source) string {
	switch source.GetState() {
	case "on":
		if source.GetLocked() {
			return "In use, locked"
		}
		return "In use"
	case "off":
		return "Off"
	}
	return "Not installed"
}

// Where the key of the profile's own vault lives, in words
func (v *screen) keyPlace() string {
	switch v.state.GetKey().GetStorage() {
	case "file":
		return "Kept in a file beside the vault"
	case "keychain":
		return "Kept in the keychain of this computer"
	}
	return "Made with the first item you save"
}

// Where a new vault keeps its key, in words
func keyGoes(configured string) string {
	if configured == "keychain" {
		return "the keychain of this computer"
	}
	return "a file beside the vault"
}

func (v *screen) cells() [][]string {
	t := v.kit.Env.Theme
	out := make([][]string, len(v.rows))
	for i, r := range v.rows {
		switch {
		case r.item != nil:
			var parts []string
			for _, part := range []string{r.item.GetIdentifier(), r.item.GetOrigin()} {
				if part != "" {
					parts = append(parts, part)
				}
			}
			if !r.item.GetLocal() {
				parts = append(parts, "from "+r.item.GetSource())
			}
			out[i] = []string{r.item.GetLabel(), render.Dim(strings.Join(parts, " · "), t)}
		case r.lock:
			out[i] = []string{"Lock every password manager", render.Dim("The agent asks again when it needs a login", t)}
		case r.source != nil:
			out[i] = []string{r.source.GetDisplayName(), render.Dim(state(r.source), t)}
		default:
			out[i] = []string{"Vault key", render.Dim(v.keyPlace(), t)}
		}
	}
	return out
}

func (v *screen) detail(i int) []string {
	t := v.kit.Env.Theme
	r, ok := v.at(i)
	switch {
	case !ok:
		return nil
	case r.item != nil:
		item := r.item
		lines := []string{"A " + word(kinds, item.GetKind()) + " the agent may fill in."}
		if origin := item.GetOrigin(); origin != "" {
			lines[0] = "A " + word(kinds, item.GetKind()) + " the agent may fill in on " + origin + "."
		}
		if identifier := item.GetIdentifier(); identifier != "" && item.GetKind() == "login" {
			lines = append(lines, "Signs in as "+identifier+".")
		}
		if item.GetOtp() {
			lines = append(lines, "Fills the authenticator code too.")
		}
		if item.GetLocal() {
			lines = append(lines, render.Dim("Kept in this vault.", t))
		} else {
			lines = append(lines, render.Dim("Kept in "+item.GetSource()+", so you change it there.", t))
		}
		return lines
	case r.lock:
		return []string{"Locks every password manager that is open now. The agent asks you to unlock one when it needs a login."}
	case r.source != nil:
		source := r.source
		lines := []string{"The logins kept in " + source.GetDisplayName() + ", which the agent may fill in while it is in use."}
		switch {
		case source.GetState() == "missing":
			lines = append(lines, render.Warm("Its program is not installed. Install it and sign in, then turn it on here.", t))
		case source.GetState() == "off":
			lines = append(lines, "Off. Turn it on to let the agent use its logins.")
		case source.GetLocked():
			lines = append(lines, render.Warm("Locked. The agent asks you to unlock it when it needs a login.", t))
		default:
			lines = append(lines, render.Good("Unlocked for this run of icebath, until it sits idle.", t))
		}
		return lines
	}
	key := v.state.GetKey()
	lines := []string{"The key that encrypts what this vault keeps. " + v.keyPlace() + "."}
	if key.GetPath() != "" {
		lines = append(lines, render.Dim("File: "+key.GetPath(), t))
	}
	if key.GetService() != "" {
		lines = append(lines, render.Dim("Keychain entry: "+key.GetService()+", account "+key.GetAccount(), t))
	}
	if key.GetStorage() == "" && key.GetConfigured() != "" {
		lines = append(lines, render.Dim("It will be kept in "+keyGoes(key.GetConfigured())+".", t))
	}
	if keychain := key.GetKeychain(); keychain != "" {
		lines = append(lines, render.Dim("Keychain: "+keychain, t))
	}
	if left := key.GetKeychainLeft(); left != "" {
		lines = append(lines, render.Warm("The keychain still holds a copy under "+left+". Move the key again once the keychain can be reached.", t))
	}
	return lines
}

// Something a row can do, on a list of them: its name, what it does, the word on its button and the run of it
type action struct {
	label, does, verb string
	run               func() tea.Cmd
}

func (v *screen) actions(title string, detail func() []string, acts func() []action) tea.Cmd {
	t := v.kit.Env.Theme
	var shown []action
	var list *cards.List
	list = cards.NewList(v.kit.Env, cards.ListSpec{
		Title:   title,
		Glyph:   t.Glyphs.Keys,
		Key:     "vault:actions:" + title,
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{}, {Flex: 1, Whole: true}},
		Rows: func() [][]string {
			shown = acts()
			rows := make([][]string, len(shown))
			for i, act := range shown {
				rows[i] = []string{act.label, render.Dim(act.does, t)}
			}
			return rows
		},
		Detail: func(i int) []string {
			var lines []string
			if i >= 0 && i < len(shown) && !list.Shows(1) {
				lines = append(lines, shown[i].does, "")
			}
			return append(lines, detail()...)
		},
		Empty: func() string { return "There is nothing to do here now." },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: shown[i].verb}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd { return shown[i].run() },
		},
	})
	list.ToggleDetail()
	v.views = append(v.views, list)
	return v.hub.Push(list)
}

// Opens a row: what a password manager can do, turning one on that is off, locking them all, where the key can move; nothing for an item
func (v *screen) use(i int) tea.Cmd {
	r, ok := v.at(i)
	switch {
	case !ok || r.item != nil:
		return nil
	case r.lock:
		return v.change("Locking every password manager…", func() (string, bool, error) {
			_, err := v.service.Lock(v.kit.Env.Ctx, &vaultv1.LockRequest{})
			return "Every password manager is locked. The agent asks again when it needs a login.", false, err
		})
	case r.source != nil:
		return v.source(r.source)
	}
	return v.actions("Vault key", func() []string { return v.detail(len(v.rows) - 1) }, func() []action {
		key := v.state.GetKey()
		var acts []action
		if key.GetStorage() != "keychain" {
			acts = append(acts, action{"Move it to the keychain", "Backups of this profile then hold the vault without its key", "move",
				func() tea.Cmd { return v.moveKey("keychain") }})
		}
		if key.GetStorage() != "file" {
			acts = append(acts, action{"Move it to a file", "The key sits beside the vault, so the profile can move to another computer", "move",
				func() tea.Cmd { return v.moveKey("file") }})
		}
		return acts
	})
}

// What a password manager can do: be turned on when it is off; once on, be unlocked or locked again and turned off
func (v *screen) source(source *vaultv1.Source) tea.Cmd {
	if source.GetState() == "missing" {
		return nil
	}
	name, title := source.GetName(), source.GetDisplayName()
	toggle := func(on bool) tea.Cmd {
		busy, done := "Turning "+title+" on…", "The agent now uses the logins of "+title+"."
		if !on {
			busy, done = "Turning "+title+" off…", "The agent no longer uses the logins of "+title+"."
		}
		return v.change(busy, func() (string, bool, error) {
			_, err := v.service.SetSource(v.kit.Env.Ctx, &vaultv1.SetSourceRequest{Name: name, On: on})
			return done, false, err
		})
	}
	if source.GetState() != "on" {
		return toggle(true)
	}
	current := func() *vaultv1.Source {
		for _, now := range v.state.GetSources() {
			if now.GetName() == name {
				return now
			}
		}
		return source
	}
	return v.actions(title, func() []string {
		for i, r := range v.rows {
			if r.source != nil && r.source.GetName() == name {
				return v.detail(i)
			}
		}
		return nil
	}, func() []action {
		var acts []action
		if current().GetLocked() {
			acts = append(acts, action{"Unlock", "Type the master password, kept for this run of icebath until it sits idle", "unlock", func() tea.Cmd {
				return v.unlock(name, title)
			}})
		} else {
			acts = append(acts, action{"Lock", "Forget its session. The agent asks again when it needs a login", "lock", func() tea.Cmd {
				return v.change("Locking "+title+"…", func() (string, bool, error) {
					_, err := v.service.Lock(v.kit.Env.Ctx, &vaultv1.LockRequest{Name: name})
					return title + " is locked. The agent asks you to unlock it when it needs a login.", false, err
				})
			}})
		}
		return append(acts, action{"Turn off", "The agent no longer reads its logins", "turn off", func() tea.Cmd { return toggle(false) }})
	})
}

// Asks for the master password of a password manager and unlocks it
func (v *screen) unlock(name, title string) tea.Cmd {
	field := cards.Field{Label: "Master password", Kind: cards.FieldSecret, Help: "Kept for this run of icebath until it sits idle."}
	return v.kit.Ask(v.hub, "Unlock", field, "Unlock", func(master string) tea.Cmd {
		if master == "" {
			return nil
		}
		return v.change("Unlocking "+title+"…", func() (string, bool, error) {
			_, err := v.service.Unlock(v.kit.Env.Ctx, &vaultv1.UnlockRequest{Name: name, MasterPassword: master})
			return title + " is unlocked until icebath sits idle.", false, err
		})
	})
}

func (v *screen) moveKey(to string) tea.Cmd {
	return v.change("Moving the vault key…", func() (string, bool, error) {
		key, err := v.service.MoveKey(v.kit.Env.Ctx, &vaultv1.MoveKeyRequest{To: to})
		if err != nil {
			return "", false, err
		}
		if left := key.GetKeychainLeft(); left != "" {
			return "The key moved, but the keychain still holds a copy under " + left + ". Move the key again once the keychain can be reached.", true, nil
		}
		if to == "keychain" {
			return "The vault key is in the keychain of this computer now.", false, nil
		}
		return "The vault key is in a file beside the vault now.", false, nil
	})
}

// Saves a new item: what it is, then a form of its name, its site and the fields of that kind, kept open with what was typed when the save
// is refused
func (v *screen) add() tea.Cmd {
	if v.state == nil {
		return nil
	}
	choices := make([]tui.Choice, len(v.state.GetKinds()))
	for i, kind := range v.state.GetKinds() {
		choices[i] = tui.Choice{ID: kind.GetKind(), Title: word(headings, kind.GetKind()), Detail: purposes[kind.GetKind()]}
	}
	return v.kit.Pick(v.hub, "New item", "choose", choices, "", func(choice tui.Choice) tea.Cmd {
		var kind *vaultv1.Kind
		for _, k := range v.state.GetKinds() {
			if k.GetKind() == choice.ID {
				kind = k
			}
		}
		what := word(kinds, kind.GetKind())
		formFields := []cards.Field{
			{ID: "label", Label: "Name", Help: "A name for you, such as GitHub work account.", Required: true},
			{ID: "origin", Label: "Site", Help: "The address it may be filled in on, such as https://github.com.", Required: true,
				Check: func(value string) string {
					if value = strings.TrimSpace(value); !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
						return "Start the address with https://."
					}
					return ""
				}},
		}
		for _, f := range kind.GetFields() {
			field := cards.Field{ID: "value:" + f.GetKey(), Label: word(fields, f.GetKey()), Required: f.GetRequired()}
			switch {
			case len(f.GetChoices()) > 0:
				field.Kind = cards.FieldChoice
				for _, c := range f.GetChoices() {
					field.Choices = append(field.Choices, cards.Choice{Value: c, Label: word(values, c)})
				}
				field.Value = f.GetChoices()[0]
			case f.GetSecret():
				field.Kind = cards.FieldSecret
			}
			formFields = append(formFields, field)
		}
		var form *cards.Form
		form = cards.NewForm(v.kit.Env, cards.FormSpec{Title: strings.ToUpper(what[:1]) + what[1:], Intro: []string{about}, Submit: "Save", Fields: formFields,
			Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
				label := strings.TrimSpace(values["label"])
				request := &vaultv1.AddRequest{Kind: kind.GetKind(), Label: label, Origin: strings.TrimSpace(values["origin"]), Values: map[string]string{}}
				for _, f := range kind.GetFields() {
					if value := values["value:"+f.GetKey()]; value != "" {
						request.Values[f.GetKey()] = value
					}
				}
				form.SetBusy(true)
				v.hub.Busy("Saving " + label + "…")
				return tui.Async(func() func() tea.Cmd {
					_, err := v.service.Add(v.kit.Env.Ctx, request)
					return func() tea.Cmd {
						form.SetBusy(false)
						if err != nil {
							return v.hub.Fail(label + " was not saved: " + strings.TrimSuffix(tui.Reason(err), ".") + ".")
						}
						return tea.Batch(v.hub.Close(form), v.hub.Done("Saved "+label+". The agent may fill it in on "+request.GetOrigin()+"."), v.load())
					}
				})
			}})
		return v.hub.Push(form)
	})
}

// Removes the item of a row from the profile's own vault after a question, an item of a password manager staying where it is kept
func (v *screen) remove(l *cards.List, i int) tea.Cmd {
	r, ok := v.at(i)
	if !ok || r.item == nil {
		return nil
	}
	item := r.item
	if !item.GetLocal() {
		return v.hub.Fail(item.GetLabel() + " is kept in " + item.GetSource() + ", so remove it there.")
	}
	l.Confirm("Remove "+item.GetLabel()+"? The agent can no longer fill it in.", func() tea.Cmd {
		return v.change("Removing "+item.GetLabel()+"…", func() (string, bool, error) {
			_, err := v.service.Remove(v.kit.Env.Ctx, &vaultv1.RemoveRequest{Handle: item.GetHandle()})
			return "Removed " + item.GetLabel() + ".", false, err
		})
	})
	return nil
}
