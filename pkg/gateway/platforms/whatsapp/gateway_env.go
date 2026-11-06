package whatsapp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Registers the env declarations of the bundled whatsapp directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("whatsapp", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
