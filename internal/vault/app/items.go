package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	"github.com/nickheyer/icebath/internal/vault"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// What a field is called and the line under it where it is typed, by its key; an identifier is called by what it is
var fieldWords = map[string]struct{ title, help string }{
	"label":               {"Name", "A name for you, such as GitHub work account."},
	"origin":              {"Site", "The address of the site it is filled on, such as https://github.com."},
	"identifier_type":     {"Signs in with", "What the login signs in with."},
	"email":               {"Email address", "The email address you sign in with, such as you@example.com."},
	"phone":               {"Phone number", "The phone number you sign in with, such as +1 555 0100."},
	"username":            {"Username", "The username you sign in with."},
	"password":            {"Password", "The password of the login. It is never shown again."},
	"otp_secret":          {"Authenticator key", "The setup key the site shows for two-step sign-in, or an otpauth:// link. It is never shown again."},
	"card_number":         {"Card number", "The number on the front of the card. It is never shown again."},
	"cardholder_name":     {"Name on the card", "The name on the card, such as Jane Doe."},
	"exp_month":           {"Expiry month", "The month the card runs out, such as 04."},
	"exp_year":            {"Expiry year", "The year the card runs out, such as 2029."},
	"cvc":                 {"Security code", "The three or four digits on the back of the card. It is never shown again."},
	"billing_postal_code": {"Billing postal code", "The postal code of the address the card bills to, such as 12345."},
	"address_line1":       {"Address line 1", "The street and number, such as 1 Main Street."},
	"address_line2":       {"Address line 2", "The flat, floor or building, such as Flat 4."},
	"city":                {"City", "The town or city, such as Springfield."},
	"state":               {"State or region", "The state, county or region, such as Oregon."},
	"postal_code":         {"Postal code", "The postal or ZIP code, such as 12345."},
	"country":             {"Country", "The country, such as United States."},
}

// Steps of the fields of a new item of each kind after its name and site, each a form of at most three fields under the line that says what
// it asks; a field of a kind no step names is asked after them, three at a time
var steps = map[string][]struct {
	intro string
	keys  []string
}{
	"login": {{"What you sign in with, and the password.", []string{"identifier", "password", "otp_secret"}}},
	"payment": {
		{"The card.", []string{"card_number", "cardholder_name", "billing_postal_code"}},
		{"When the card runs out, and its security code.", []string{"exp_month", "exp_year", "cvc"}},
	},
	"address": {
		{"The street and the town.", []string{"address_line1", "address_line2", "city"}},
		{"The region, the postal code and the country.", []string{"state", "postal_code", "country"}},
	},
}

// Title of a field, an identifier by what the login signs in with
func fieldTitle(key, identifier string) string {
	if key == "identifier" {
		key = identifier
	}
	if words, ok := fieldWords[key]; ok {
		return words.title
	}
	return key
}

// Field of a form for one field of an item, typed into: a secret one never holds what is kept, an optional one says what empty does
func formField(f *vaultv1.Field, identifier, value string) cards.Field {
	key := f.GetKey()
	words := fieldWords[key]
	if key == "identifier" {
		words = fieldWords[identifier]
	}
	field := cards.Field{ID: key, Label: words.title, Value: value, Required: f.GetRequired(), Help: words.help}
	if f.GetSecret() {
		field.Kind, field.Value = cards.FieldSecret, ""
	}
	if !f.GetRequired() {
		field.Help += " Leave it empty when there is none."
	}
	return field
}

// Opens the page of an item of this vault: its name, its site and every field of its kind as rows, a secret one as dots, each changed in a
// form of its own, then Remove
func (p *page) itemPage(listed *vaultv1.Item) tea.Cmd {
	handle := listed.GetHandle()
	t := p.kit.Env.Theme
	var item *vaultv1.ItemResponse
	var err error
	loaded := false
	var view *cards.List
	// Rows of the page: the name, the site, the fields of its kind, then Remove
	type fieldRow struct {
		key    string
		value  *vaultv1.Value
		remove bool
	}
	var shown []fieldRow
	read := func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			got, failed := p.service.Item(p.kit.Env.Ctx, &vaultv1.ItemRequest{Profile: p.profile, Handle: handle})
			return func() tea.Cmd {
				item, err, loaded = got, failed, true
				view.Refresh()
				return nil
			}
		})
	}
	identifier := func() string {
		for _, value := range item.GetValues() {
			if value.GetField().GetKey() == "identifier_type" {
				return value.GetValue()
			}
		}
		return "username"
	}
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   listed.GetLabel(),
		Glyph:   t.Glyphs.Keys,
		Key:     Screen + ":" + handle,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro: func() []string {
			if item == nil {
				return nil
			}
			return []string{withArticle(kinds[item.GetItem().GetKind()]) + " the agent may fill in on " + item.GetItem().GetOrigin() + "."}
		},
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = nil
			if item == nil {
				return nil
			}
			shown = append(shown, fieldRow{key: "label"}, fieldRow{key: "origin"})
			for _, value := range item.GetValues() {
				shown = append(shown, fieldRow{key: value.GetField().GetKey(), value: value})
			}
			shown = append(shown, fieldRow{remove: true})
			cells := make([][]string, len(shown))
			for i, r := range shown {
				switch {
				case r.remove:
					cells[i] = []string{"Remove", "", ""}
				case r.key == "label":
					cells[i] = []string{"Name", "", item.GetItem().GetLabel()}
				case r.key == "origin":
					cells[i] = []string{"Site", "", item.GetItem().GetOrigin()}
				default:
					cells[i] = []string{fieldTitle(r.key, identifier()), "", valueWords(r.value)}
				}
			}
			return cells
		},
		Detail: func(i int) []string {
			switch r := shown[i]; {
			case r.remove:
				return []string{"Take " + item.GetItem().GetLabel() + " out of the vault for good."}
			case r.key == "identifier":
				return []string{fieldWords[identifier()].help}
			}
			return []string{fieldWords[shown[i].key].help}
		},
		Empty: func() string { return tui.Waiting(loaded, err, "") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			if shown[i].remove {
				return []wm.Hint{{Action: "hub.use", Label: "remove"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				r := shown[i]
				save := func(value string) tea.Cmd {
					return p.changeField(item.GetItem(), r.key, value, func() tea.Cmd {
						// The page goes by the name of its item
						if r.key == "label" {
							view.Retitle(value)
						}
						return read()
					})
				}
				switch {
				case r.remove:
					question := cards.Question{Text: "Remove " + item.GetItem().GetLabel() + "? The agent can no longer fill it in, and what it holds is " +
						"gone for good.", Verb: "Remove", Deletes: true}
					l.Confirm(question, func() tea.Cmd {
						label := item.GetItem().GetLabel()
						p.hub.Busy("Removing " + label + "…")
						return tui.Call(p.hub, func() (*vaultv1.RemoveResponse, error) {
							return p.service.Remove(p.kit.Env.Ctx, &vaultv1.RemoveRequest{Profile: p.profile, Handle: handle})
						}, func(*vaultv1.RemoveResponse) tea.Cmd {
							return tea.Batch(p.hub.Done("Removed "+label+"."), p.hub.Close(view), p.load(nil))
						})
					})
					return nil
				case r.key == "label", r.key == "origin":
					field := cards.Field{Label: fieldWords[r.key].title, Help: fieldWords[r.key].help, Select: true, Value: item.GetItem().GetLabel()}
					if r.key == "origin" {
						field.Value = item.GetItem().GetOrigin()
					}
					return p.kit.Ask(p.hub, field.Label, field, "Save", save)
				case r.key == "identifier_type":
					return p.kit.PickFrom(p.hub, "Signs in with", []string{"What the login signs in with."}, "choose", identifierChoices(), identifier(),
						func(choice tui.Choice) tea.Cmd { return save(choice.ID) })
				}
				field := formField(r.value.GetField(), identifier(), r.value.GetValue())
				field.Select = true
				return p.askField(field, r.value.GetField().GetRequired(), save)
			},
		},
	})
	view.ToggleDetail()
	p.views.Add(view)
	return tea.Batch(p.hub.Push(view), read())
}

// Asks one field typed into, an optional one taking an empty answer as taking it away
func (p *page) askField(field cards.Field, required bool, save func(value string) tea.Cmd) tea.Cmd {
	if required {
		return p.kit.Ask(p.hub, field.Label, field, "Save", save)
	}
	return p.kit.AskAny(p.hub, field.Label, field, "Save", save)
}

// What a field holds as its row shows it: a secret as dots, Not set for nothing
func valueWords(value *vaultv1.Value) string {
	switch {
	case !value.GetSet():
		return "Not set"
	case value.GetField().GetSecret():
		return "••••••••"
	case value.GetField().GetKey() == "identifier_type":
		return fieldWords[value.GetValue()].title
	}
	return value.GetValue()
}

// What a login may sign in with, as a list to pick from
func identifierChoices() []tui.Choice {
	choices := make([]tui.Choice, len(vault.IdentifierTypes))
	for i, id := range vault.IdentifierTypes {
		choices[i] = tui.Choice{ID: id, Title: fieldWords[id].title}
	}
	return choices
}

// Changes one field of an item, saying what changed, then reads the item and the vault again
func (p *page) changeField(item *vaultv1.Item, field, value string, then func() tea.Cmd) tea.Cmd {
	p.hub.Busy("Saving…")
	return tui.Call(p.hub, func() (*vaultv1.AddResponse, error) {
		return p.service.Change(p.kit.Env.Ctx, &vaultv1.ChangeRequest{Profile: p.profile, Handle: item.GetHandle(), Field: field, Value: value})
	}, func(changed *vaultv1.AddResponse) tea.Cmd {
		title := fieldTitle(field, "")
		if field == "identifier" {
			title = "What it signs in with"
		}
		return tea.Batch(p.hub.Done("Changed "+strs.Lower(title)+" of "+changed.GetItem().GetLabel()+"."), then(), p.load(nil))
	})
}

// Adds an item in steps: its kind, its name and site, then its fields in forms of at most three, a login first picking what it signs in
// with. Nothing is saved until the last step is answered and the vault takes it
func (p *page) add() tea.Cmd {
	if p.state == nil {
		return nil
	}
	f := p.kit.Flow(p.hub, "Add an item")
	var choices []tui.Choice
	for _, kind := range p.state.GetKinds() {
		choices = append(choices, tui.Choice{ID: kind.GetKind(), Title: titles[kind.GetKind()], Detail: purposes[kind.GetKind()]})
	}
	return f.Pick("What to save.", choices, "", func(choice tui.Choice) tea.Cmd {
		var kind *vaultv1.Kind
		for _, k := range p.state.GetKinds() {
			if k.GetKind() == choice.ID {
				kind = k
			}
		}
		named := []cards.Field{
			{ID: "label", Label: "Name", Required: true, Help: fieldWords["label"].help},
			{ID: "origin", Label: "Site", Required: true, Help: fieldWords["origin"].help, Check: func(value string) string {
				if value = strings.TrimSpace(value); !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
					return "An address starts with https://."
				}
				return ""
			}},
		}
		return f.Ask("Which "+kinds[kind.GetKind()]+" it is and where it is filled in.", named, "", func(values map[string]string) tea.Cmd {
			request := &vaultv1.AddRequest{Profile: p.profile, Kind: kind.GetKind(), Label: strings.TrimSpace(values["label"]),
				Origin: strings.TrimSpace(values["origin"]), Values: map[string]string{}}
			if kind.GetKind() != "login" {
				return p.fields(f, "", request, batches(kind), 0)
			}
			return f.Pick("What the login signs in with.", identifierChoices(), "", func(choice tui.Choice) tea.Cmd {
				request.Values["identifier_type"] = choice.ID
				return p.fields(f, choice.ID, request, batches(kind), 0)
			})
		})
	})
}

// One form of the fields of a new item and the line over it
type batch struct {
	intro  string
	fields []*vaultv1.Field
}

// Forms of the fields of a new item of the kind after its name and site: those its steps name, then the rest three at a time; what a login
// signs in with is picked before them
func batches(kind *vaultv1.Kind) []batch {
	byKey := map[string]*vaultv1.Field{}
	for _, field := range kind.GetFields() {
		byKey[field.GetKey()] = field
	}
	asked := map[string]bool{"identifier_type": true}
	var out []batch
	for _, step := range steps[kind.GetKind()] {
		b := batch{intro: step.intro}
		for _, key := range step.keys {
			if field, ok := byKey[key]; ok {
				b.fields, asked[key] = append(b.fields, field), true
			}
		}
		if len(b.fields) > 0 {
			out = append(out, b)
		}
	}
	var rest []*vaultv1.Field
	for _, field := range kind.GetFields() {
		if !asked[field.GetKey()] {
			rest = append(rest, field)
		}
	}
	for len(rest) > 0 {
		n := min(len(rest), 3)
		out = append(out, batch{intro: "The rest of the " + kinds[kind.GetKind()] + ".", fields: rest[:n]})
		rest = rest[n:]
	}
	return out
}

// Asks the form at the place, then the next, then saves the item; a refusal is said and the job stays on its last step
func (p *page) fields(f *tui.Flow, identifier string, request *vaultv1.AddRequest, forms []batch, at int) tea.Cmd {
	form := forms[at]
	var asked []cards.Field
	for _, field := range form.fields {
		asked = append(asked, formField(field, identifier, ""))
	}
	last := at == len(forms)-1
	verb := ""
	if last {
		verb = "Add"
	}
	return f.Ask(form.intro, asked, verb, func(values map[string]string) tea.Cmd {
		for _, field := range form.fields {
			request.Values[field.GetKey()] = values[field.GetKey()]
		}
		if !last {
			return p.fields(f, identifier, request, forms, at+1)
		}
		p.hub.Busy("Saving " + request.GetLabel() + "…")
		return tui.Call(p.hub, func() (*vaultv1.AddResponse, error) {
			return p.service.Add(p.kit.Env.Ctx, request)
		}, func(added *vaultv1.AddResponse) tea.Cmd {
			item := added.GetItem()
			return tea.Batch(f.End(), p.hub.Done("Saved "+item.GetLabel()+". The agent may fill it in on "+item.GetOrigin()+"."), p.load(func() tea.Cmd {
				for i, r := range p.rows() {
					if r.item != nil && r.item.GetHandle() == item.GetHandle() {
						p.root.Select(i)
					}
				}
				return nil
			}))
		})
	})
}
