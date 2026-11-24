package gateway

import (
	"strings"

	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
)

const relayStockAgentName = "Icebath Agent"

// Effective connector URL, empty when unset or vetoed by an explicit platform disable
func RelayURLChecked(ctx context.Context) (string, error) {
	disabled, err := RelayExplicitlyDisabled(ctx)
	if err != nil || disabled {
		return "", err
	}
	if u := relaySettingURL(ctx, settings.GatewayRelayURL); u != nil {
		return *u, nil
	}
	return "", nil
}

// Effective connector URL raising a malformed relay config the way the uncaught Python exception does
func RelayURL(ctx context.Context) string {
	u, err := RelayURLChecked(ctx)
	if err != nil {
		panic(err)
	}
	return u
}

// Primary platform and bot id pair, the first of the fronted identities
func RelayPlatformIdentity() [2]string {
	return RelayPlatformIdentities()[0]
}

// Enrolled gateway id and upgrade secret, both empty when either is absent
func RelayConnectionAuth(ctx context.Context) (string, string) {
	return relaySetting(ctx, settings.GatewayRelayID), relaySetting(ctx, settings.GatewayRelaySecret)
}

// Public inbound URL asserted at provision, empty for an outbound only gateway
func RelayEndpoint(ctx context.Context) string {
	if u := relaySettingURL(ctx, settings.GatewayRelayEndpoint); u != nil {
		return *u
	}
	return ""
}

// Route discriminators of gateway.relay_route_keys (or GATEWAY_RELAY_ROUTE_KEYS), each trimmed, empty when none
func RelayRouteKeys(ctx context.Context) []string {
	keys := SettingList(ctx, settings.GatewayRelayRouteKeys)
	if keys == nil {
		return []string{}
	}
	return keys
}

// Stable instance id forwarded at provision, empty when unset
func RelayInstanceID(ctx context.Context) string {
	return relaySetting(ctx, settings.GatewayRelayInstanceID)
}

// Customized agent display name capped at 64 characters, empty for the stock brand or when unset
func RelayDisplayName(ctx context.Context) string {
	value := relaySetting(ctx, settings.GatewayRelayDisplayName)
	if value == "" {
		value = relaySkinAgentName(ctx)
		if value == relayStockAgentName {
			value = ""
		}
	}
	return pystr.Slice(value, 0, 64)
}

// Branded agent name of the active skin, empty when the skin cannot be read
func relaySkinAgentName(ctx context.Context) (name string) {
	defer func() {
		if recover() != nil {
			name = ""
		}
	}()
	skin := config.GetActiveSkin(ctx)
	if skin == nil {
		return ""
	}
	return strings.TrimSpace(skin.GetBranding("agent_name", ""))
}

// HTTP connector route of a ws or wss dial URL
func relayConnectorURL(relayDialURL, path string) string {
	return RelayMediaBaseURL(relayDialURL) + path
}

// Provision route of a dial URL
func relayProvisionURL(relayDialURL string) string {
	return relayConnectorURL(relayDialURL, "/relay/provision")
}
