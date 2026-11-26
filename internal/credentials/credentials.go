package credentials

import (
	"cmp"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Where a credential was found
type Origin int

const (
	// Saved in an account of the provider
	Saved Origin = iota + 1
	// Read from an environment variable, the one the source names
	Environment
	// Taken from the sign in of another program, the one the source names
	External
	// None is needed, the stand in of a server on this machine
	Unneeded
)

type Credential struct {
	APIKey  string
	BaseURL string
	Auth    provider.AuthKind
	Origin  Origin
	// Variable or program the credential came from, empty for one saved in icebath
	Source string
}

// One account of a provider of the profile: the fields it keeps, under the provider's accounts. A provider with no account has one with
// no name and no fields, whose credential can only come from the environment or another program
type Account struct {
	Name     string
	Provider provider.Instance
	Fields   config.View
}

// Kind of the provider the account is of
func (a Account) Spec() string {
	return a.Provider.Spec.Name
}

type Registrant interface {
	Credential(account Account) (Credential, bool)
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Credential(account Account) (Credential, bool) {
	for _, registrant := range r.registrants {
		if credential, ok := registrant.Credential(account); ok {
			return credential, true
		}
	}
	return Credential{}, false
}

// Account of the provider with the name, the one in use for no name
func Of(instance provider.Instance, name string) Account {
	name = cmp.Or(name, instance.Account)
	if name == "" {
		return Account{Provider: instance}
	}
	return Account{Name: name, Provider: instance, Fields: instance.AccountFields(name)}
}

// Credential of a kind no provider of the profile has yet, found in the environment or another program
func (r *Registry) Detect(spec provider.Spec) (Credential, bool) {
	return r.Credential(Account{Provider: provider.Instance{ID: spec.Name, Spec: spec, Known: true}})
}

// Route of a request to the provider with its account in use
func (r *Registry) Route(instance provider.Instance, model string) (provider.Route, error) {
	spec := instance.Spec
	route := provider.Route{
		Provider: instance.ID,
		Spec:     spec.Name,
		Model:    cmp.Or(model, spec.DefaultModel),
		BaseURL:  spec.BaseURL,
		Headers:  spec.Headers,
		Mode:     spec.Mode,
		Auth:     spec.Auth,
		Local:    spec.Local,
	}
	if !instance.Known {
		return route, fmt.Errorf("%s is a kind of provider icebath doesn't know (%s).", instance.Label(), spec.Name)
	}
	credential, ok := r.Credential(Of(instance, ""))
	if !ok && !spec.Local && spec.Auth != provider.AuthNone {
		return route, fmt.Errorf("%s is not connected. Connect it with /providers.", instance.Label())
	}
	route.APIKey = credential.APIKey
	route.BaseURL = cmp.Or(credential.BaseURL, route.BaseURL)
	route.Auth = cmp.Or(credential.Auth, route.Auth)
	if spec.PerModelMode != nil {
		route.Mode = cmp.Or(spec.PerModelMode(route.Model), route.Mode)
	}
	if route.BaseURL == "" {
		return route, fmt.Errorf("%s has no base URL.", instance.Label())
	}
	return route, nil
}

// Credential kept in the field of the account, else in the first of the variables set
func Lookup(fields config.View, field string, variables ...string) (Credential, bool) {
	if value := strings.TrimSpace(fields.GetString(field)); value != "" {
		return Credential{APIKey: value, Origin: Saved}, true
	}
	return FromEnv(variables...)
}

// Credential in the first of the variables set
func FromEnv(variables ...string) (Credential, bool) {
	for _, variable := range variables {
		if value := strings.TrimSpace(os.Getenv(variable)); value != "" {
			return Credential{APIKey: value, Origin: Environment, Source: variable}, true
		}
	}
	return Credential{}, false
}

// Credential of a kind whose accounts keep an API key, read from the environment when an account keeps none
type Keyed struct {
	Provider  string
	Variables []string
}

func (k Keyed) Credential(account Account) (Credential, bool) {
	if account.Spec() != k.Provider {
		return Credential{}, false
	}
	return Lookup(account.Fields, providerconfig.APIKeyField, k.Variables...)
}

type Credentials struct {
	Registry
}

func New() *Credentials {
	return &Credentials{}
}
