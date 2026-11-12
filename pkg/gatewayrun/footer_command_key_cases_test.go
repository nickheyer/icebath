package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestFooterWritesGlobalFromChatAndCliKeyLocally(t *testing.T) {
	for _, tc := range []struct {
		platform gateway.Platform
		path     []string
	}{
		{gateway.PlatformTelegram, []string{"display", "runtime_footer", "enabled"}},
		{gateway.PlatformLocal, []string{"display", "platforms", "cli", "runtime_footer", "enabled"}},
	} {
		t.Run(string(tc.platform), func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			configPath := filepath.Join(h, "config.yaml")
			if err := os.WriteFile(configPath, []byte(`{"display": {"runtime_footer": {"enabled": false}}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			userID := "u1"
			source := &gateway.SessionSource{Platform: tc.platform, ChatID: "c1", UserID: &userID, ChatType: "dm"}
			event := gateway.NewMessageEvent("/footer on")
			event.Source = source
			if _, err := (&GatewayRunner{}).handleFooterCommand(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			saved, err := config.ReadUserConfigRaw(context.Background(), configPath)
			if err != nil {
				t.Fatal(err)
			}
			var node any = saved
			for _, part := range tc.path {
				m, _ := node.(map[string]any)
				node = m[part]
			}
			if node != true {
				t.Fatalf("%v saved %v", tc.path, saved)
			}
			if tc.platform == gateway.PlatformTelegram {
				display, _ := saved["display"].(map[string]any)
				if _, local := display["platforms"]; local {
					t.Fatalf("a chat footer wrote the local key %v", saved)
				}
			}
		})
	}
}
