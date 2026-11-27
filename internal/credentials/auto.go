package credentials

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Provider a request goes to when nothing names one, in the order icebath looks: a kind taken first whose credential is found, such as an
// OpenRouter key; the provider of the profile signed in first, in the order the kinds are registered, unless a key of another kind is set
// in the environment, which wins with a warning; the first kind of the registry order with a key in the environment; a provider of the
// profile whose credential is found anywhere; a kind taken last whose credential is found outside icebath, such as AWS's. Only kinds a wire serves are taken, and none on this machine, which a person
// picks with /model
func (r *Registry) Auto(cfg *config.Config, providers *provider.Registry, wires map[provider.APIMode]provider.Factory) (provider.Instance, error) {
	instances := providers.Instances(cfg)
	served := func(instance provider.Instance) bool {
		_, wired := wires[instance.Spec.Mode]
		return wired && instance.Known && !instance.Spec.Local && instance.Spec.Auth != provider.AuthNone
	}
	// The profile's provider of the kind, else one set up as the kind's plugin has it
	of := func(spec provider.Spec) provider.Instance {
		for _, instance := range instances {
			if instance.Spec.Name == spec.Name {
				return instance
			}
		}
		view := cfg.View(providerconfig.Key).View(spec.Name)
		if spec.Resolve != nil {
			spec = spec.Resolve(spec, view)
		}
		return provider.Instance{ID: spec.Name, Spec: spec, Known: true, Settings: view}
	}
	specs := providers.Specs()
	for _, spec := range specs {
		if instance := of(spec); spec.AutoFirst && served(instance) && r.has(instance) {
			return instance, nil
		}
	}
	signedIn, found := provider.Instance{}, false
	for _, spec := range specs {
		for _, instance := range instances {
			if instance.Spec.Name == spec.Name && served(instance) && r.signedIn(instance) {
				signedIn, found = instance, true
				break
			}
		}
		if found {
			break
		}
	}
	for _, spec := range specs {
		if spec.AuthType != provider.AuthTypeAPIKey || spec.AmbientCredentials || spec.Local {
			continue
		}
		instance := of(spec)
		variable := keyed(cfg, spec)
		if variable == "" || !served(instance) || !r.has(instance) {
			continue
		}
		if found && signedIn.Spec.Name != spec.Name {
			slog.Warn(fmt.Sprintf("Provider resolved to %q via %s, preempting your signed-in provider %q. If you meant to use that login, unset %s or "+
				"set the provider explicitly with /model --provider.", spec.Name, variable, signedIn.ID, variable))
		}
		return instance, nil
	}
	if found {
		return signedIn, nil
	}
	// A provider the profile has whose credential comes from somewhere else, such as a key file a variable names
	for _, spec := range specs {
		for _, instance := range instances {
			if instance.Spec.Name == spec.Name && served(instance) && r.has(instance) {
				return instance, nil
			}
		}
	}
	for _, spec := range specs {
		if instance := of(spec); spec.AutoLast && served(instance) && r.has(instance) {
			return instance, nil
		}
	}
	return provider.Instance{}, fmt.Errorf("Icebath is not connected to any AI provider yet. Add one with /providers or `icebath auth add <provider>`, " +
		"or pick a local model with /model. (Advanced: put an API key such as OPENROUTER_API_KEY in the profile's .env.)")
}

// Whether the provider has a credential it can use now
func (r *Registry) has(instance provider.Instance) bool {
	credential, ok := r.Credential(Of(instance, ""))
	return ok && (credential.APIKey != "" || credential.Sign != nil)
}

// Whether the provider is signed in: a credential kept in one of its accounts or taken from another program's sign-in, which still works
func (r *Registry) signedIn(instance provider.Instance) bool {
	account := Of(instance, "")
	credential, ok := r.Credential(account)
	if !ok || credential.Origin != Saved && credential.Origin != External {
		return false
	}
	state, _ := r.State(account)
	return state == Ready || state == Limited
}

// Variable of the kind's keys the profile sets with a usable key, empty when it sets none
func keyed(cfg *config.Config, spec provider.Spec) string {
	if i := slices.IndexFunc(spec.KeyEnvNames(), func(name string) bool { value, _ := cfg.LookupEnv(name); return Usable(value) }); i >= 0 {
		return spec.KeyEnvNames()[i]
	}
	return ""
}
