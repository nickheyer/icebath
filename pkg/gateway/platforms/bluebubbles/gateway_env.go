package bluebubbles

import (
	"github.com/nickheyer/icebath/pkg/gateway"
)

var envEnableCredentials = []string{"BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformBlueBubbles, Creds: [][]string{{"BLUEBUBBLES_SERVER_URL"}, {"BLUEBUBBLES_PASSWORD"}},
		Fixed: []gateway.EnvSpec{{Key: "server_url", Env: "BLUEBUBBLES_SERVER_URL", Fn: gateway.StripSlash}, {Key: "password", Env: "BLUEBUBBLES_PASSWORD"}}},
	gateway.HomeStep{Platform: gateway.PlatformBlueBubbles, EnvBase: "BLUEBUBBLES_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "BLUEBUBBLES_ALLOWED_USERS", "allow_all_env": "BLUEBUBBLES_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled bluebubbles directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("bluebubbles", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
