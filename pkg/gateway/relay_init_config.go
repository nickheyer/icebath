package gateway

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

func relayLog() *slog.Logger { return slog.Default().With("logger", "gateway.relay") }

const relayStockAgentName = "Icebath Agent"

// Effective connector URL, empty when unset or vetoed by an explicit platform disable
func RelayURLChecked(ctx context.Context) (string, error) {
	disabled, err := RelayExplicitlyDisabled(ctx)
	if err != nil || disabled {
		return "", err
	}
	if u := relayEnvOrCfgURL(ctx, "GATEWAY_RELAY_URL", "relay_url"); u != nil {
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
	gatewayID := relayEnvOrCfg(ctx, "GATEWAY_RELAY_ID", "relay_id")
	secret := relayEnvOrCfg(ctx, "GATEWAY_RELAY_SECRET", "relay_secret")
	return gatewayID, secret
}

// Public inbound URL asserted at provision, empty for an outbound only gateway
func RelayEndpoint(ctx context.Context) string {
	if u := relayEnvOrCfgURL(ctx, "GATEWAY_RELAY_ENDPOINT", "relay_endpoint"); u != nil {
		return *u
	}
	return ""
}

// Route discriminators from the comma separated env, or the config list or comma string
func RelayRouteKeys(ctx context.Context) []string {
	raw := pystr.Strip(os.Getenv("GATEWAY_RELAY_ROUTE_KEYS"))
	if raw == "" {
		val := relayGatewayCfg(ctx)["relay_route_keys"]
		if list, ok := val.([]any); ok {
			keys := []string{}
			for _, k := range list {
				if s := pystr.Strip(pyrepr.Str(k)); s != "" {
					keys = append(keys, s)
				}
			}
			return keys
		}
		if !pyrepr.Truthy(val) {
			val = ""
		}
		raw = pystr.Strip(pyrepr.Str(val))
	}
	keys := []string{}
	for _, k := range strings.Split(raw, ",") {
		if s := pystr.Strip(k); s != "" {
			keys = append(keys, s)
		}
	}
	return keys
}

// Stable instance id forwarded at provision, empty when unset
func RelayInstanceID(ctx context.Context) string {
	return relayEnvOrCfg(ctx, "GATEWAY_RELAY_INSTANCE_ID", "relay_instance_id")
}

// Customized agent display name capped at 64 characters, empty for the stock brand or when unset
func RelayDisplayName(ctx context.Context) string {
	value := pystr.Strip(os.Getenv("GATEWAY_RELAY_DISPLAY_NAME"))
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
	return pystr.Strip(skin.GetBranding("agent_name", ""))
}

// HTTP connector route of a ws or wss dial URL
func relayConnectorURL(relayDialURL, path string) string {
	return RelayMediaBaseURL(relayDialURL) + path
}

// Provision route of a dial URL
func relayProvisionURL(relayDialURL string) string {
	return relayConnectorURL(relayDialURL, "/relay/provision")
}
