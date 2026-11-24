package weixin

import (
	"github.com/nickheyer/icebath/pkg/gateway"
)

var envEnableCredentials = []string{"WEIXIN_TOKEN", "WEIXIN_ACCOUNT_ID"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformWeixin, Creds: [][]string{{"WEIXIN_TOKEN", "WEIXIN_ACCOUNT_ID"}}, Token: "WEIXIN_TOKEN",
		Optional: []gateway.EnvSpec{{Key: "account_id", Env: "WEIXIN_ACCOUNT_ID"}},
		Home:     "WEIXIN_HOME_CHANNEL", HomeStrip: true},
}

var authz = map[string]any{"allowed_users_env": "WEIXIN_ALLOWED_USERS", "allow_all_env": "WEIXIN_ALLOW_ALL_USERS",
	"own_policy_env": []string{"WEIXIN_DM_POLICY", "WEIXIN_GROUP_POLICY"}, "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled weixin directory
func init() {
	gateway.RegisterBundledEnv("weixin", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
