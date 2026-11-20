package email

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformEmail, Creds: [][]string{{"EMAIL_ADDRESS"}, {"EMAIL_PASSWORD"}, {"EMAIL_IMAP_HOST"}, {"EMAIL_SMTP_HOST"}},
		Fixed: []gateway.EnvSpec{{Key: "address", Env: "EMAIL_ADDRESS"}, {Key: "imap_host", Env: "EMAIL_IMAP_HOST"}, {Key: "smtp_host", Env: "EMAIL_SMTP_HOST"}}},
	gateway.HomeStep{Platform: gateway.PlatformEmail, EnvBase: "EMAIL_HOME_ADDRESS"},
}

var authz = map[string]any{"allowed_users_env": "EMAIL_ALLOWED_USERS", "allow_all_env": "EMAIL_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled email directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("email", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
