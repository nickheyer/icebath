package weixin

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
)

var envEnableCredentials = []string{"WEIXIN_TOKEN", "WEIXIN_ACCOUNT_ID"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformWeixin, Creds: [][]string{{"WEIXIN_TOKEN", "WEIXIN_ACCOUNT_ID"}}, Token: "WEIXIN_TOKEN",
		Optional: []gateway.EnvSpec{{Key: "account_id", Env: "WEIXIN_ACCOUNT_ID"}},
		OptionalStripped: []gateway.EnvSpec{
			{Key: "base_url", Env: "WEIXIN_BASE_URL", Fn: gateway.StripSlash}, {Key: "cdn_base_url", Env: "WEIXIN_CDN_BASE_URL", Fn: gateway.StripSlash},
			{Key: "dm_policy", Env: "WEIXIN_DM_POLICY", Fn: lower}, {Key: "group_policy", Env: "WEIXIN_GROUP_POLICY", Fn: lower},
			{Key: "allow_from", Env: "WEIXIN_ALLOWED_USERS"}, {Key: "group_allow_from", Env: "WEIXIN_GROUP_ALLOWED_USERS"},
			{Key: "split_multiline_messages", Env: "WEIXIN_SPLIT_MULTILINE_MESSAGES"},
		},
		Home: "WEIXIN_HOME_CHANNEL", HomeStrip: true},
}

// Lowercased env value
func lower(value string) any { return strings.ToLower(value) }

var authz = map[string]any{"allowed_users_env": "WEIXIN_ALLOWED_USERS", "allow_all_env": "WEIXIN_ALLOW_ALL_USERS",
	"own_policy_env": []string{"WEIXIN_DM_POLICY", "WEIXIN_GROUP_POLICY"}, "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled weixin directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("weixin", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
