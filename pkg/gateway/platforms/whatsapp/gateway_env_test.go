package whatsapp

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

func TestWhatsAppEnvFlagEnablesAndDisables(t *testing.T) {
	cfg := emptyConfig(t)
	t.Setenv("WHATSAPP_ENABLED", "false")
	applyEnv(t, cfg)
	if len(cfg.Platforms) != 0 {
		t.Fatalf("false flag created %+v", cfg.Platforms)
	}
	t.Setenv("WHATSAPP_ENABLED", "true")
	t.Setenv("WHATSAPP_HOME_CHANNEL", "123@s.whatsapp.net")
	applyEnv(t, cfg)
	pc := cfg.Platforms[gateway.PlatformWhatsApp]
	if pc == nil || !pc.Enabled || pc.HomeChannel == nil || pc.HomeChannel.ChatID != "123@s.whatsapp.net" {
		t.Fatalf("true flag %+v", pc)
	}
	t.Setenv("WHATSAPP_ENABLED", "No")
	applyEnv(t, cfg)
	if cfg.Platforms[gateway.PlatformWhatsApp].Enabled {
		t.Fatal("no flag left the entry enabled")
	}
}
