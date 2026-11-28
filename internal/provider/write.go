package provider

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Turns the use of a credential the provider finds outside its accounts off or on: a variable holding a key, or the sign-in another
// program keeps on this computer, by its name. The one control of that choice, kept in the provider's list of what it leaves out, which
// nothing else writes; whether it changed
func (r *Registry) SetOutside(cfg *config.Config, id, source string, on bool) (bool, error) {
	instance, ok := r.Instance(cfg, id)
	source = strings.TrimSpace(source)
	switch {
	case !ok:
		return false, noProvider(id)
	case source == "":
		return false, fmt.Errorf("Name the variable or program whose key or sign-in %s should take or leave out.", instance.Label())
	}
	ignored := instance.Settings.GetStringSlice(providerconfig.Ignored)
	if slices.Contains(ignored, source) != on {
		return false, nil
	}
	if on {
		ignored = slices.DeleteFunc(slices.Clone(ignored), func(have string) bool { return have == source })
	} else {
		ignored = append(slices.Clone(ignored), source)
	}
	if len(ignored) == 0 {
		return true, instance.Settings.Set(providerconfig.Ignored, nil)
	}
	return true, instance.Settings.Set(providerconfig.Ignored, ignored)
}

// Keeps the key an account read from a variable in the account itself, which stops reading the variable, in one write; the limits it ran
// into stay, as the key is the same
func (r *Registry) KeepKey(cfg *config.Config, id, name, key string) error {
	instance, ok := r.Instance(cfg, id)
	if !ok || !slices.Contains(instance.Accounts, name) {
		return noAccount(instance, id, name)
	}
	return r.rewrite(instance, func(kept map[string]any) error {
		entry, _ := table(kept, providerconfig.Accounts)[name].(map[string]any)
		if entry == nil {
			return noAccount(instance, id, name)
		}
		entry[providerconfig.APIKeyField] = key
		delete(entry, providerconfig.KeyEnv)
		return nil
	})
}

// Whether a credential found outside the accounts is one the provider uses, by the variable or program it comes from
func (i Instance) Takes(source string) bool {
	return !slices.Contains(i.Settings.GetStringSlice(providerconfig.Ignored), strings.TrimSpace(source))
}

// Turns an account's taking turns on or off, in one write with the provider's choice of account: turning it on makes the provider take
// turns between the accounts that do, and turning the last one off makes it try every account in order again
func (r *Registry) SetTurns(cfg *config.Config, id, name string, on bool) error {
	instance, ok := r.Instance(cfg, id)
	if !ok || !slices.Contains(instance.Accounts, name) {
		return noAccount(instance, id, name)
	}
	return r.rewrite(instance, func(kept map[string]any) error {
		accounts := table(kept, providerconfig.Accounts)
		entry, _ := accounts[name].(map[string]any)
		if entry == nil {
			return noAccount(instance, id, name)
		}
		if on {
			entry[providerconfig.InRotation] = true
			kept[providerconfig.Account] = providerconfig.Rotation
			return nil
		}
		delete(entry, providerconfig.InRotation)
		turning := slices.ContainsFunc(instance.Accounts, func(other string) bool {
			marked, _ := table(accounts, other)[providerconfig.InRotation].(bool)
			return other != name && marked
		})
		if !turning && kept[providerconfig.Account] == providerconfig.Rotation {
			delete(kept, providerconfig.Account)
		}
		return nil
	})
}

// Address of the provider's server, which must start http:// or https:// and is kept without a slash at its end; empty puts the kind's
// own back, refused for a kind that has none
func (r *Registry) SetAddress(cfg *config.Config, id, address string) error {
	instance, ok := r.Instance(cfg, id)
	if !ok {
		return noProvider(id)
	}
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	if address == "" {
		if kind, known := r.exact(instance.Spec.Name); !known || kind.BaseURL == "" {
			return fmt.Errorf("%s needs the address of its server to send requests to.", instance.Label())
		}
		return instance.Settings.Set(providerconfig.BaseURL, nil)
	}
	if err := Address(address); err != nil {
		return err
	}
	return instance.Settings.Set(providerconfig.BaseURL, address)
}

// Why text can't be the address of a server, nil when it can
func Address(address string) error {
	if !strings.HasPrefix(address, "http://") && !strings.HasPrefix(address, "https://") {
		return fmt.Errorf("%q can't be an address. Start it with http:// or https://.", address)
	}
	if BaseHost(address) == "" {
		return fmt.Errorf("%q names no server. Write it as http://host:port/path.", address)
	}
	return nil
}

// Offers the provider's models in the model list, or leaves them out: the provider's place in the providers the model list hides, by its
// name in the profile
func (r *Registry) SetOffered(cfg *config.Config, id string, on bool) error {
	instance, ok := r.Instance(cfg, id)
	if !ok {
		return noProvider(id)
	}
	hidden := cfg.GetStringSlice(modelsconfig.CatalogExcluded)
	kept := slices.DeleteFunc(slices.Clone(hidden), func(name string) bool { return strings.EqualFold(strings.TrimSpace(name), instance.ID) })
	if !on {
		kept = append(kept, instance.ID)
	}
	if slices.Equal(kept, hidden) {
		return nil
	}
	if len(kept) == 0 {
		return cfg.Set(modelsconfig.CatalogExcluded, nil)
	}
	return cfg.Set(modelsconfig.CatalogExcluded, kept)
}

// Whether the model list offers the provider's models
func (i Instance) Offered(cfg *config.Config) bool {
	return !slices.ContainsFunc(cfg.GetStringSlice(modelsconfig.CatalogExcluded), func(name string) bool {
		return strings.EqualFold(strings.TrimSpace(name), i.ID)
	})
}
