package credentials

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// What else changes when a credential variable of a profile's .env is set or removed, as config set and unset do: the copies of its value
// kept in api key settings, the providers that read it, and what holds back the accounts it is the key of
type Lifecycle struct {
	Providers *provider.Registry
}

// After the variable was saved over the value it held: copies of the old value in api key settings take the new one, the providers that
// read it take it from it again by the one control of that choice, and what held back the accounts it is the key of on the old value is
// cleared
func (l Lifecycle) Saved(cfg *config.Config, name, old, value string) error {
	if value != "" && old != "" && old != value {
		if _, err := l.mirrors(cfg, old, value); err != nil {
			return err
		}
	}
	for _, instance := range l.readers(cfg, name) {
		if _, err := l.Providers.SetOutside(cfg, instance.ID, name, true); err != nil {
			return err
		}
	}
	if old == value {
		return nil
	}
	for _, account := range l.keyedBy(cfg, name) {
		if _, err := Reset(account); err != nil {
			return err
		}
	}
	return nil
}

// After the variable was removed: the providers that read it leave it out from now on by the one control of that choice, so a shell that
// still exports it can't give it back until it is set again, and copies of its old value in api key settings are removed, with the accounts
// they were the one credential of. Whether a setting held a copy
func (l Lifecycle) Removed(cfg *config.Config, name, old string) (bool, error) {
	for _, instance := range l.readers(cfg, name) {
		if _, err := l.Providers.SetOutside(cfg, instance.ID, name, false); err != nil {
			return false, err
		}
	}
	if old == "" {
		return false, nil
	}
	scrubbed, err := l.mirrors(cfg, old, "")
	return len(scrubbed) > 0, err
}

// Providers of the profile whose kind reads its key from the variable
func (l Lifecycle) readers(cfg *config.Config, name string) []provider.Instance {
	var out []provider.Instance
	for _, instance := range l.Providers.Instances(cfg) {
		if slices.Contains(instance.Spec.KeyEnvNames(), name) {
			out = append(out, instance)
		}
	}
	return out
}

// Accounts whose key the variable gives: the one with no key of its own of each provider whose kind reads it, and every account linked to it
func (l Lifecycle) keyedBy(cfg *config.Config, name string) []Account {
	var out []Account
	for _, instance := range l.Providers.Instances(cfg) {
		if slices.Contains(instance.Spec.KeyEnvNames(), name) {
			out = append(out, Account{Provider: instance, Fields: instance.Settings.Bare()})
		}
		for _, account := range instance.Accounts {
			if strings.TrimSpace(instance.AccountFields(account).GetString(providerconfig.KeyEnv)) == name {
				out = append(out, Of(instance, account))
			}
		}
	}
	return out
}

// Rewrites the api key settings of the profile's files that hold the old value to the new one, removing them for no new one; those the
// administrator or Remote Config decide are left as they are. An account whose one credential the removed key was goes with it, so no
// account stands without a credential. The keys touched
func (l Lifecycle) mirrors(cfg *config.Config, old, value string) ([]string, error) {
	var replacement any
	if value != "" {
		replacement = value
	}
	var touched []string
	for _, section := range cfg.Sections() {
		for _, setting := range section.Settings {
			if !strings.HasSuffix(setting.Key, "."+providerconfig.APIKeyField) || !cfg.Holds(setting.Key) || cfg.Lock(setting.Key) != nil {
				continue
			}
			if held, _ := cfg.Stored(setting.Key).(string); held != old {
				continue
			}
			if instance, name, ok := l.keeper(cfg, setting.Key); ok && replacement == nil && !Of(instance, name).Holds(providerconfig.APIKeyField) {
				if _, err := l.Providers.RemoveAccount(context.Background(), cfg, instance.ID, name); err != nil {
					return nil, err
				}
			} else if err := cfg.Set(setting.Key, replacement); err != nil {
				return nil, err
			}
			touched = append(touched, setting.Key)
		}
	}
	return touched, nil
}

// Provider and account whose api key the key is, false for a key of anything else
func (l Lifecycle) keeper(cfg *config.Config, key string) (provider.Instance, string, bool) {
	parts := strings.Split(key, ".")
	if len(parts) != 5 || parts[0] != providerconfig.Key || parts[2] != providerconfig.Accounts || parts[4] != providerconfig.APIKeyField {
		return provider.Instance{}, "", false
	}
	instance, ok := l.Providers.Instance(cfg, parts[1])
	if !ok || instance.ID != parts[1] || !slices.Contains(instance.Accounts, parts[3]) {
		return provider.Instance{}, "", false
	}
	return instance, parts[3], true
}
