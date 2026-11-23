package discord

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

var envEnableCredentials = []string{"DISCORD_BOT_TOKEN"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformDiscord, Creds: [][]string{{"DISCORD_BOT_TOKEN"}}, Token: "DISCORD_BOT_TOKEN"},
	gateway.HomeStep{Platform: gateway.PlatformDiscord, EnvBase: "DISCORD_HOME_CHANNEL"},
	gateway.ReplyModeStep{Platform: gateway.PlatformDiscord, Env: "DISCORD_REPLY_TO_MODE"},
}

var authz = map[string]any{"allowed_users_env": "DISCORD_ALLOWED_USERS", "allow_all_env": "DISCORD_ALLOW_ALL_USERS", "allow_bots_env": "DISCORD_ALLOW_BOTS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled discord directory with the gateway config bridge, and the settings its access names moved to
func init() {
	gateway.RegisterBundledEnv("discord", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
	gateway.RegisterEnvSettings(map[string]settings.Entry{"DISCORD_ALLOWED_USERS": settings.DiscordAllowFrom,
		"DISCORD_ALLOW_ALL_USERS": settings.DiscordAllowAllUsers, "DISCORD_ALLOW_BOTS": settings.DiscordAllowBots,
		"DISCORD_ALLOWED_ROLES": settings.DiscordAllowedRoles, "DISCORD_ALLOWED_CHANNELS": settings.DiscordAllowedChannels})
}
