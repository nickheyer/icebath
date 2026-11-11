package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Clears the process multiplex flag around a case
func resetMultiplexFlag(t *testing.T) {
	provider.SetMultiplexActive(false)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
}

func TestUnscopedWhenMultiplexOff(t *testing.T) {
	resetMultiplexFlag(t)
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	os.WriteFile(filepath.Join(h, ".env"), []byte("TELEGRAM_BOT_TOKEN=from-default-env\n"), 0o644)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  multiplex_profiles: false\n"), 0o644)
	t.Setenv("ICEBATH_HOME", h)
	os.Unsetenv("TELEGRAM_BOT_TOKEN")
	cfg, err := loadGatewayConfigForRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MultiplexProfiles == nil || *cfg.MultiplexProfiles {
		t.Fatal(cfg.MultiplexProfiles)
	}
}

func TestScopedReloadStillSeesContainerApiServerEnv(t *testing.T) {
	resetMultiplexFlag(t)
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	os.WriteFile(filepath.Join(h, ".env"), []byte("TELEGRAM_BOT_TOKEN=default-profile-token-123\n"+
		"API_SERVER_KEY=profile-scoped-key-0123456789abcdef\n"), 0o644)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("gateway:\n  multiplex_profiles: true\n"), 0o644)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("API_SERVER_ENABLED", "true")
	t.Setenv("API_SERVER_HOST", "0.0.0.0")
	t.Setenv("API_SERVER_PORT", "8642")
	os.Unsetenv("API_SERVER_KEY")
	os.Unsetenv("TELEGRAM_BOT_TOKEN")
	provider.SetMultiplexActive(true)
	cfg, err := loadGatewayConfigForRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		t.Fatal(cfg.MultiplexProfiles)
	}
	tg := cfg.Platforms[gateway.PlatformTelegram]
	if tg == nil || tg.Token != "default-profile-token-123" {
		t.Fatalf("telegram %+v", tg)
	}
	api := cfg.Platforms[gateway.PlatformAPIServer]
	if api == nil || !api.Enabled {
		t.Fatalf("api_server should be enabled from container env even inside the scoped runner reload %+v", api)
	}
	if api.Extra["key"] != "profile-scoped-key-0123456789abcdef" || api.Extra["host"] != "0.0.0.0" {
		t.Fatal(api.Extra)
	}
	if port, ok := api.Extra["port"]; !ok || (port != int64(8642) && port != 8642) {
		t.Fatal(api.Extra["port"])
	}
}
