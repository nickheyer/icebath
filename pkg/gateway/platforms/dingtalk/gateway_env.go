package dingtalk

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"DINGTALK_CLIENT_ID", "DINGTALK_CLIENT_SECRET"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformDingTalk, Creds: [][]string{{"DINGTALK_CLIENT_ID"}, {"DINGTALK_CLIENT_SECRET"}},
		Fixed: []gateway.EnvSpec{{Key: "client_id", Env: "DINGTALK_CLIENT_ID"}, {Key: "client_secret", Env: "DINGTALK_CLIENT_SECRET"}},
		Home:  "DINGTALK_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "DINGTALK_ALLOWED_USERS", "allow_all_env": "DINGTALK_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled dingtalk directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("dingtalk", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
