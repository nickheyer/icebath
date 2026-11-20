package bluebubbles

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"BLUEBUBBLES_SERVER_URL", "BLUEBUBBLES_PASSWORD"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformBlueBubbles, Creds: [][]string{{"BLUEBUBBLES_SERVER_URL"}, {"BLUEBUBBLES_PASSWORD"}},
		Fixed: []gateway.EnvSpec{
			{Key: "server_url", Env: "BLUEBUBBLES_SERVER_URL", Fn: gateway.StripSlash}, {Key: "password", Env: "BLUEBUBBLES_PASSWORD"},
			{Key: "webhook_host", Env: "BLUEBUBBLES_WEBHOOK_HOST", Default: "127.0.0.1"},
			{Key: "webhook_port", Env: "BLUEBUBBLES_WEBHOOK_PORT", Fn: gateway.IntOr(8645)},
			{Key: "webhook_path", Env: "BLUEBUBBLES_WEBHOOK_PATH", Default: "/bluebubbles-webhook"},
			{Key: "send_read_receipts", Env: "BLUEBUBBLES_SEND_READ_RECEIPTS", Default: "true", Fn: gateway.TruthyValue},
			{Key: "require_mention", Env: "BLUEBUBBLES_REQUIRE_MENTION", Fn: gateway.TruthyToken},
		},
		Optional: []gateway.EnvSpec{{Key: "mention_patterns", Env: "BLUEBUBBLES_MENTION_PATTERNS", Fn: gateway.MentionPatterns}}},
	gateway.HomeStep{Platform: gateway.PlatformBlueBubbles, EnvBase: "BLUEBUBBLES_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "BLUEBUBBLES_ALLOWED_USERS", "allow_all_env": "BLUEBUBBLES_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled bluebubbles directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("bluebubbles", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
