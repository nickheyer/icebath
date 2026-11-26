package app

import (
	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"

	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Account a way to connect signs in to, of one provider of the profile
type Target struct {
	// Profile the provider is of, the one in use when empty
	Profile  string
	Provider *providerv1.Provider
	Account  *providerv1.Account
}

// Key of a field the account keeps
func (t Target) Key(field string) string {
	return t.Account.GetSection() + "." + field
}

// Way to sign an account in from the app besides pasting an API key, such as a browser sign in
type Way struct {
	// Shown as a choice when there is more than one way, such as "Claude subscription"
	Title string
	Open  func(kit *tui.Kit, target Target) tea.Cmd
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
