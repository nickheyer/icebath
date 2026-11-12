package discord

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Discord send targets, a channel id with an optional thread id after a colon
const sendTargetPattern = `^\s*(-?\d+)(?::(\d+))?\s*$`

// Whether the Discord client is available, always since discordgo is compiled into the binary
func discordDepsPresent() bool { return true }

// Bundled Discord platform plugin registering the adapter with the gateway
type discordPlugin struct{}

func (discordPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "discord-platform", Key: "platforms/discord", Kind: "platform", Version: "1.0.0", Author: "NousResearch",
		Description: "Discord gateway adapter for Icebath Agent. Connects to Discord via the discordgo library and relays messages between " +
			"Discord guilds/DMs and the Icebath agent. Supports voice mode, slash commands, free-response channels, role-based DM auth, " +
			"threads, reactions, and channel skill bindings.",
		RequiresEnv: []plugins.EnvSpec{{Name: "DISCORD_BOT_TOKEN", Description: "Discord bot token"}}}
}

// Registers the Discord platform with its sender, YAML bridge, setup, card and access policy declarations
func (discordPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn": gateway.RegexGroupsParser(sendTargetPattern, true),
		"standalone_send": &gateway.StandaloneSendPolicy{Label: "Discord", Media: true, AllSends: true, Captionable: true,
			EmptyListBetweenChunks: true},
		"dashboard_description":  "Connect Icebath to Discord DMs, channels, and threads.",
		"docs_url":               "https://discord.com/developers/applications",
		"dashboard_env_vars":     []string{"DISCORD_BOT_TOKEN", "DISCORD_ALLOWED_USERS"},
		"ensure_deps_fn":         func(context.Context) (bool, error) { return discordDepsPresent(), nil },
		"is_connected":           gateway.EnvIsConnected("DISCORD_BOT_TOKEN"),
		"setup_fn":               InteractiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
		"cron_deliver_env_var":   "DISCORD_HOME_CHANNEL",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     discordMaxMessageLength,
		"emoji":                  "\U0001F3AE",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "high",
		"display_overrides":      map[string]any{"reasoning_style": "subtext"},
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{Name: "discord", Label: "Discord",
		AdapterFactory: gateway.AdapterFactory(func(c context.Context, cfg *gateway.PlatformConfig) (any, error) { return NewDiscordAdapter(c, cfg) }),
		Check:          discordDepsPresent, RequiredEnv: []string{"DISCORD_BOT_TOKEN"},
		InstallHint: "Run `icebath setup` to install Discord support.", Extra: extra})
	return err
}

// Adds the Discord platform plugin to the compiled-in set
func init() { plugins.RegisterBundled(discordPlugin{}) }

var _ gateway.PlatformAdapter = (*DiscordAdapter)(nil)
