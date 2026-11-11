package plugins

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plugin at root/segments named after its last segment, extra manifest lines appended
func scannerPlugin(segments []string, extra string) mv2Plugin {
	return mv2Plugin{dir: filepath.Join(segments...), manifest: "name: " + segments[len(segments)-1] + "\nversion: 0.1.0\ndescription: Test plugin " +
		strings.Join(segments, "/") + "\n" + extra, init: "def register(ctx):\n    pass\n"}
}

// Keys of the plugins a manager recorded from any source but bundled
func nonBundledKeys(m *Manager) []string {
	keys := []string{}
	for key, p := range m.plugins {
		if p.Manifest.Source != "bundled" {
			keys = append(keys, key)
		}
	}
	return keys
}

func TestCategoryNamespaceRecursionCategoryNamespaceDiscovered(t *testing.T) {
	m, _ := mv2Discover(t, nil, []mv2Plugin{scannerPlugin([]string{"image_gen", "openai"}, "")}, "  enabled: [image_gen/openai]\n")
	loaded := m.plugins["image_gen/openai"]
	if loaded == nil {
		t.Fatalf("plugins %v", nonBundledKeys(m))
	}
	if loaded.Manifest.Key != "image_gen/openai" || loaded.Manifest.Name != "openai" || !loaded.Enabled {
		t.Fatalf("loaded %+v manifest %+v", loaded, loaded.Manifest)
	}
}

func TestCategoryNamespaceRecursionDepthCapTwo(t *testing.T) {
	m, _ := mv2Discover(t, nil, []mv2Plugin{scannerPlugin([]string{"a", "b", "c"}, "")}, "")
	if got := nonBundledKeys(m); len(got) != 0 {
		t.Fatalf("non bundled plugins %v", got)
	}
}

func TestForeignHarnessManifestDirsForeignHarnessDirsSkippedWithoutWarnings(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	user := []mv2Plugin{{dir: filepath.Join("superpowers", ".icebath-plugin"), manifest: "name: superpowers\nversion: 6.3.0\ndescription: multi-harness plugin\n"}}
	m, h := mv2Discover(t, nil, user, "")
	for _, harness := range []string{".claude-plugin", ".codex-plugin", ".cursor-plugin", ".devin-plugin", ".kimi-plugin"} {
		mv2Write(t, filepath.Join(h, "plugins", "superpowers", harness, "plugin.json"), `{"name": "superpowers", "version": "6.3.0"}`)
	}
	if err := os.Remove(filepath.Join(h, "plugins", "superpowers", ".icebath-plugin", "__init__.py")); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	m = NewManager(h)
	m.DiscoverAndLoad(homeCtx(h), false)
	if m.plugins["superpowers/.icebath-plugin"] == nil {
		t.Fatalf("plugins %v", nonBundledKeys(m))
	}
	if strings.Contains(logs.String(), "Failed to parse") {
		t.Fatalf("parse warnings: %s", logs.String())
	}
}

func TestForeignHarnessManifestDirsBrokenPortablePluginStillWarns(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	_, h := mv2Discover(t, nil, nil, "")
	mv2Write(t, filepath.Join(h, "plugins", "broken-portable", "plugin.json"), `{"name": "broken"}`)
	logs.Reset()
	NewManager(h).DiscoverAndLoad(homeCtx(h), false)
	warned := false
	for _, line := range strings.Split(logs.String(), "\n") {
		warned = warned || (strings.Contains(line, "Failed to parse") && strings.Contains(line, "broken-portable"))
	}
	if !warned {
		t.Fatalf("no parse warning: %s", logs.String())
	}
}

func TestKindFieldUnknownKindFallsBackToStandalone(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	m, _ := mv2Discover(t, nil, []mv2Plugin{scannerPlugin([]string{"p1"}, "kind: bogus\n")}, "  enabled: [p1]\n")
	if got := m.plugins["p1"].Manifest.Kind; got != "standalone" {
		t.Fatalf("kind %q", got)
	}
	if !strings.Contains(logs.String(), "unknown kind") {
		t.Fatalf("no unknown kind warning: %s", logs.String())
	}
}

func TestBackendGateUserBackendStillGatedByEnabled(t *testing.T) {
	m, _ := mv2Discover(t, nil, []mv2Plugin{scannerPlugin([]string{"image_gen", "fancy"}, "kind: backend\n")}, "")
	loaded := m.plugins["image_gen/fancy"]
	if loaded == nil || loaded.Enabled || !strings.Contains(loaded.Error, "not enabled") {
		t.Fatalf("loaded %+v", loaded)
	}
}

func TestBackendGateExclusiveKindSkipped(t *testing.T) {
	m, _ := mv2Discover(t, nil, []mv2Plugin{scannerPlugin([]string{"some-backend"}, "kind: exclusive\n")}, "  enabled: [some-backend]\n")
	loaded := m.plugins["some-backend"]
	if loaded == nil || loaded.Enabled || !strings.Contains(loaded.Error, "exclusive") {
		t.Fatalf("loaded %+v", loaded)
	}
}
