package gateway

import (
	"context"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Directory names of the platform plugins hermes ships under plugins/platforms
var shippedPlatformDirs = []string{"a2a", "bluebubbles", "buzz", "dingtalk", "discord", "email", "feishu", "google_chat", "irc",
	"line", "matrix", "mattermost", "ntfy", "photon", "qqbot", "raft", "signal", "simplex", "slack", "sms", "teams", "telegram",
	"wecom", "weixin", "whatsapp", "whatsapp_cloud", "yuanbao"}

// Unsets the bundled override and clears the scan cache so the test reads the tree compiled into the binary
func useCompiledTree(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	os.Unsetenv("ICEBATH_BUNDLED_PLUGINS")
	reset := func() {
		platformMembers.Lock()
		platformMembers.scanned, platformMembers.pseudo, platformMembers.memberMap = false, nil, nil
		platformMembers.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestDynamicMemberCaseNormalised(t *testing.T) {
	useCompiledTree(t)
	ctx := context.Background()
	a, errA := ParsePlatform(ctx, "IRC")
	b, errB := ParsePlatform(ctx, "irc")
	if errA != nil || errB != nil || a != b || a != "irc" {
		t.Fatalf("%q %v %q %v", a, errA, b, errB)
	}
}

func TestDynamicMemberWithHyphens(t *testing.T) {
	useCompiledTree(t)
	registerPluginEntry(t, NewPlatformEntry("my-platform", "My Platform", nil, func() bool { return true }))
	p, err := ParsePlatform(context.Background(), "my-platform")
	if err != nil || p != "my-platform" || p.Name() != "MY_PLATFORM" {
		t.Fatalf("%q %q %v", p, p.Name(), err)
	}
}

func TestBundledManifestNameAliasResolvesToDirectoryMember(t *testing.T) {
	useCompiledTree(t)
	ctx := context.Background()
	byDir, errDir := ParsePlatform(ctx, "a2a")
	byManifest, errManifest := ParsePlatform(ctx, "a2a-platform")
	if errDir != nil || errManifest != nil || byManifest != byDir || byManifest != "a2a" {
		t.Fatalf("%q %v %q %v", byDir, errDir, byManifest, errManifest)
	}
}

func TestConfigKeepsPlatformWrittenUnderManifestName(t *testing.T) {
	useCompiledTree(t)
	cfg, err := GatewayConfigFromDict(context.Background(), map[string]any{"platforms": map[string]any{"a2a-platform": map[string]any{"enabled": true}}})
	if err != nil {
		t.Fatal(err)
	}
	if pc, ok := cfg.Platforms["a2a"]; !ok || !pc.Enabled {
		t.Fatalf("a2a missing or disabled %+v", cfg.Platforms)
	}
}

func TestAliasNeverShadowsADirectoryName(t *testing.T) {
	useCompiledTree(t)
	names, aliases := scanBundledPluginPlatforms()
	if !names["a2a"] {
		t.Fatalf("a2a missing from %v", names)
	}
	for alias, dir := range aliases {
		if !names[dir] || names[alias] {
			t.Fatalf("alias %q of %q", alias, dir)
		}
	}
}

func TestCompiledTreeServesEveryShippedPlatformWithoutOverride(t *testing.T) {
	useCompiledTree(t)
	ctx := context.Background()
	names, aliases := scanBundledPluginPlatforms()
	if got := slices.Sorted(maps.Keys(names)); !slices.Equal(got, shippedPlatformDirs) {
		t.Fatalf("names %v", got)
	}
	wantAliases := map[string]string{}
	for _, dir := range shippedPlatformDirs {
		wantAliases[dir+"-platform"] = dir
	}
	if !maps.Equal(aliases, wantAliases) {
		t.Fatalf("aliases %v", aliases)
	}
	labels := MessagingPlatformLabels()
	if !slices.IsSorted(labels) {
		t.Fatalf("labels not sorted %v", labels)
	}
	for _, p := range builtinPlatforms {
		if !slices.Contains(labels, string(p)) {
			t.Fatalf("label %q missing", p)
		}
	}
	gate := tools.BundledPlatformNames()
	for _, dir := range shippedPlatformDirs {
		for _, value := range []string{dir, dir + "-platform", " " + dir + "-PLATFORM "} {
			if p, err := ParsePlatform(ctx, value); err != nil || p != Platform(dir) {
				t.Fatalf("%q parsed %q %v", value, p, err)
			}
		}
		if !CoreShipsPlatform(dir) {
			t.Fatalf("core ships %q", dir)
		}
		for _, label := range []string{dir, dir + "-platform"} {
			if !slices.Contains(labels, label) || !slices.Contains(gate, label) {
				t.Fatalf("label %q missing from %v or %v", label, labels, gate)
			}
		}
	}
	if !slices.Equal(config.BundledPlatformDirNames(t.TempDir()), shippedPlatformDirs) {
		t.Fatalf("config dir names %v", config.BundledPlatformDirNames(t.TempDir()))
	}
}
