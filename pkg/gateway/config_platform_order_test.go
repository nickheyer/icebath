package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestGatewayConfigKeepsPlatformsInFileOrder(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("MSGRAPH_WEBHOOK_ENABLED", "true")
	yaml := "platforms:\n  signal:\n    enabled: true\n  webhook:\n    enabled: true\n  telegram:\n    enabled: true\n  discord:\n    enabled: true\n" +
		"  email:\n    enabled: true\n  slack:\n    enabled: false\n  api_server:\n    enabled: true\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGatewayConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Platform{PlatformSignal, PlatformWebhook, PlatformTelegram, PlatformDiscord, PlatformEmail, PlatformSlack, PlatformAPIServer, PlatformMSGraphWebhook}
	if got := cfg.PlatformKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("platform order %v, want %v", got, want)
	}
	wantNames := make([]string, 0, len(want))
	for _, p := range want {
		wantNames = append(wantNames, string(p))
	}
	if names := config.MappingKeys(cfg.ToDict()["platforms"].(map[string]any)); !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("to_dict platforms %v, want %v", names, wantNames)
	}
	delete(cfg.Platforms, PlatformSignal)
	cfg.SetPlatform(PlatformSignal, NewPlatformConfig())
	cfg.SetPlatform(PlatformTelegram, NewPlatformConfig())
	want = []Platform{PlatformWebhook, PlatformTelegram, PlatformDiscord, PlatformEmail, PlatformSlack, PlatformAPIServer, PlatformMSGraphWebhook, PlatformSignal}
	if got := cfg.PlatformKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reinserted order %v, want %v", got, want)
	}
}
