package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	config.RegisterConfigFacade(config.FacadeHooks{
		ApplyUserMoAPresets: ApplyUserMoAPresets,
		SkipDeepMerge:       SkipDeepMerge,
		IsPresetFieldPath:   IsPresetFieldPath,
		Stamp:               configFacadeStampHooks,
		DotenvUnreadable:    DotenvUnreadable,
		ExtraEnvKeys:        configFacadeExtraEnvKeys,

		Migration:                   configFacadeMigrationSources,
		MaskedSecretPrompt:          maskedSecretPrompt,
		ScopedSecret:                GetSecret,
		PublishScopedEnv:            publishScopedEnv,
		SaveProviderEnvCredential:   saveProviderEnvCredential,
		RemoveProviderEnvCredential: removeProviderEnvCredential,
		CoreDeclaredEnvNames:        configFacadeCoreDeclaredEnvNames,
	})
}

// Ladder collaborators provider and tools own, the facade adds its readers, writers and personality sources
func configFacadeMigrationSources(h string) config.MigrationSources {
	return config.MigrationSources{
		CustomProviderEntryToProviderConfig: customProviderEntryToProviderConfig,
		LegacyRelayPluginKeys:               plugins.LegacyRelayPluginKeysIn,
		BotProtocolHeading:                  tools.BotModeProtocolHeading,
		BotRoster:                           botmode.ConfigRoster,
		StripLegacyBotProtocol:              tools.StripLegacyBotProtocol,
		ParseConfigStringList:               tools.ParseConfigStringList,
		ExplicitToolsetKeys:                 explicitToolsetKeys,
		ToolsetAllowedForPlatform:           tools.ToolsetAllowedForPlatform,
		ParseEnabledFlag:                    parseEnabledFlag,
		RecordCoreUse:                       RecordCoreUse,
		Browser: config.BrowserEnvSources{
			HomeDotenv:   resolveHomeDotenv,
			IsLaunchHome: func(h string) bool { return home.Key(h) == home.Key(RoutingProcessHome()) },
			PMStoreRoots: func() ([]string, error) {
				store, err := pm.StoreRoot(context.Background())
				if err != nil {
					return nil, err
				}
				writable, err := pm.WritableStoreRoot(context.Background())
				if err != nil {
					return nil, err
				}
				return []string{store, writable}, nil
			},
			DeferStartupNotice: func(message string) { DeferStartupNotice(home.WithOverride(context.Background(), h), message) },
		},
	}
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

// First stamp collaborators every facade save hands to NoteFirstStamp
func configFacadeStampHooks() config.StampHooks {
	return config.StampHooks{
		WorkerSnapshotActive: workerConfigSnapshotActive,
		CoreVersions:         LeftCoreVersions(),
		RecordCoreUse:        RecordCoreUse,
		AdoptAgentBrowserEnv: adoptAgentBrowserEnv,
	}
}

// Env names outside the optional catalog that reload_env still removes when the .env drops them
func configFacadeExtraEnvKeys() []string {
	out := make([]string, 0, len(extraEnvKeys))
	for name := range extraEnvKeys {
		out = append(out, name)
	}
	return out
}

// Config issue for a failed config load, storage failures kept apart from a broken config file
func configLoadIssue(err error) config.ConfigIssue { return config.ConfigLoadIssue(err) }

// Bridges the active home terminal config into the process env the terminal tools read
func applyTerminalConfigToEnv(ctx context.Context) error {
	return config.ApplyTerminalConfigToEnv(ctx, home.Dir(ctx), config.TerminalEnvOptions{})
}

// Names core declares, the static table read before platform manifests plus the provider profile names
func configFacadeCoreDeclaredEnvNames() map[string]bool {
	FillProviderEnvVars()
	out := config.StaticCoreEnvNames()
	providerEnvMu.Lock()
	defer providerEnvMu.Unlock()
	for name := range providerEnvNames {
		out[name] = true
	}
	return out
}
