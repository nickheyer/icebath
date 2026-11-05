package commands

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/spf13/cobra"
)

func init() {
	config.RegisterMigrateCollaborators(config.MigrateCollaborators{
		Personality: config.PersonalitySources{
			Normalize:  NormalizePersonalityName,
			PromptText: PromptText,
			Render:     RenderPersonalityPrompt,
			Available:  func(cfg map[string]any) map[string]any { return AvailablePersonalities(config.Tree(cfg)) },
		},
		ValidateMCPServerEntry:  mcp.ValidateServerEntry,
		PlatformToolsetWarnings: platformToolsetWarnings,
		SkillConfigVars:         discoverSkillConfigVars,
	})
}

// Saved platform toolset problems resolved against the toolsets the saved config defines
func platformToolsetWarnings(cfg map[string]any) ([]string, error) {
	pt, _ := cfg["platform_toolsets"].(map[string]any)
	return tools.ValidatePlatformToolsets(pt, tools.SavedToolsetResolver(cfg)), nil
}

// Skill declared config variables of every installed skill in the facade shape
func discoverSkillConfigVars() ([]config.SkillConfigVar, error) {
	found, err := skills.DiscoverAllSkillConfigVars()
	if err != nil {
		return nil, err
	}
	out := make([]config.SkillConfigVar, 0, len(found))
	for _, v := range found {
		out = append(out, config.SkillConfigVar{Key: v.Key, Skill: v.Skill, Description: v.Description, Prompt: v.Prompt, Default: v.Default})
	}
	return out, nil
}

// Binds icebath config and every config subcommand to the config facade
func (c *CLI) bindConfig() {
	for _, sub := range []string{"", "show", "edit", "get", "set", "unset", "path", "env-path", "check", "migrate"} {
		path := strings.TrimSpace("config " + sub)
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runConfig(cmd, sub, args) })
	}
}

// Parses one config command line into ConfigArgs and runs it, a nonzero status becoming an ExitError
func runConfig(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f := cmd.Flags()
	a := config.ConfigArgs{Command: sub}
	if len(args) > 0 {
		a.Key = args[0]
	}
	if len(args) > 1 {
		value := args[1]
		a.Value = &value
	}
	if f.Lookup("json") != nil {
		a.JSON, _ = f.GetBool("json")
	}
	if f.Lookup("raw") != nil {
		a.Raw, _ = f.GetBool("raw")
	}
	if f.Lookup("force") != nil {
		a.Force, _ = f.GetBool("force")
	}
	h := home.Dir(ctx)
	err := config.ConfigCommand(ctx, h, a, configCommandSources(h))
	var exit *config.ConfigExit
	if errors.As(err, &exit) {
		return &ExitError{Code: exit.Code}
	}
	return err
}

// Readers and writers the config facade commands reach in provider, tools, plugins, skills and gateway owners
func configCommandSources(h string) config.ConfigCommandSources {
	return config.ConfigCommandSources{
		RuntimeKeyOwners: config.RuntimeKeyOwners{
			SelectionConfigKeys: tools.SelectionConfigKeys,
			ActiveManifests:     func() []config.PluginConfigKeys { return plugins.ActiveConfigKeyManifests(h) },
		},
		LeftCoreTerminalKeys:    provider.LeftCoreTerminalKeys,
		DisplayOverrideableKeys: provider.DisplayOverrideableKeys(),
		IsEnvSettingKey:         provider.IsEnvSettingKey,
		SaveEnvSetting:          provider.SaveEnvSetting,
		ReadEnvSetting:          provider.ReadEnvSetting,
		RemoveEnvSetting:        provider.RemoveEnvSetting,
		DropStaleModelRoute:     provider.DropStaleModelRoute,
		Language:                provider.LanguageCatalog(),
		Show:                    configShowSources,
		CheckDiagnostics: config.CheckDiagnosticsSources{
			PlatformToolsetDiagnostics: func(pt any, cfg map[string]any) []string {
				m, _ := pt.(map[string]any)
				return tools.ValidatePlatformToolsets(m, tools.SavedToolsetResolver(cfg))
			},
			DisabledPlugins:          func() map[string]bool { return plugins.DisabledPlugins(h) },
			BundledPlatformManifests: func() ([]config.PlatformManifest, error) { return config.BundledPlatformManifests(h) },
		},
	}
}

// Readers config show consults, the loaded config and env of the home plus auth, personality and skill owners
func configShowSources(ctx context.Context, h string) (config.ShowSources, error) {
	cfg, err := config.LoadContext(ctx, h, config.EnvRefLookup)
	if err != nil {
		return config.ShowSources{}, err
	}
	return config.ShowSources{
		Config:      map[string]any(cfg),
		ConfigPath:  home.ConfigPath(h),
		EnvPath:     home.EnvPath(h),
		ProjectRoot: config.GetProjectRoot(),
		EnvValue: func(name string) string {
			v, _, _ := config.GetEnvValue(ctx, h, name)
			return v
		},
		EnvFile:           func() (map[string]string, error) { return config.LoadEnvFile(home.EnvPath(h)), nil },
		AnthropicKey:      func() string { return provider.GetAnthropicKey(ctx) },
		ActivePersonality: ActivePersonalityName,
		SkillSettings:     skills.SkillSettingsForShow,
		MaskSecret:        config.MaskSecret,
		RedactConfigValue: config.RedactConfigValue,
	}, nil
}
