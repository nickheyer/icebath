package app

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Account a way to connect signs in to, of one provider of the profile
type Target struct {
	// Profile the provider is of, the one in use when empty
	Profile  string
	Provider *providerv1.Provider
	// Account signed in to again, nil for a new one
	Account *providerv1.Account
	// Name the person gave a new account, empty for the one the server gives it after who signed in or the way it signs in
	Title string
	// Dialog of the page that started the way, and how the way shows its page there: as the next step of the job it is part of, so the
	// trail goes on and esc goes back a step
	Hub  *cards.Hub
	Next func(step wm.Card) tea.Cmd
	// Keeps what the way gave in the account, adding the provider first when the job adds it: the account as kept
	Keep func(ctx context.Context, fields map[string]any, identity string) (*providerv1.Account, error)
	// What the page does once the account is kept, such as ending its job and saying so
	Done func(account *providerv1.Account) tea.Cmd
}

// Name the account is shown by, the one the person gave a new one while it is not made yet
func (t Target) Label() string {
	if t.Account != nil {
		return t.Account.GetTitle()
	}
	return t.Title
}

// Way to sign an account in from the app besides pasting an API key, such as a browser sign in
type Way struct {
	// Shown as a choice when there is more than one way, such as "Claude subscription"
	Title string
	// How it signs in, as the sign-in names it: oauth for a browser sign-in
	Kind string
	Open func(kit *tui.Kit, target Target) tea.Cmd
}

// Ways a plugin adds to connect the accounts of one kind of provider
type Connector struct {
	// Kind of provider, by the name of its spec
	Spec string
	Ways []Way
}

type Registrant interface {
	Connectors(conn *connect.Client) []Connector
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Connectors(conn *connect.Client) []Connector {
	var connectors []Connector
	for _, registrant := range r.registrants {
		connectors = append(connectors, registrant.Connectors(conn)...)
	}
	return connectors
}

// Connectors plugins register, read by the providers screen
type Connectors struct {
	Registry
}

func New() *Connectors {
	return &Connectors{}
}
