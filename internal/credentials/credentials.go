package credentials

import (
	"cmp"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type Credential struct {
	APIKey  string
	BaseURL string
	Auth    provider.AuthKind
	Source  string
}

type Registrant interface {
	Credential(provider string) (Credential, bool)
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Credential(provider string) (Credential, bool) {
	for _, registrant := range r.registrants {
		if credential, ok := registrant.Credential(provider); ok {
			return credential, true
		}
	}
	return Credential{}, false
}

func (r *Registry) Route(profile provider.Profile, model string) (provider.Route, error) {
	route := provider.Route{
		Provider: profile.Name,
		Model:    cmp.Or(model, profile.DefaultModel),
		BaseURL:  profile.BaseURL,
		Headers:  profile.Headers,
		Mode:     profile.Mode,
		Auth:     profile.Auth,
		Local:    profile.Local,
	}
	credential, ok := r.Credential(profile.Name)
	if !ok && !profile.Local && profile.Auth != provider.AuthNone {
		return route, fmt.Errorf("provider %s: no credential", profile.Name)
	}
	route.APIKey = credential.APIKey
	route.BaseURL = cmp.Or(credential.BaseURL, route.BaseURL)
	route.Auth = cmp.Or(credential.Auth, route.Auth)
	if profile.PerModelMode != nil {
		route.Mode = cmp.Or(profile.PerModelMode(route.Model), route.Mode)
	}
	if route.BaseURL == "" {
		return route, fmt.Errorf("provider %s: no base url", profile.Name)
	}
	return route, nil
}

func Lookup(cfg *config.Config, key string, variables ...string) (Credential, bool) {
	value := cfg.GetString(key)
	if value == "" {
		return Credential{}, false
	}
	credential := Credential{APIKey: value, Source: "config"}
	for _, variable := range variables {
		if os.Getenv(variable) == value {
			credential.Source = variable
			break
		}
	}
	return credential, true
}

type Keyed struct {
	Config    *config.Config
	Provider  string
	Key       string
	Variables []string
}

func (k Keyed) Credential(provider string) (Credential, bool) {
	if provider != k.Provider {
		return Credential{}, false
	}
	return Lookup(k.Config, k.Key, k.Variables...)
}

type Credentials struct {
	Registry
}

func New() *Credentials {
	return &Credentials{}
}
