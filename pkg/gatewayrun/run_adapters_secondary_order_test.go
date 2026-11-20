package gatewayrun

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// A secondary profile's platforms are loaded and walked in the order its config.yaml lists them
func TestSecondaryProfilePlatformsStartInConfigOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	profileHome := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	config := "platforms:\n  telegram:\n    enabled: true\n  discord:\n    enabled: true\n  slack:\n    enabled: true\n  mattermost:\n    enabled: true\n"
	if err := os.WriteFile(filepath.Join(profileHome, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := makeRestartRunner(t)
	multiplex := true
	r.config.MultiplexProfiles = &multiplex
	logs := captureLogRecords(t, slog.LevelInfo)
	connected, err := r.startOneProfileAdapters(context.Background(), "coder", profileHome, map[claimKey]string{})
	if err != nil || connected != 0 {
		t.Fatalf("started %d adapters, %v", connected, err)
	}
	loaded, _ := r.profileConfigs.Get("coder")
	if loaded == nil {
		t.Fatal("the profile config was not recorded")
	}
	want := []gateway.Platform{gateway.PlatformTelegram, gateway.PlatformDiscord, gateway.PlatformSlack, gateway.PlatformMattermost}
	var enabled []gateway.Platform
	for _, platform := range loaded.PlatformKeys() {
		if pc := loaded.Platforms[platform]; pc != nil && pc.Enabled {
			enabled = append(enabled, platform)
		}
	}
	if !reflect.DeepEqual(enabled, want) {
		t.Fatalf("loaded platforms %v, want %v", enabled, want)
	}
	var walked []gateway.Platform
	for _, message := range logs.messages(slog.LevelInfo) {
		rest, found := strings.CutPrefix(message, "[MULTIPLEX] Profile 'coder': skipping ")
		if !found {
			continue
		}
		name, _, _ := strings.Cut(rest, " - no bot credential")
		walked = append(walked, gateway.Platform(name))
	}
	if !reflect.DeepEqual(walked, want) {
		t.Fatalf("platforms walked %v, want %v", walked, want)
	}
}
