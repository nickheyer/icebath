package sms

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Stores the Twilio auth token as the platform api key
func smsAPIKey(ctx context.Context, cfg *gateway.GatewayConfig, pc *gateway.PlatformConfig) error {
	pc.APIKey = gateway.EnvFirst(ctx, []string{"TWILIO_AUTH_TOKEN"})
	return nil
}

var envEnableCredentials = []string{"TWILIO_ACCOUNT_SID"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformSMS, Creds: [][]string{{"TWILIO_ACCOUNT_SID"}}, Then: smsAPIKey},
	gateway.HomeStep{Platform: gateway.PlatformSMS, EnvBase: "SMS_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "SMS_ALLOWED_USERS", "allow_all_env": "SMS_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled sms directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("sms", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
