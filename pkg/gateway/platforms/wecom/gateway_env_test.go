package wecom

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

func TestWeComCallbackEnvPortDefault(t *testing.T) {
	cfg := emptyConfig(t)
	t.Setenv("WECOM_CALLBACK_CORP_ID", "corp")
	t.Setenv("WECOM_CALLBACK_CORP_SECRET", "sec")
	t.Setenv("WECOM_CALLBACK_PORT", "bad")
	for _, step := range callbackEnvOverrides {
		if err := step.Apply(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	pc := cfg.Platforms[gateway.PlatformWeComCallback]
	if !pc.Enabled || pc.Extra["port"] != 8645 || pc.Extra["corp_id"] != "corp" || pc.Extra["host"] != "" {
		t.Fatalf("callback config %+v", pc.Extra)
	}
	applyEnv(t, cfg)
	if _, ok := cfg.Platforms[gateway.PlatformWeCom]; ok {
		t.Fatal("bot entry enabled without credentials")
	}
}
