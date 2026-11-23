package yuanbao

import (
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
)

var envEnableCredentials = []string{"YUANBAO_APP_ID", "YUANBAO_APP_SECRET"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformYuanbao, Creds: [][]string{{"YUANBAO_APP_ID", "YUANBAO_APP_KEY"}, {"YUANBAO_APP_SECRET"}},
		Fixed: []gateway.EnvSpec{{Key: "app_id", AnyOf: []string{"YUANBAO_APP_ID", "YUANBAO_APP_KEY"}}, {Key: "app_secret", Env: "YUANBAO_APP_SECRET"}},
		Optional: []gateway.EnvSpec{
			{Key: "bot_id", Env: "YUANBAO_BOT_ID"},
		},
		Home: "YUANBAO_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "YUANBAO_ALLOWED_USERS", "allow_all_env": "YUANBAO_ALLOW_ALL_USERS",
	"own_policy_env": []string{"YUANBAO_DM_POLICY", "YUANBAO_GROUP_POLICY"}}

// Registers the env declarations of the bundled yuanbao directory and the settings its access names moved to
func init() {
	gateway.RegisterBundledEnv("yuanbao", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
	gateway.RegisterEnvSettings(map[string]settings.Entry{"YUANBAO_DM_POLICY": settings.YuanbaoDmPolicy, "YUANBAO_GROUP_POLICY": settings.YuanbaoGroupPolicy,
		"YUANBAO_DM_ALLOW_FROM": settings.YuanbaoDmAllowFrom, "YUANBAO_GROUP_ALLOW_FROM": settings.YuanbaoGroupAllowFrom})
}
