package credentials

import (
	"errors"
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
// profile ready with a credential found anywhere; a kind taken last whose credential is found outside icebath, such as AWS's. Only providers
// ready by the test the providers page uses are taken, and none on this machine, which a person adds themselves. With none, why the first
// provider of the profile is not ready, else what to connect
func (r *Registry) Auto(cfg *config.Config, providers *provider.Registry, wires map[provider.APIMode]provider.Factory) (provider.Instance, error) {
	instances := providers.Instances(cfg)
	// The credential a provider sends requests with now, false while it is not ready, runs on this machine or needs none
	ready := func(instance provider.Instance) (Credential, bool) {
		if instance.Spec.Local || instance.Spec.Auth == provider.AuthNone {
			return Credential{}, false
		}
		_, credential, err := r.Ready(instance, wires)
		return credential, err == nil && (credential.APIKey != "" || credential.Sign != nil)
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
		if instance := of(spec); spec.AutoFirst {
			if _, ok := ready(instance); ok {
				return instance, nil
			}
		}
	}
	signedIn, found := provider.Instance{}, false
	for _, spec := range specs {
		for _, instance := range instances {
			if instance.Spec.Name != spec.Name {
				continue
			}
			if credential, ok := ready(instance); ok && (credential.Origin == Saved || credential.Origin == External) {
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
		if variable == "" {
			continue
		}
		if _, ok := ready(instance); !ok {
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
			if instance.Spec.Name != spec.Name {
				continue
			}
			if _, ok := ready(instance); ok {
				return instance, nil
			}
		}
	}
	for _, spec := range specs {
		if instance := of(spec); spec.AutoLast {
			if _, ok := ready(instance); ok {
				return instance, nil
			}
		}
	}
	for _, spec := range specs {
		for _, instance := range instances {
			if instance.Spec.Name != spec.Name || instance.Spec.Local || instance.Spec.Auth == provider.AuthNone {
				continue
			}
			if _, _, err := r.Ready(instance, wires); err != nil {
				return provider.Instance{}, fmt.Errorf("No provider of yours is ready. %w", err)
			}
		}
	}
	return provider.Instance{}, errors.New("icebath is not connected to any provider yet. Type /providers in icebath to add one, such as a " +
		"server on this computer, or run `icebath auth add <provider>`.")
}

// Variable of the kind's keys the profile sets with a usable key, empty when it sets none
func keyed(cfg *config.Config, spec provider.Spec) string {
	if i := slices.IndexFunc(spec.KeyEnvNames(), func(name string) bool { value, _ := cfg.LookupEnv(name); return Usable(value) }); i >= 0 {
		return spec.KeyEnvNames()[i]
	}
	return ""
}
