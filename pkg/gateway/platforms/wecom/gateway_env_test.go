package wecom

import (
	"context"
	"os"
	"path/filepath"
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

func TestWeComCallbackBadPortFallsBackToTheDefault(t *testing.T) {
	cfg := emptyConfig(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("wecom_callback:\n  corp_id: corp\n  port: bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WECOM_CALLBACK_CORP_SECRET", "sec")
	for _, step := range callbackEnvOverrides {
		if err := step.Apply(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	pc := cfg.Platforms[gateway.PlatformWeComCallback]
	if pc == nil || !pc.Enabled || pc.Extra["port"] != 8645 || pc.Extra["corp_id"] != "corp" || pc.Extra["host"] != "" {
		t.Fatalf("callback config %+v", pc)
	}
	applyEnv(t, cfg)
	if _, ok := cfg.Platforms[gateway.PlatformWeCom]; ok {
		t.Fatal("bot entry enabled without credentials")
	}
}
