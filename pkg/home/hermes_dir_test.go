package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHermesDirPrefersPopulatedLegacyDir(t *testing.T) {
	h := t.TempDir()
	newPath := filepath.Join(h, "platforms", "whatsapp", "session")
	if got := HermesDir(h, "platforms/whatsapp/session", "whatsapp/session"); got != newPath {
		t.Fatalf("missing legacy %s", got)
	}
	legacy := filepath.Join(h, "whatsapp", "session")
	os.MkdirAll(legacy, 0o755)
	if got := HermesDir(h, "platforms/whatsapp/session", "whatsapp/session"); got != newPath {
		t.Fatalf("empty legacy must not shadow %s", got)
	}
	os.WriteFile(filepath.Join(legacy, "creds.json"), []byte("{}"), 0o644)
	if got := HermesDir(h, "platforms/whatsapp/session", "whatsapp/session"); got != legacy {
		t.Fatalf("populated legacy %s", got)
	}
	os.Symlink(filepath.Join(h, "nowhere"), filepath.Join(h, "dangling"))
	if got := HermesDir(h, "fresh", "dangling"); got != filepath.Join(h, "fresh") {
		t.Fatalf("dangling symlink %s", got)
	}
	os.WriteFile(filepath.Join(h, "plainfile"), nil, 0o644)
	if got := HermesDir(h, "fresh", "plainfile"); got != filepath.Join(h, "plainfile") {
		t.Fatalf("legacy file %s", got)
	}
	if got := HermesDir(h, "fresh", "plainfile/child"); got != filepath.Join(h, "plainfile", "child") {
		t.Fatalf("not a directory counts as occupied %s", got)
	}
}
