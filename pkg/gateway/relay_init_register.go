package gateway

import "context"

// Registers the relay platform when a connector URL resolves, never over an explicit profile disable
func RegisterRelayAdapter(ctx context.Context) (bool, error) {
	return RegisterRelayAdapterWith(ctx, false, nil)
}

// Registers the relay platform for a resolved or given URL, a forced one without a URL building a transportless adapter
func RegisterRelayAdapterWith(ctx context.Context, force bool, url *string) (bool, error) {
	disabled, err := RelayExplicitlyDisabled(ctx)
	if err != nil || disabled {
		return false, err
	}
	resolvedURL := ""
	if url != nil {
		resolvedURL = *url
	} else if resolvedURL, err = RelayURLChecked(ctx); err != nil {
		return false, err
	}
	if !force && resolvedURL == "" {
		return false, nil
	}
	identity := RelayPlatformIdentity()
	platform, botID := identity[0], identity[1]
	factory := func(fctx context.Context, config *PlatformConfig) (any, error) {
		placeholder := NewCapabilityDescriptor(RelayContractVersion, platform, "Relay", 4096, false, true, false, "plain", "chars")
		var transport RelayTransportConn
		if resolvedURL != "" {
			gatewayID, upgradeSecret := RelayConnectionAuth(fctx)
			transport = NewWebSocketRelayTransport(resolvedURL, platform, botID, RelayTransportOptions{
				Identities: RelayPlatformIdentities(), GatewayID: gatewayID, UpgradeSecret: upgradeSecret, Reconnect: true})
		}
		return NewRelayAdapter(config, placeholder, transport), nil
	}
	entry := &PlatformEntry{Name: "relay", Label: "Relay", AdapterFactory: factory, CheckFn: func() bool { return true },
		Source: "builtin", Emoji: "\U0001f50c"}
	if err := Registry.Register(ctx, entry, ""); err != nil {
		return false, err
	}
	return true, nil
}
