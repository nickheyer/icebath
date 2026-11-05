package pm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCorruptShippedFactsAreOnlyRead(t *testing.T) {
	isolateHome(t)
	tmp := realpath(t.TempDir())
	store := filepath.Join(tmp, "payload", "tools")
	if err := os.MkdirAll(store, 0o777); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(store), "manifest.json"), []byte("{}"), 0o666)
	facts := filepath.Join(store, "facts.json")
	writeFile(t, facts, []byte("{broken"), 0o666)
	t.Setenv(EnvRuntimeDir, store)
	if root, err := StoreRoot(contextBackground()); err != nil || root != store {
		t.Fatalf("store root = %q %v", root, err)
	}
	f, err := NewFacts(facts, false)
	if err != nil {
		t.Fatal(err)
	}
	if fact, ok := f.Get("anything"); ok || fact != nil {
		t.Fatalf("damaged facts produced %v", fact)
	}
	if string(readFile(t, facts)) != "{broken" {
		t.Fatal("shipped facts were rewritten")
	}
	if zsLexists(filepath.Join(store, "facts.corrupt")) {
		t.Fatal("shipped facts were copied aside")
	}
}
