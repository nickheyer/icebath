package gateway

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Gateway block of the effective user config, empty when absent or not a mapping
func relayGatewayCfg(ctx context.Context) map[string]any {
	block, _ := config.EffectiveUserConfig(ctx)["gateway"].(map[string]any)
	if block == nil {
		return map[string]any{}
	}
	return block
}

// Environment variable first then the gateway config key, stripped and empty when unset
func relayEnvOrCfg(ctx context.Context, envVar, cfgKey string) string {
	if value := pystr.Strip(os.Getenv(envVar)); value != "" {
		return value
	}
	raw := relayGatewayCfg(ctx)[cfgKey]
	if !pyrepr.Truthy(raw) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(raw))
}

// URL valued relay setting with trailing slashes stripped, nil when unset
func relayEnvOrCfgURL(ctx context.Context, envVar, cfgKey string) *string {
	value := pystr.RstripChars(relayEnvOrCfg(ctx, envVar, cfgKey), "/")
	if value == "" {
		return nil
	}
	return &value
}

// Wake URL the connector pokes to revive a suspended gateway, nil when none is registered
func RelayWakeURL(ctx context.Context) *string {
	return relayEnvOrCfgURL(ctx, "GATEWAY_RELAY_WAKE_URL", "relay_wake_url")
}
