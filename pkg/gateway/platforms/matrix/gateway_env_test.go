package matrix

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

func TestMatrixEnvE2EEModeAndEncryptionFlag(t *testing.T) {
	cfg := emptyConfig(t)
	t.Setenv("MATRIX_ACCESS_TOKEN", "tok")
	t.Setenv("MATRIX_E2EE_MODE", " Prefer ")
	t.Setenv("MATRIX_DEVICE_ID", "DEV")
	applyEnv(t, cfg)
	pc := cfg.Platforms[gateway.PlatformMatrix]
	if pc == nil || !pc.Enabled || pc.Token != "tok" || pc.Extra["encryption"] != true || pc.Extra["e2ee_mode"] != "prefer" || pc.Extra["device_id"] != "DEV" || pc.Extra["homeserver"] != "" {
		t.Fatalf("matrix config %+v", pc)
	}
	cfg = emptyConfig(t)
	t.Setenv("MATRIX_E2EE_MODE", "")
	t.Setenv("MATRIX_ENCRYPTION", "yes")
	applyEnv(t, cfg)
	if pc := cfg.Platforms[gateway.PlatformMatrix]; pc.Extra["encryption"] != true || pc.Extra["e2ee_mode"] != nil {
		t.Fatalf("flag config %+v", pc.Extra)
	}
	cfg = emptyConfig(t)
	t.Setenv("MATRIX_ENCRYPTION", "")
	applyEnv(t, cfg)
	if pc := cfg.Platforms[gateway.PlatformMatrix]; pc.Extra["encryption"] != false {
		t.Fatalf("off config %+v", pc.Extra)
	}
}
