package yuanbao

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

func TestYuanbaoEnvAppKeyAlternative(t *testing.T) {
	cfg := emptyConfig(t)
	t.Setenv("YUANBAO_APP_KEY", "key")
	applyEnv(t, cfg)
	if len(cfg.Platforms) != 0 {
		t.Fatal("enabled without the secret")
	}
	t.Setenv("YUANBAO_APP_SECRET", "sec")
	t.Setenv("YUANBAO_DM_POLICY", " Open ")
	applyEnv(t, cfg)
	pc := cfg.Platforms[gateway.PlatformYuanbao]
	if !pc.Enabled || pc.Extra["app_id"] != "key" || pc.Extra["app_secret"] != "sec" || pc.Extra["dm_policy"] != "open" || pc.Extra["bot_id"] != nil {
		t.Fatalf("yuanbao config %+v", pc.Extra)
	}
}
