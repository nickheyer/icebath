package telegram

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Writes the csv list of fallback ips into the telegram extras, creating the entry when absent
func telegramFallbackIPs(ctx context.Context, cfg *gateway.GatewayConfig) error {
	ips := gateway.EnvFirst(ctx, []string{"TELEGRAM_FALLBACK_IPS"})
	if ips == "" {
		return nil
	}
	pc, ok := cfg.Platforms[gateway.PlatformTelegram]
	if !ok {
		pc = gateway.NewPlatformConfig()
		cfg.Platforms[gateway.PlatformTelegram] = pc
	}
	pc.Extra["fallback_ips"] = gateway.CSVList(ips)
	return nil
}

var envEnableCredentials = []string{"TELEGRAM_BOT_TOKEN"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformTelegram, Creds: [][]string{{"TELEGRAM_BOT_TOKEN"}}, Token: "TELEGRAM_BOT_TOKEN"},
	gateway.ReplyModeStep{Platform: gateway.PlatformTelegram, Env: "TELEGRAM_REPLY_TO_MODE"},
	gateway.EnvStepFunc(telegramFallbackIPs),
	gateway.HomeStep{Platform: gateway.PlatformTelegram, EnvBase: "TELEGRAM_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "TELEGRAM_ALLOWED_USERS", "allow_all_env": "TELEGRAM_ALLOW_ALL_USERS", "allow_bots_env": "TELEGRAM_ALLOW_BOTS", "group_allowed_chats_env": "TELEGRAM_GROUP_ALLOWED_CHATS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled telegram directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("telegram", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
