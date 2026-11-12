package matrix

import "github.com/nickheyer/icebath/pkg/gateway"

// Sets the channels page card description, docs link, env vars and required env on the registry entry
func applyDashboardCard(e *gateway.PlatformEntry) {
	e.DashboardDescription = "Use Icebath in Matrix rooms and direct messages."
	e.DocsURL = "https://matrix.org/ecosystem/servers/"
	e.DashboardEnvVars = []string{"MATRIX_HOMESERVER", "MATRIX_ACCESS_TOKEN", "MATRIX_USER_ID", "MATRIX_ALLOWED_USERS"}
	e.DashboardRequiredEnv = []string{"MATRIX_HOMESERVER", "MATRIX_ACCESS_TOKEN", "MATRIX_USER_ID"}
}

// Registers the card of the bundled matrix directory for dashboards whose registry holds no matrix entry
func init() { gateway.RegisterBundledDashboardCard("matrix", applyDashboardCard) }
