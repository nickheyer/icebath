package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestAtomicJSONWriteKeepsOrderModeAndRawUnicode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "honcho.json")
	if err := os.WriteFile(path, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	obj := pyjson.NewObject()
	obj.Set("z", "café")
	obj.Set("a", []any{1, true})
	if err := AtomicJSONWrite(path, obj); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	want := "{\n  \"z\": \"café\",\n  \"a\": [\n    1,\n    true\n  ]\n}"
	if string(raw) != want {
		t.Fatalf("got %q", raw)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestAtomicJSONWriteModeForcesThePrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shell-hooks-allowlist.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"b": 1, "a": "é"}
	if err := AtomicJSONWriteMode(path, data, 0o600, pyjson.Indent(2), pyjson.SortKeys(true)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{\n  \"a\": \"é\",\n  \"b\": 1\n}" {
		t.Fatalf("written %q", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*tmp")); len(left) != 0 {
		t.Fatalf("temp files left %v", left)
	}
}

func TestReadJSONOrEmptyDegradesToAnEmptyObject(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte("\ufeff{\"trees\": {\"a\": 1}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadJSONOrEmpty(good); len(got) != 1 || got["trees"] == nil {
		t.Fatalf("got %v", got)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("[1, 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{bad, filepath.Join(dir, "missing.json")} {
		if got := ReadJSONOrEmpty(p); len(got) != 0 {
			t.Fatalf("%s: got %v", p, got)
		}
	}
	list := filepath.Join(dir, "list.json")
	if err := os.WriteFile(list, []byte("[1]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadJSONOrEmpty(list); len(got) != 0 {
		t.Fatalf("list: got %v", got)
	}
}
