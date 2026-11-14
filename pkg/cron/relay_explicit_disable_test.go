package cron_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
)

func TestPredicateAgreesWithLoaderAndKeepsNativeAdapters(t *testing.T) {
	for spelling, doc := range map[string]string{
		"top":              "platforms:\n  relay:\n    enabled: false\n",
		"nested":           "gateway:\n  platforms:\n    relay:\n      enabled: false\n",
		"string-false":     "platforms:\n  relay:\n    enabled: 'false'\n",
		"top-beats-nested": "platforms:\n  relay:\n    enabled: false\ngateway:\n  platforms:\n    relay:\n      enabled: true\n",
		"managed":          "",
	} {
		t.Run(spelling, func(t *testing.T) {
			h := filepath.Join(t.TempDir(), ".icebath")
			if err := os.MkdirAll(filepath.Join(h, "cron"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", h)
			s := cron.OpenStoreForTest(h)
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
			if doc == "" {
				managed := filepath.Join(h, "managed")
				if err := os.MkdirAll(managed, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("platforms:\n  relay:\n    enabled: false\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv("ICEBATH_MANAGED_DIR", managed)
			} else if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			cfg, err := gateway.LoadGatewayConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if disabled, err := gateway.RelayExplicitlyDisabled(ctx); err != nil || !disabled {
				t.Fatalf("predicate %v %v", disabled, err)
			}
			if relay := cfg.Platforms[gateway.PlatformRelay]; relay == nil || relay.Enabled {
				t.Fatalf("loader relay %+v", relay)
			}
			slack := cfg.Platforms[gateway.PlatformSlack]
			if slack == nil || !slack.Enabled {
				t.Fatalf("loader slack %+v", slack)
			}
			pconfig, reason := s.ResolvedTransportConfigForTest(ctx, gateway.PlatformSlack, "slack", "channel", map[string]any{}, cfg)
			if reason != "" || pconfig != slack {
				t.Fatalf("resolved %+v reason %q", pconfig, reason)
			}
		})
	}
}
