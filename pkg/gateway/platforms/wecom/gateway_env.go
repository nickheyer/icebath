package wecom

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"WECOM_BOT_ID", "WECOM_SECRET"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformWeCom, Creds: [][]string{{"WECOM_BOT_ID"}, {"WECOM_SECRET"}},
		Fixed:    []gateway.EnvSpec{{Key: "bot_id", Env: "WECOM_BOT_ID"}, {Key: "secret", Env: "WECOM_SECRET"}},
		Optional: []gateway.EnvSpec{{Key: "websocket_url", Env: "WECOM_WEBSOCKET_URL"}},
		Home:     "WECOM_HOME_CHANNEL"},
}

var callbackEnvEnableCredentials = []string{"WECOM_CALLBACK_CORP_ID", "WECOM_CALLBACK_CORP_SECRET"}

var callbackEnvOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformWeComCallback, Creds: [][]string{{"WECOM_CALLBACK_CORP_ID"}, {"WECOM_CALLBACK_CORP_SECRET"}},
		Fixed: []gateway.EnvSpec{
			{Key: "corp_id", Env: "WECOM_CALLBACK_CORP_ID"}, {Key: "corp_secret", Env: "WECOM_CALLBACK_CORP_SECRET"},
			{Key: "agent_id", Env: "WECOM_CALLBACK_AGENT_ID"}, {Key: "token", Env: "WECOM_CALLBACK_TOKEN"},
			{Key: "encoding_aes_key", Env: "WECOM_CALLBACK_ENCODING_AES_KEY"},
			{Key: "host", Env: "WECOM_CALLBACK_HOST"}, {Key: "port", Env: "WECOM_CALLBACK_PORT", Fn: gateway.IntOr(8645)},
		}},
}

var authz = map[string]any{"allowed_users_env": "WECOM_ALLOWED_USERS", "allow_all_env": "WECOM_ALLOW_ALL_USERS",
	"own_policy_env": []string{"WECOM_DM_POLICY", "WECOM_GROUP_POLICY"}, "allowlist_suppresses_pairing": true}

var callbackAuthz = map[string]any{"allowed_users_env": "WECOM_CALLBACK_ALLOWED_USERS", "allow_all_env": "WECOM_CALLBACK_ALLOW_ALL_USERS",
	"allowlist_suppresses_pairing": true}

// Registers the wecom and wecom callback env declarations with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("wecom", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
	gateway.RegisterBundledEnv("wecom", "CALLBACK_", gateway.BundledEnvRows{Overrides: callbackEnvOverrides, EnableCredentials: callbackEnvEnableCredentials, Authz: callbackAuthz})
}
