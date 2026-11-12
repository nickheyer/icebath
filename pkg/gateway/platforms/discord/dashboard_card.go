package discord

import "github.com/nickheyer/icebath/pkg/gateway"

// Sets the channels page card description, docs link and env vars on the registry entry
func applyDashboardCard(e *gateway.PlatformEntry) {
	e.DashboardDescription = "Connect Icebath to Discord DMs, channels, and threads."
	e.DocsURL = "https://discord.com/developers/applications"
	e.DashboardEnvVars = []string{"DISCORD_BOT_TOKEN", "DISCORD_ALLOWED_USERS"}
}

// Registers the card of the bundled discord directory for dashboards whose registry holds no discord entry
func init() { gateway.RegisterBundledDashboardCard("discord", applyDashboardCard) }
