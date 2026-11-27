package app

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Account a way to connect signs in to, of one provider of the profile
type Target struct {
	// Profile the provider is of, the one in use when empty
	Profile  string
	Provider *providerv1.Provider
	// Account signed in to, nil for one made only once the way to connect it succeeds
	Account *providerv1.Account
	// Name and title of the account made once the way succeeds, while Account is nil
	Name, Title string
	// Whether the person gave the title, which a sign-in otherwise replaces with who it signed in as
	Titled bool
	// Makes the account named, nil when Account is set
	make func(ctx context.Context, name, title string) (*providerv1.Account, error)
	// Dialog of the screen that started the way, which a way shows its page in so the trail goes on and the person comes back to where
	// they were, and what that screen does once the account is connected. A way opens a dialog of its own when Hub is nil
	Hub  *cards.Hub
	Done func() tea.Cmd
}

// Key of a field the account keeps
func (t Target) Key(field string) string {
	return t.Account.GetSection() + "." + field
}

// Name the account is shown by, the one it will have while it is not made yet
func (t Target) Label() string {
	if t.Account != nil {
		return t.Account.GetTitle()
	}
	return t.Title
}

// Target whose account exists, made now when it was waiting for the way to succeed
func (t Target) Ready(ctx context.Context) (Target, error) {
	if t.Account != nil {
		return t, nil
	}
	if t.make == nil {
		return t, errors.New("There is no account to keep what the sign-in gave.")
	}
	account, err := t.make(ctx, t.Name, t.Title)
	if err != nil {
		return t, err
	}
	t.Account, t.make = account, nil
	return t, nil
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
