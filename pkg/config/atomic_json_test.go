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
