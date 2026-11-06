package yuanbao

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"YUANBAO_APP_ID", "YUANBAO_APP_SECRET"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformYuanbao, Creds: [][]string{{"YUANBAO_APP_ID", "YUANBAO_APP_KEY"}, {"YUANBAO_APP_SECRET"}},
		Fixed: []gateway.EnvSpec{{Key: "app_id", AnyOf: []string{"YUANBAO_APP_ID", "YUANBAO_APP_KEY"}}, {Key: "app_secret", Env: "YUANBAO_APP_SECRET"}},
		Optional: []gateway.EnvSpec{
			{Key: "bot_id", Env: "YUANBAO_BOT_ID"}, {Key: "ws_url", Env: "YUANBAO_WS_URL"},
			{Key: "api_domain", Env: "YUANBAO_API_DOMAIN"}, {Key: "route_env", Env: "YUANBAO_ROUTE_ENV"},
			{Key: "dm_policy", Env: "YUANBAO_DM_POLICY", Fn: gateway.StripLower}, {Key: "dm_allow_from", Env: "YUANBAO_DM_ALLOW_FROM"},
			{Key: "group_policy", Env: "YUANBAO_GROUP_POLICY", Fn: gateway.StripLower}, {Key: "group_allow_from", Env: "YUANBAO_GROUP_ALLOW_FROM"},
		},
		Home: "YUANBAO_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "YUANBAO_ALLOWED_USERS", "allow_all_env": "YUANBAO_ALLOW_ALL_USERS",
	"own_policy_env": []string{"YUANBAO_DM_POLICY", "YUANBAO_GROUP_POLICY"}, "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled yuanbao directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("yuanbao", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
