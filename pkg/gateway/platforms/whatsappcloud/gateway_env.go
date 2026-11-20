package whatsappcloud

import "github.com/nickheyer/icebath/pkg/gateway"

var envEnableCredentials = []string{"WHATSAPP_CLOUD_PHONE_NUMBER_ID", "WHATSAPP_CLOUD_ACCESS_TOKEN"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformWhatsAppCloud, Creds: [][]string{{"WHATSAPP_CLOUD_PHONE_NUMBER_ID"}, {"WHATSAPP_CLOUD_ACCESS_TOKEN"}},
		Fixed: []gateway.EnvSpec{{Key: "phone_number_id", Env: "WHATSAPP_CLOUD_PHONE_NUMBER_ID"}, {Key: "access_token", Env: "WHATSAPP_CLOUD_ACCESS_TOKEN"}},
		Optional: []gateway.EnvSpec{
			{Key: "app_id", Env: "WHATSAPP_CLOUD_APP_ID"}, {Key: "app_secret", Env: "WHATSAPP_CLOUD_APP_SECRET"},
			{Key: "waba_id", Env: "WHATSAPP_CLOUD_WABA_ID"}, {Key: "verify_token", Env: "WHATSAPP_CLOUD_VERIFY_TOKEN"},
			{Key: "webhook_host", Env: "WHATSAPP_CLOUD_WEBHOOK_HOST"}, {Key: "webhook_port", Env: "WHATSAPP_CLOUD_WEBHOOK_PORT", Int: true},
			{Key: "webhook_path", Env: "WHATSAPP_CLOUD_WEBHOOK_PATH"}, {Key: "api_version", Env: "WHATSAPP_CLOUD_API_VERSION"},
		}},
	gateway.HomeStep{Platform: gateway.PlatformWhatsAppCloud, EnvBase: "WHATSAPP_CLOUD_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "WHATSAPP_CLOUD_ALLOWED_USERS", "allow_all_env": "WHATSAPP_CLOUD_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled whatsapp_cloud directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("whatsapp_cloud", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
