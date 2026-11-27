package cli

import "github.com/nickheyer/icebath/internal/credentials"

// Ways a plugin adds to sign the accounts of its kinds in from the command line
type Registrant interface {
	SignIns() []credentials.SignIn
}

// Registrant that keeps a profile's choice of provider in a setting, such as the agent's provider of new sessions: the key of the setting,
// which a logout clears when it names the provider logged out of
type Chooser interface {
	Choice() string
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) SignIns() []credentials.SignIn {
	var ways []credentials.SignIn
	for _, registrant := range r.registrants {
		ways = append(ways, registrant.SignIns()...)
	}
	return ways
}

// Keys of the settings the registrants keep a profile's choice of provider in, in the order they registered
func (r *Registry) Choices() []string {
	var keys []string
	for _, registrant := range r.registrants {
		if chooser, ok := registrant.(Chooser); ok {
			keys = append(keys, chooser.Choice())
		}
	}
	return keys
}

// Sign-ins plugins register, run by auth add
type SignIns struct {
	Registry
}

func New() *SignIns {
	return &SignIns{}
}
