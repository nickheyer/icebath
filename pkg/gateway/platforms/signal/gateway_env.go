package signal

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"SIGNAL_HTTP_URL"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformSignal, Creds: [][]string{{"SIGNAL_HTTP_URL"}, {"SIGNAL_ACCOUNT"}},
		Fixed: []gateway.EnvSpec{
			{Key: "http_url", Env: "SIGNAL_HTTP_URL"}, {Key: "account", Env: "SIGNAL_ACCOUNT"},
			{Key: "ignore_stories", Env: "SIGNAL_IGNORE_STORIES", Default: "true", Fn: gateway.TruthyValue},
		}},
	gateway.HomeStep{Platform: gateway.PlatformSignal, EnvBase: "SIGNAL_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "SIGNAL_ALLOWED_USERS", "allow_all_env": "SIGNAL_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled signal directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("signal", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
