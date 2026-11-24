package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestAtomicJSONWriteKeepsOrderModeAndRawUnicode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "honcho.json")
	if err := os.WriteFile(path, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	obj := jsonutil.NewObject()
	obj.Set("z", "café")
	obj.Set("a", []any{1, true})
	if err := AtomicJSONWrite(context.Background(), path, obj); err != nil {
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
	if err := AtomicJSONWriteMode(context.Background(), path, data, 0o600, jsonutil.Indent(2), jsonutil.SortKeys()); err != nil {
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
