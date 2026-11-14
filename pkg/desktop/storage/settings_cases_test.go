package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultProjectDirIsCreatedStoredResolvedAndClearedByABlankValue(t *testing.T) {
	s, host := serviceForTest(t)
	target := filepath.Join(t.TempDir(), "projects", "new")
	raw, _ := json.Marshal("  " + target + "  ")
	got, err := s.SettingsSetDefaultProjectDir(raw)
	if err != nil || jsStringify(got, "") != `{"dir":`+mustJSON(target)+`}` {
		t.Fatalf("set %s %v", jsStringify(got, ""), err)
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		t.Fatal("directory not created")
	}
	if dir, ok := s.readDefaultProjectDir(); !ok || dir != target {
		t.Fatalf("read %q %v", dir, ok)
	}
	host.pick = filepath.Join(target, "chosen")
	picked, _ := s.SettingsPickDefaultProjectDir()
	if jsStringify(picked, "") != `{"canceled":false,"dir":`+mustJSON(host.pick)+`}` || host.picked[0] != "Choose default project directory|"+target {
		t.Fatalf("pick %s %v", jsStringify(picked, ""), host.picked)
	}
	host.pick = ""
	if canceled, _ := s.SettingsPickDefaultProjectDir(); jsStringify(canceled, "") != `{"canceled":true,"dir":null}` {
		t.Fatalf("cancel %s", jsStringify(canceled, ""))
	}
	cleared, _ := s.SettingsSetDefaultProjectDir(json.RawMessage(`"   "`))
	if jsStringify(cleared, "") != `{"dir":null}` {
		t.Fatalf("clear %s", jsStringify(cleared, ""))
	}
	if raw, _ := os.ReadFile(filepath.Join(s.userData, "project-dir.json")); string(raw) != "{}" {
		t.Fatalf("file %q", raw)
	}
	if _, ok := s.readDefaultProjectDir(); ok {
		t.Fatal("cleared dir still read")
	}
}

func mustJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
