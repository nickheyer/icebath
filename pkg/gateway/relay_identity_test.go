package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var relayDisableSpellings = map[string]string{
	"top":              "platforms:\n  relay:\n    enabled: false\n",
	"nested":           "gateway:\n  platforms:\n    relay:\n      enabled: false\n",
	"string-false":     "platforms:\n  relay:\n    enabled: 'false'\n",
	"top-beats-nested": "platforms:\n  relay:\n    enabled: false\ngateway:\n  platforms:\n    relay:\n      enabled: true\n",
	"managed":          "",
}

// Temp profile with the relay env of the explicit disable suite
func relayProfile(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GATEWAY_RELAY_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(h, "no-managed"))
	t.Setenv("GATEWAY_RELAY_URL", "wss://connector.example/relay")
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "slack")
	t.Setenv("GATEWAY_RELAY_IDP_TOKEN_URL", "https://identity.example/token")
	t.Setenv("SLACK_BOT_TOKEN", "native-test-token")
	return h
}

func writeRelayDisable(t *testing.T, h, spelling string) {
	t.Helper()
	doc := relayDisableSpellings[spelling]
	if doc == "" {
		managed := filepath.Join(h, "managed")
		mkdirAllT(t, managed)
		if err := os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("platforms:\n  relay:\n    enabled: false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_MANAGED_DIR", managed)
		return
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRelayPredicateAgreesWithLoaderAndKeepsNativeAdapters(t *testing.T) {
	for spelling := range relayDisableSpellings {
		t.Run(spelling, func(t *testing.T) {
			h := relayProfile(t)
			writeRelayDisable(t, h, spelling)
			ctx := context.Background()
			cfg, err := LoadGatewayConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if disabled, err := RelayExplicitlyDisabled(ctx); err != nil || !disabled {
				t.Fatalf("predicate %v %v", disabled, err)
			}
			if relay := cfg.Platforms[PlatformRelay]; relay == nil || relay.Enabled {
				t.Fatalf("loader relay %+v", relay)
			}
			if got := RelayFrontedPlatforms(ctx); len(got) != 0 {
				t.Fatalf("disabled relay fronts %v", got)
			}
		})
	}
}

func TestRelayURLOnlyActivationFrontsTheEnvPlatforms(t *testing.T) {
	h := relayProfile(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platforms:\n  relay:\n    extra:\n      note: kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := RelayFrontedPlatforms(ctx); len(got) != 1 || !got["slack"] {
		t.Fatalf("fronted %v", got)
	}
}

func TestRelayLegacyJSONDisableIsAdvisoryLikeOtherPlatforms(t *testing.T) {
	h := relayProfile(t)
	if err := os.WriteFile(filepath.Join(h, "gateway.json"), []byte(`{"platforms": {"relay": {"enabled": false}, "telegram": {"enabled": false}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "native-telegram-test-token")
	t.Setenv("GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "true")
	ctx := context.Background()
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if disabled, err := RelayExplicitlyDisabled(ctx); err != nil || disabled {
		t.Fatalf("legacy json must stay advisory %v %v", disabled, err)
	}
	if relay := cfg.Platforms[PlatformRelay]; relay == nil || !relay.Enabled {
		t.Fatalf("loader must keep relay enabled %+v", relay)
	}
}

func TestRelayMalformedUserYAMLDropsTheYAMLLayerForBothReaders(t *testing.T) {
	h := relayProfile(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platforms: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRelayDisable(t, h, "managed")
	ctx := context.Background()
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if disabled, err := RelayExplicitlyDisabled(ctx); err != nil || disabled {
		t.Fatalf("malformed yaml predicate %v %v", disabled, err)
	}
	if !cfg.Platforms[PlatformRelay].Enabled {
		t.Fatal("loader must keep relay enabled")
	}
}

func TestRelayPlatformIdentitiesReadBotIDs(t *testing.T) {
	relayProfile(t)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", " discord, ,slack,teams ")
	t.Setenv("GATEWAY_RELAY_BOT_IDS", `{"discord": {"botId": " 42 "}, "slack": "flat", "teams": {}}`)
	got := RelayPlatformIdentities()
	want := [][2]string{{"discord", "42"}, {"slack", ""}, {"teams", ""}}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("identities %v", got)
	}
	t.Setenv("GATEWAY_RELAY_BOT_IDS", "{not json")
	if got := RelayPlatformIdentities(); got[0] != [2]string{"discord", ""} {
		t.Fatalf("malformed ids %v", got)
	}
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "")
	if got := RelayPlatformIdentities(); len(got) != 1 || got[0] != [2]string{"relay", ""} {
		t.Fatalf("fallback %v", got)
	}
	if got := RelayFrontedPlatforms(context.Background()); len(got) != 0 {
		t.Fatalf("generic relay is not a fronted platform %v", got)
	}
}
