package whatsapp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Disables a configured entry on a false flag and enables from env on a truthy one without beating a yaml disable
func whatsappEnabledFlag(ctx context.Context, cfg *gateway.GatewayConfig) error {
	raw := gateway.EnvFirst(ctx, []string{"WHATSAPP_ENABLED"})
	pc, ok := cfg.Platforms[gateway.PlatformWhatsApp]
	switch lowered := pystr.Lower(raw); {
	case ok && pc != nil && (lowered == "false" || lowered == "0" || lowered == "no"):
		pc.Enabled = false
	case gateway.TruthyValue(raw).(bool):
		gateway.EnableFromEnv(ctx, cfg, gateway.PlatformWhatsApp)
	}
	return nil
}

var envEnableCredentials = []string{"WHATSAPP_ENABLED"}

var envOverrides = []gateway.EnvStep{
	gateway.EnvStepFunc(whatsappEnabledFlag),
	gateway.HomeStep{Platform: gateway.PlatformWhatsApp, EnvBase: "WHATSAPP_HOME_CHANNEL"},
}

var authz = map[string]any{"allowed_users_env": "WHATSAPP_ALLOWED_USERS", "allow_all_env": "WHATSAPP_ALLOW_ALL_USERS",
	"own_policy_env": []string{"WHATSAPP_DM_POLICY", "WHATSAPP_GROUP_POLICY"}, "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled whatsapp directory with the gateway config bridge, and the settings its names moved to
func init() {
	gateway.RegisterBundledEnv("whatsapp", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
	gateway.RegisterEnvSettings(map[string]settings.Entry{"WHATSAPP_ALLOWED_USERS": settings.WhatsappAllowFrom,
		"WHATSAPP_ALLOW_FROM": settings.WhatsappAllowFrom, "WHATSAPP_GROUP_ALLOWED_USERS": settings.WhatsappGroupAllowFrom,
		"WHATSAPP_GROUP_ALLOW_FROM": settings.WhatsappGroupAllowFrom, "WHATSAPP_DM_POLICY": settings.WhatsappDmPolicy,
		"WHATSAPP_GROUP_POLICY": settings.WhatsappGroupPolicy, "WHATSAPP_REQUIRE_MENTION": settings.WhatsappRequireMention,
		"WHATSAPP_MENTION_PATTERNS": settings.WhatsappMentionPatterns, "WHATSAPP_FREE_RESPONSE_CHATS": settings.WhatsappFreeResponseChats})
}
