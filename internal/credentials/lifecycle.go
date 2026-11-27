package credentials

import (
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
// read it take it from it again, and what held back the accounts it is the key of on the old value is cleared
func (l Lifecycle) Saved(cfg *config.Config, name, old, value string) error {
	if value != "" && old != "" && old != value {
		if _, err := mirrors(cfg, old, value); err != nil {
			return err
		}
	}
	for _, instance := range l.readers(cfg, name) {
		ignored := instance.Settings.GetStringSlice(providerconfig.Ignored)
		if kept := slices.DeleteFunc(slices.Clone(ignored), func(source string) bool { return source == name }); len(kept) < len(ignored) {
			if err := instance.Settings.Set(providerconfig.Ignored, kept); err != nil {
				return err
			}
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

// After the variable was removed: the providers that read it leave it out from now on, so a shell that still exports it can't give it back
// until it is set again, and copies of its old value in api key settings are removed. Whether a setting held a copy
func (l Lifecycle) Removed(cfg *config.Config, name, old string) (bool, error) {
	for _, instance := range l.readers(cfg, name) {
		if ignored := instance.Settings.GetStringSlice(providerconfig.Ignored); !slices.Contains(ignored, name) {
			if err := instance.Settings.Set(providerconfig.Ignored, append(slices.Clone(ignored), name)); err != nil {
				return false, err
			}
		}
	}
	if old == "" {
		return false, nil
	}
	scrubbed, err := mirrors(cfg, old, "")
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
// administrator or Remote Config decide are left as they are. The keys touched
func mirrors(cfg *config.Config, old, value string) ([]string, error) {
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
			if err := cfg.Set(setting.Key, replacement); err != nil {
				return nil, err
			}
			touched = append(touched, setting.Key)
		}
	}
	return touched, nil
}
