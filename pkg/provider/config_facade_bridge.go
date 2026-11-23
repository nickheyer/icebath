package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	armProviderEnv(config.OptionalEnvVars, config.StaticCoreEnvNames())
	config.RegisterConfigFacade(config.FacadeHooks{
		ApplyUserMoAPresets: ApplyUserMoAPresets,
		SkipDeepMerge:       SkipDeepMerge,
		DotenvUnreadable:    DotenvUnreadable,
		ExtraEnvKeys:        configFacadeExtraEnvKeys,

		MaskedSecretPrompt:          maskedSecretPrompt,
		ScopedSecret:                GetSecret,
		SecretScopeBound:            func(ctx context.Context) bool { return CurrentSecretScope(ctx) != nil },
		PublishScopedEnv:            publishScopedEnv,
		SaveProviderEnvCredential:   saveProviderEnvCredential,
		RemoveProviderEnvCredential: removeProviderEnvCredential,
		CoreDeclaredEnvNames:        CoreDeclaredEnvNames,
	})
}

// Configurable toolset keys plus plugin toolset keys, the keys that select a saved list's explicit branch
func explicitToolsetKeys() map[string]bool {
	keys := tools.ConfigurableKeys()
	for k := range tools.PluginToolsetKeys(context.Background()) {
		keys[k] = true
	}
	return keys
}

// Writes a persisted .env change into the active scope mapping and reports whether the process env takes it too
func publishScopedEnv(ctx context.Context, key string, value *string) bool {
	routed := ServesRoutedProfile(ctx)
	if s := boundScope(ctx); s != nil && s.Mapping != nil {
		s.mu.Lock()
		if value == nil {
			delete(s.Mapping, key)
		} else {
			s.Mapping[key] = *value
		}
		s.mu.Unlock()
	}
	return !routed
}

// Env names outside the optional catalog that reload_env still removes when the .env drops them
func configFacadeExtraEnvKeys() []string {
	out := make([]string, 0, len(extraEnvKeys))
	for name := range extraEnvKeys {
		out = append(out, name)
	}
	for _, name := range leftCoreProfileEnv() {
		if !extraEnvKeys[name] {
			out = append(out, name)
		}
	}
	return out
}

// Config issue for a failed config load, storage failures kept apart from a broken config file
func configLoadIssue(err error) config.ConfigIssue { return config.ConfigLoadIssue(err) }

// Bridges the active home terminal config into the process env the terminal tools read
func applyTerminalConfigToEnv(ctx context.Context) error {
	return config.ApplyTerminalConfigToEnv(ctx, home.Dir(ctx), config.TerminalEnvOptions{})
}
