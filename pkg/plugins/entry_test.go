package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func writePluginDir(t *testing.T, dir, manifest string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindPluginEntryByKeyNameUniqueLeafAndNeverGuesses(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	writePluginDir(t, filepath.Join(h, "plugins", "voice-dir"), "name: voice\nversion: 1.2.0\ndescription: Voice\n")
	writePluginDir(t, filepath.Join(h, "plugins", "web", "exa"), "name: exa\n")
	writePluginDir(t, filepath.Join(h, "plugins", "search", "dup"), "name: dup-a\n")
	writePluginDir(t, filepath.Join(h, "plugins", "other", "dup"), "name: dup-b\n")
	_ = os.MkdirAll(filepath.Join(h, "plugins", "voice-dir", ".git"), 0o755)
	e, err := FindPluginEntry(h, "voice")
	if err != nil || e == nil || e.Dir != filepath.Join(h, "plugins", "voice-dir") || e.Key != "voice" || e.Version != "1.2.0" || e.Source != "git" {
		t.Fatalf("%+v %v", e, err)
	}
	if e, _ := FindPluginEntry(h, "web/exa"); e == nil || e.Name != "exa" {
		t.Fatalf("by key %+v", e)
	}
	if e, _ := FindPluginEntry(h, "exa"); e == nil || e.Key != "web/exa" {
		t.Fatalf("by leaf %+v", e)
	}
	if e, _ := FindPluginEntry(h, "dup"); e != nil {
		t.Fatalf("ambiguous leaf resolved to %+v", e)
	}
	t.Setenv("ICEBATH_HOME", h)
	if dir, found, err := FindPluginEntryDir("voice"); err != nil || !found || dir != filepath.Join(h, "plugins", "voice-dir") {
		t.Fatal(dir, found, err)
	}
}
