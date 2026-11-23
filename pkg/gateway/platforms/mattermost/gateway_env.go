package mattermost

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"MATTERMOST_TOKEN"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformMattermost, Creds: [][]string{{"MATTERMOST_TOKEN"}}, Token: "MATTERMOST_TOKEN"},
	gateway.HomeStep{Platform: gateway.PlatformMattermost, EnvBase: "MATTERMOST_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "MATTERMOST_ALLOWED_USERS", "allow_all_env": "MATTERMOST_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled mattermost directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("mattermost", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
