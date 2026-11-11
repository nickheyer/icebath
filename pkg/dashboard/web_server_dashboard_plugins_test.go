package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Writes a dashboard manifest under the home plugins dir
func writeHomePlugin(t *testing.T, h, name string, manifest map[string]any) {
	t.Helper()
	writePluginManifest(t, filepath.Join(h, "plugins"), name, manifest)
}

// Entries of the discovered plugins carrying the name
func entriesNamed(list []*pyjson.Object, name string) []*pyjson.Object {
	out := []*pyjson.Object{}
	for _, p := range list {
		if p.GetOr("name", nil) == name {
			out = append(out, p)
		}
	}
	return out
}

func TestDashboardPluginManifestExtensionsOverrideAndHiddenCarriedThrough(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	writeHomePlugin(t, tmp, "skin-home", map[string]any{"name": "skin-home", "label": "Skin Home",
		"tab": map[string]any{"path": "/skin-home", "override": "/", "hidden": true}, "slots": []any{"sidebar", "header-left"},
		"entry": "dist/index.js"})
	resetDashboardPluginCache(t)
	entry := namedPlugin(getDashboardPlugins(context.Background(), true), "skin-home")
	if entry == nil {
		t.Fatal("skin-home not discovered")
	}
	tab := entry.GetOr("tab", nil).(*pyjson.Object)
	if tab.GetOr("override", nil) != "/" || tab.GetOr("hidden", nil) != true {
		t.Fatalf("tab %v", tab)
	}
	slots := entry.GetOr("slots", nil).([]any)
	if len(slots) != 2 || slots[0] != "sidebar" || slots[1] != "header-left" {
		t.Fatalf("slots %v", slots)
	}
}

func TestDashboardPluginManifestExtensionsUserPluginsIgnoreProfileHomeOverride(t *testing.T) {
	tmp := t.TempDir()
	launch, other := filepath.Join(tmp, "launch"), filepath.Join(tmp, "other-profile")
	for _, d := range []string{launch, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeHomePlugin(t, launch, "skin-home", map[string]any{"name": "skin-home", "label": "Skin Home",
		"tab": map[string]any{"path": "/skin-home"}, "entry": "dist/index.js"})
	t.Setenv("ICEBATH_HOME", launch)
	ctx := home.WithOverride(context.Background(), other)
	if home.Dir(ctx) != other {
		t.Fatalf("override not in effect: %s", home.Dir(ctx))
	}
	if len(entriesNamed(discoverDashboardPlugins(nil), "skin-home")) == 0 {
		t.Fatal("launch home plugin hidden by the profile override")
	}
}

func TestDashboardPluginManifestExtensionsUserPluginsFoundUnderProfileScopedProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath-root")
	profileHome := filepath.Join(root, "profiles", "presale")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHomePlugin(t, root, "meeting-intelligence", map[string]any{"name": "meeting-intelligence",
		"label": "Meeting Intelligence", "tab": map[string]any{"path": "/meetings"}, "entry": "dist/index.js"})
	t.Setenv("ICEBATH_HOME", profileHome)
	if len(entriesNamed(discoverDashboardPlugins(nil), "meeting-intelligence")) == 0 {
		t.Fatal("root plugin not discovered from a profile scoped process")
	}
}

func TestDashboardPluginManifestExtensionsProfileLocalPluginWinsOverRootPlugin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath-root")
	profileHome := filepath.Join(root, "profiles", "presale")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHomePlugin(t, profileHome, "dupe", map[string]any{"name": "dupe", "label": "Profile Copy",
		"tab": map[string]any{"path": "/from-profile"}, "entry": "dist/index.js"})
	writeHomePlugin(t, root, "dupe", map[string]any{"name": "dupe", "label": "Root Copy",
		"tab": map[string]any{"path": "/from-root"}, "entry": "dist/index.js"})
	t.Setenv("ICEBATH_HOME", profileHome)
	entries := entriesNamed(discoverDashboardPlugins(nil), "dupe")
	if len(entries) != 1 {
		t.Fatalf("%d entries", len(entries))
	}
	if path := entries[0].GetOr("tab", nil).(*pyjson.Object).GetOr("path", nil); path != "/from-profile" {
		t.Fatalf("path %v", path)
	}
}
