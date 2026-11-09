package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Ordered selection keyed by plugins dir for comparisons
func wsSelection(t *testing.T, opts EnabledPluginsOptions) map[string][]string {
	t.Helper()
	got, err := EnabledPluginsOrdered(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, h := range got {
		out[h.PluginsDir] = h.Names
	}
	return out
}

// Requires a ValueError whose message names path
func wsRequireValueError(t *testing.T, err error, path string) {
	t.Helper()
	var pyErr *PyError
	if !errors.As(err, &pyErr) || pyErr.Class != "ValueError" || !strings.Contains(pyErr.Message, path) {
		t.Fatalf("want ValueError naming %s, got %v", path, err)
	}
}

func TestPluginsStateReadsAllHomes(t *testing.T) {
	root, profile := wsHomes(t)
	wsWriteConfig(t, root, "a-plug", "b-plug")
	wsWriteConfig(t, profile, "c-plug")
	got := wsSelection(t, EnabledPluginsOptions{})
	want := map[string][]string{filepath.Join(root, "plugins"): {"a-plug", "b-plug"}, filepath.Join(profile, "plugins"): {"c-plug"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection %v", got)
	}
}

func TestPluginsStateOnlyLiveProfilesJoinTheUnion(t *testing.T) {
	root, profile := wsHomes(t)
	profiles := filepath.Dir(profile)
	wsWriteConfig(t, root, "a-plug")
	wsWriteConfig(t, profile, "live-plug")
	for _, name := range []string{".work.staging-123", "Bad Name", "retired", "ghost"} {
		os.MkdirAll(filepath.Join(profiles, name), 0o777)
	}
	for _, name := range []string{".work.staging-123", "Bad Name", "retired"} {
		wsWriteConfig(t, filepath.Join(profiles, name), name+"-plug")
	}
	if err := home.MarkProfileDeleted(filepath.Join(profiles, "retired")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(profiles, "ghost", "plugins"), 0o777)
	homes, err := dependencyHomes(context.Background())
	if err != nil || !reflect.DeepEqual(homes, []string{root, profile}) {
		t.Fatalf("homes %v %v", homes, err)
	}
	got := wsSelection(t, EnabledPluginsOptions{})
	if len(got) != 2 || got[filepath.Join(root, "plugins")] == nil || got[filepath.Join(profile, "plugins")] == nil {
		t.Fatalf("selection %v", got)
	}
}

func TestPluginsStateUnreadableProfileStateIsNotAnEmptySelection(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission faults need an unprivileged user")
	}
	for _, boundary := range []string{"profile-listing", "profile-stat", "plugin-stat", "manifest-read", "manifest-stat", "provider-stat"} {
		t.Run(boundary, func(t *testing.T) {
			_, profile := wsHomes(t)
			wsWriteConfig(t, profile, "keep-plug")
			plugin := filepath.Join(profile, "plugins", "keep-plug")
			manifest := filepath.Join(plugin, "plugin.yaml")
			peWrite(t, manifest, "python_dependencies: [fixture-dep]\n")
			if boundary == "provider-stat" {
				peWrite(t, filepath.Join(profile, "config.yaml"), "memory:\n  provider: keep-plug\n")
			}
			locked, mode, target := map[string][2]any{
				"profile-listing": {filepath.Dir(profile), os.FileMode(0o000)},
				"profile-stat":    {filepath.Dir(profile), os.FileMode(0o444)},
				"plugin-stat":     {filepath.Dir(plugin), os.FileMode(0o444)},
				"manifest-read":   {manifest, os.FileMode(0o000)},
				"manifest-stat":   {plugin, os.FileMode(0o444)},
				"provider-stat":   {filepath.Dir(plugin), os.FileMode(0o444)},
			}[boundary], os.FileMode(0), map[string]string{
				"profile-listing": filepath.Dir(profile), "profile-stat": profile, "plugin-stat": plugin,
				"manifest-read": manifest, "manifest-stat": manifest, "provider-stat": plugin,
			}[boundary]
			mode = locked[1].(os.FileMode)
			path := locked[0].(string)
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(path, 0o777) })
			_, err := EnabledMemberDirs(context.Background(), MemberSelection{})
			wsRequireValueError(t, err, target)
		})
	}
}

func TestPluginsStateUnreadableConfigLinkIsNotAnEmptySelection(t *testing.T) {
	root, _ := wsHomes(t)
	config := filepath.Join(root, "config.yaml")
	os.Remove(config)
	if err := os.Symlink(config, config); err != nil {
		t.Fatal(err)
	}
	_, err := EnabledPluginsOrdered(context.Background(), EnabledPluginsOptions{})
	wsRequireValueError(t, err, config)
}

func TestPluginsStateRefusesInvalidExistingConfig(t *testing.T) {
	for _, content := range []string{"{ not yaml", "[]", "plugins: wrong", "plugins:\n  enabled: wrong", "memory: wrong"} {
		root, _ := wsHomes(t)
		config := filepath.Join(root, "config.yaml")
		peWrite(t, config, content)
		_, err := EnabledPluginsOrdered(context.Background(), EnabledPluginsOptions{})
		wsRequireValueError(t, err, config)
		if data, _ := os.ReadFile(config); string(data) != content {
			t.Fatal("config changed")
		}
	}
}

func TestPluginsStateVerifySkipsBadSecondaryButAdmissionRefusesIt(t *testing.T) {
	root, profile := wsHomes(t)
	plugin := filepath.Join(root, "plugins", "working")
	peWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: working\npython_dependencies: [requests]\n")
	wsWriteConfig(t, root, "working")
	bad := filepath.Join(profile, "config.yaml")
	peWrite(t, bad, "plugins: [invalid]\n")
	if got, err := EnabledMemberDirs(context.Background(), MemberSelection{}); err != nil || !reflect.DeepEqual(got, []string{plugin}) {
		t.Fatalf("members %v %v", got, err)
	}
	_, err := enabledPluginDirs(context.Background(), EnabledPluginsOptions{ProposedHome: root, Enabled: []string{"working"}})
	wsRequireValueError(t, err, bad)
	peWrite(t, bad, "plugins: {enabled: []}\n")
	if got, err := EnabledMemberDirs(context.Background(), MemberSelection{}); err != nil || !reflect.DeepEqual(got, []string{plugin}) {
		t.Fatalf("members %v %v", got, err)
	}
}

func TestPluginsStateEmptyConfigIsAnExplicitEmptySelection(t *testing.T) {
	for _, content := range []string{"", "# empty config\n", "null", "{}", "plugins: {}", "plugins:\n  enabled: []"} {
		root, profile := wsHomes(t)
		peWrite(t, filepath.Join(root, "config.yaml"), content)
		os.Remove(filepath.Join(profile, "config.yaml"))
		peWrite(t, filepath.Join(profile, "SOUL.md"), "work\n")
		if got := wsSelection(t, EnabledPluginsOptions{}); len(got) != 0 {
			t.Fatalf("%q gave %v", content, got)
		}
	}
}

func TestPluginsStateMemoryProviderJoinsOrderedSelection(t *testing.T) {
	cases := []struct {
		enabled  []string
		provider string
		exists   bool
		expected []string
	}{
		{[]string{"z-first", "a-second"}, "provider", true, []string{"z-first", "a-second", "provider"}},
		{[]string{"provider"}, "provider", true, []string{"provider"}},
		{nil, "ghost", false, nil},
	}
	for _, c := range cases {
		root, sibling := wsHomes(t)
		if c.exists {
			os.MkdirAll(filepath.Join(root, "plugins", c.provider), 0o777)
		}
		body := "plugins:\n  enabled: [" + strings.Join(c.enabled, ", ") + "]\nmemory:\n  provider: " + c.provider + "\n"
		peWrite(t, filepath.Join(root, "config.yaml"), body)
		wsWriteConfig(t, sibling, "sibling")
		got := wsSelection(t, EnabledPluginsOptions{})
		if !reflect.DeepEqual(got[filepath.Join(root, "plugins")], c.expected) || !reflect.DeepEqual(got[filepath.Join(sibling, "plugins")], []string{"sibling"}) {
			t.Fatalf("selection %v", got)
		}
	}
}

func TestPluginsStateMemberDiscoveryUsesRealEnabledConfig(t *testing.T) {
	root, _ := wsHomes(t)
	for rel, body := range map[string]string{
		"modern/pyproject.toml": "[project]\ndependencies = [\"requests>=2\"]\n",
		"legacy/plugin.yaml":    "name: legacy\npip_dependencies: [\"requests>=2\"]\n",
		"plain/plugin.yaml":     "name: plain\n",
		"orphan/pyproject.toml": "[project]\n",
	} {
		peWrite(t, filepath.Join(root, "plugins", rel), body)
	}
	wsWriteConfig(t, root, "legacy", "modern", "plain")
	got, err := EnabledMemberDirs(context.Background(), MemberSelection{})
	if want := []string{filepath.Join(root, "plugins", "legacy"), filepath.Join(root, "plugins", "modern")}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("members %v %v", got, err)
	}
	wsWriteConfig(t, root)
	if got, err := EnabledMemberDirs(context.Background(), MemberSelection{}); err != nil || len(got) != 0 {
		t.Fatalf("members %v %v", got, err)
	}
}

func TestPluginsStateParsesConfigOncePerHome(t *testing.T) {
	root, profile := wsHomes(t)
	wsWriteConfig(t, root, "a-plug")
	wsWriteConfig(t, profile, "c-plug")
	calls := 0
	original := selectionYAMLUnmarshal
	peSwap(t, &selectionYAMLUnmarshal, func(data []byte, out any) error {
		calls++
		return original(data, out)
	})
	wsSelection(t, EnabledPluginsOptions{})
	if calls != 2 {
		t.Fatalf("parsed %d times", calls)
	}
}

func TestPluginsStatePMReadsTheLocalSelection(t *testing.T) {
	root, _ := wsHomes(t)
	peWrite(t, filepath.Join(root, "config.yaml"), "plugins:\n  enabled: [alpha]\n")
	config, err := ReadHomeSelection(root)
	if err != nil || !reflect.DeepEqual(config["plugins"].(map[string]any)["enabled"], []any{"alpha"}) {
		t.Fatalf("selection %v %v", config, err)
	}
	if config, err := ReadHomeSelection(filepath.Join(root, "absent")); config != nil || err != nil {
		t.Fatalf("absent home %v %v", config, err)
	}
}

func TestDependencyHomesExportsTheUnion(t *testing.T) {
	root, profile := wsHomes(t)
	homes, err := DependencyHomes(context.Background())
	if err != nil || !reflect.DeepEqual(homes, []string{root, profile}) {
		t.Fatalf("homes %v %v", homes, err)
	}
}
