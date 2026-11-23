package feishu

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

var envEnableCredentials = []string{"FEISHU_APP_ID", "FEISHU_APP_SECRET"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformFeishu, Creds: [][]string{{"FEISHU_APP_ID"}, {"FEISHU_APP_SECRET"}},
		Fixed:    []gateway.EnvSpec{{Key: "app_id", Env: "FEISHU_APP_ID"}, {Key: "app_secret", Env: "FEISHU_APP_SECRET"}},
		Optional: []gateway.EnvSpec{{Key: "encrypt_key", Env: "FEISHU_ENCRYPT_KEY"}, {Key: "verification_token", Env: "FEISHU_VERIFICATION_TOKEN"}},
		Home:     "FEISHU_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "FEISHU_ALLOWED_USERS", "allow_all_env": "FEISHU_ALLOW_ALL_USERS", "allow_bots_env": "FEISHU_ALLOW_BOTS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled feishu directory and the setting its bot policy name moved to
func init() {
	gateway.RegisterBundledEnv("feishu", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
	gateway.RegisterEnvSettings(map[string]settings.Entry{"FEISHU_ALLOW_BOTS": settings.FeishuAllowBots})
}
