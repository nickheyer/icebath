package telegram

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func applyEnv(t *testing.T, cfg *gateway.GatewayConfig) {
	t.Helper()
	for _, step := range envOverrides {
		if err := step.Apply(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func emptyConfig(t *testing.T) *gateway.GatewayConfig {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}
}

func TestTelegramEnvFallbackIPsCreateEntry(t *testing.T) {
	cfg := emptyConfig(t)
	t.Setenv("TELEGRAM_FALLBACK_IPS", "1.1.1.1, 2.2.2.2,")
	t.Setenv("TELEGRAM_REPLY_TO_MODE", "ALL")
	applyEnv(t, cfg)
	pc := cfg.Platforms[gateway.PlatformTelegram]
	ips, _ := pc.Extra["fallback_ips"].([]any)
	if pc.Enabled || len(ips) != 2 || ips[0] != "1.1.1.1" || ips[1] != "2.2.2.2" || pc.ReplyToMode != "all" {
		t.Fatalf("telegram config %+v", pc)
	}
}
