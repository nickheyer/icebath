package config

import (
	"reflect"
	"testing"
)

func TestConfigCheckDiagnosticsMentionsDisabledPlatformsWithCredentials(t *testing.T) {
	env := map[string]string{"TELEGRAM_BOT_TOKEN": "t", "DISCORD_BOT_TOKEN": "d"}
	var gotToolsets any
	src := CheckDiagnosticsSources{
		PlatformToolsetDiagnostics: func(pt any, cfg map[string]any) []string {
			gotToolsets = pt
			return []string{"platform_toolsets.cli names unknown toolset 'webb'"}
		},
		DisabledPlugins: func() map[string]bool {
			return map[string]bool{"platforms/telegram": true, "discord-platform": true, "platforms/slack": true}
		},
		BundledPlatformManifests: func() ([]PlatformManifest, error) {
			return []PlatformManifest{
				{Name: "telegram", ManifestName: "telegram", RequiredEnv: []string{"TELEGRAM_BOT_TOKEN"}},
				{Name: "discord", ManifestName: "discord-platform", RequiredEnv: []string{"DISCORD_BOT_TOKEN"}},
				{Name: "slack", ManifestName: "slack", RequiredEnv: []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"}},
				{Name: "matrix", ManifestName: "matrix", RequiredEnv: []string{"TELEGRAM_BOT_TOKEN"}},
				{Name: "webhook", ManifestName: "platforms/webhook"},
			}, nil
		},
	}
	cfg := map[string]any{"platform_toolsets": map[string]any{"cli": []any{"webb"}}}
	got, err := ConfigCheckDiagnostics(cfg, func(k string) string { return env[k] }, src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"platform_toolsets.cli names unknown toolset 'webb'",
		"platform plugin 'platforms/telegram' is disabled while its required credentials are configured. Run `icebath plugins enable platforms/telegram` if you want it active.",
		"platform plugin 'platforms/discord' is disabled while its required credentials are configured. Run `icebath plugins enable platforms/discord` if you want it active.",
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotToolsets, cfg["platform_toolsets"]) {
		t.Fatalf("got %q", got)
	}
}
