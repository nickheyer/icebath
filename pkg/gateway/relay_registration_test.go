package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Clean relay env with no relay entry before and after the case
func relayCleanRegistry(t *testing.T) string {
	t.Helper()
	h := relayCleanEnv(t)
	Registry.Unregister(context.Background(), "relay", "")
	t.Cleanup(func() { Registry.Unregister(context.Background(), "relay", "") })
	return h
}

func TestOffWhenNoURLConfigured(t *testing.T) {
	relayCleanRegistry(t)
	ctx := context.Background()
	registered, err := RegisterRelayAdapter(ctx)
	if err != nil || registered || Registry.IsRegistered(ctx, "relay") {
		t.Fatalf("registered %v err %v", registered, err)
	}
}

func TestRegistersWhenURLConfigured(t *testing.T) {
	relayCleanRegistry(t)
	ctx := context.Background()
	t.Setenv("GATEWAY_RELAY_URL", "wss://connector.example/relay")
	if got := RelayURL(ctx); got != "wss://connector.example/relay" {
		t.Fatalf("relay url %q", got)
	}
	registered, err := RegisterRelayAdapter(ctx)
	if err != nil || !registered || !Registry.IsRegistered(ctx, "relay") {
		t.Fatalf("registered %v err %v", registered, err)
	}
}

// Enabled telegram home and a profile env file holding the text, under an active multiplex scope
func relayMultiplexScope(t *testing.T, profileEnv string) context.Context {
	t.Helper()
	h := relayCleanRegistry(t)
	relayWriteConfig(t, h, "gateway:\n  platforms:\n    telegram:\n      enabled: true\n      bot_token: '123:abc'\n")
	profileDir := filepath.Join(t.TempDir(), "profile-a")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, ".env"), []byte(profileEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	return provider.WithSecretScope(context.Background(), provider.BuildProfileSecretScope(profileDir), profileDir)
}

func TestConfigRegistrationAgreementUnderMultiplexScopeProcessStampAgreesOnBothSidesUnderScope(t *testing.T) {
	ctx := relayMultiplexScope(t, "")
	t.Setenv("GATEWAY_RELAY_URL", "wss://deploy.example/relay")
	config, err := LoadGatewayConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := RelayURL(ctx); got != "wss://deploy.example/relay" {
		t.Fatalf("relay url %q", got)
	}
	if registered, err := RegisterRelayAdapter(ctx); err != nil || !registered {
		t.Fatalf("registered %v err %v", registered, err)
	}
	provider.SetMultiplexActive(false)
	relayCfg, telegramCfg := config.Platforms[PlatformRelay], config.Platforms[PlatformTelegram]
	if relayCfg == nil || !relayCfg.Enabled || telegramCfg == nil || telegramCfg.Enabled {
		t.Fatalf("relay %+v telegram %+v", relayCfg, telegramCfg)
	}
	adapter, ok := Registry.CreateAdapter(context.Background(), "relay", relayCfg).(*RelayAdapter)
	if !ok || adapter.transport == nil {
		t.Fatalf("adapter %#v", adapter)
	}
}

func TestConfigRegistrationAgreementUnderMultiplexScopeProfileOnlyStampIsInertOnBothSidesUnderScope(t *testing.T) {
	ctx := relayMultiplexScope(t, "GATEWAY_RELAY_URL=wss://profile.example/relay\n")
	config, err := LoadGatewayConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := RelayURL(ctx); got != "" {
		t.Fatalf("relay url %q", got)
	}
	if registered, err := RegisterRelayAdapter(ctx); err != nil || registered {
		t.Fatalf("registered %v err %v", registered, err)
	}
	provider.SetMultiplexActive(false)
	if relayCfg := config.Platforms[PlatformRelay]; relayCfg != nil && relayCfg.Enabled {
		t.Fatalf("relay half enabled %+v", relayCfg)
	}
	if telegramCfg := config.Platforms[PlatformTelegram]; telegramCfg == nil || !telegramCfg.Enabled {
		t.Fatalf("telegram %+v", telegramCfg)
	}
}
