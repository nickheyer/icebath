package plugins

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Keys hermes _discover_all_plugins lists as bundled from its shipped plugins tree
var shippedBundledListingKeys = []string{"browser/browser_use", "browser/browserbase", "browser/firecrawl", "cron_providers/chronos",
	"dashboard_auth/basic", "dashboard_auth/drain", "dashboard_auth/nous", "dashboard_auth/self_hosted", "dashboard_auth/session_token",
	"delegation/default", "disk-cleanup", "google_meet", "image_gen/deepinfra", "image_gen/fal", "image_gen/krea", "image_gen/meta-ai",
	"image_gen/openai", "image_gen/openai-codex", "image_gen/openrouter", "image_gen/xai", "platforms/a2a", "platforms/bluebubbles",
	"platforms/buzz", "platforms/dingtalk", "platforms/discord", "platforms/email", "platforms/feishu", "platforms/google_chat",
	"platforms/irc", "platforms/line", "platforms/matrix", "platforms/mattermost", "platforms/ntfy", "platforms/photon", "platforms/qqbot",
	"platforms/raft", "platforms/signal", "platforms/simplex", "platforms/slack", "platforms/sms", "platforms/teams", "platforms/telegram",
	"platforms/wecom", "platforms/weixin", "platforms/whatsapp", "platforms/whatsapp_cloud", "platforms/yuanbao", "security-guidance",
	"teams_pipeline", "video_gen/deepinfra", "video_gen/fal", "video_gen/openrouter", "video_gen/xai", "web/brave_free", "web/ddgs",
	"web/exa", "web/firecrawl", "web/keenable", "web/openai_native", "web/parallel", "web/perplexity", "web/searxng", "web/tavily", "web/xai"}

// Unsets the bundled override so the test reads the tree compiled into the binary
func unsetBundledOverride(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	os.Unsetenv("ICEBATH_BUNDLED_PLUGINS")
}

func TestDiscoverAllPluginsListsTheShippedBundledTreeWithoutOverride(t *testing.T) {
	unsetBundledOverride(t)
	h := t.TempDir()
	entries, err := DiscoverAllPlugins(h)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]PluginEntry{}
	for _, e := range entries {
		if _, dup := byKey[e.Key]; dup {
			t.Fatalf("key %q listed twice", e.Key)
		}
		byKey[e.Key] = e
	}
	for _, key := range shippedBundledListingKeys {
		if e, ok := byKey[key]; !ok || e.Source != "bundled" {
			t.Fatalf("bundled %q missing %+v", key, e)
		}
	}
	for key, e := range byKey {
		if e.Source == "bundled" && !slices.Contains(shippedBundledListingKeys, key) && !slices.Contains(BundledPluginKeys(), key) {
			t.Fatalf("bundled %q is neither shipped nor compiled in", key)
		}
	}
	telegram := byKey["platforms/telegram"]
	if telegram.Name != "telegram-platform" || telegram.Version != "1.0.0" || telegram.Dir != "" ||
		!strings.HasPrefix(telegram.Description, "Telegram gateway adapter for Hermes Agent.") {
		t.Fatalf("telegram row %+v", telegram)
	}
	if tavily := byKey["web/tavily"]; tavily.Name != "web-tavily" || !strings.Contains(tavily.Description, "Opt-in keyless via icebath tools") {
		t.Fatalf("tavily row %+v", tavily)
	}
	none := map[string]bool{}
	for key, want := range map[string]string{"platforms/telegram": "enabled", "web/tavily": "enabled", "dashboard_auth/basic": "enabled",
		"disk-cleanup": "not enabled", "google_meet": "not enabled"} {
		got, err := pluginStatus(h, byKey[key], none, none, none, true)
		if err != nil || got != want {
			t.Fatalf("%s status %q %v", key, got, err)
		}
	}
	if got, _ := pluginStatus(h, byKey["platforms/telegram"], none, map[string]bool{"platforms/telegram": true}, none, true); got != "disabled" {
		t.Fatalf("an explicit disable wins, got %q", got)
	}
}

func TestPluginToolsetKeyReadsCompiledProvidesToolsWithoutOverride(t *testing.T) {
	unsetBundledOverride(t)
	h := t.TempDir()
	name := "a2a_discover"
	previous := tools.Default.SnapshotRegistration("", name)
	tools.Default.Register(tools.Def{Name: name, Toolset: "a2a", Schema: ledgerSchema(name), Handler: ledgerSharedHandler})
	registered := tools.Default.SnapshotRegistration("", name)
	t.Cleanup(func() { tools.Default.RestoreRegistration(name, registered, previous, "") })
	ctx := home.WithOverride(context.Background(), h)
	if got := getPluginToolsetKey(ctx, h, "platforms/a2a"); got != "a2a" {
		t.Fatalf("toolset %q", got)
	}
	if got := getPluginToolsetKey(ctx, h, "platforms/telegram"); got != "" {
		t.Fatalf("a manifest without provides_tools names no toolset, got %q", got)
	}
	if names, ok := compiledProvidesTools("google_meet"); !ok || !slices.Equal(names, []string{"meet_join", "meet_leave", "meet_status", "meet_transcript", "meet_say"}) {
		t.Fatalf("google_meet provides %v %v", names, ok)
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	if _, ok := compiledProvidesTools("platforms/a2a"); ok {
		t.Fatal("the override directory replaces the compiled tree")
	}
}
