package cards

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathsCompleted(t *testing.T) {
	dir := t.TempDir()
	for _, folder := range []string{"alpha", "alpine", "beta", "ünïcode-one", "ünïcode-two"} {
		if err := os.Mkdir(filepath.Join(dir, folder), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "beta", "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	for typed, want := range map[string]string{
		// One that fits is written in, several write in what they all start with
		dir + "/b":     dir + "/beta/",
		dir + "/al":    dir + "/alp",
		dir + "/beta/": dir + "/beta/",
		// What several share is cut letter by letter, never inside one
		dir + "/ü": dir + "/ünïcode-",
		// Of text that names several folders as PATH separates them, the last is completed and those before it stay
		dir + "/alpha" + sep + dir + "/be":               dir + "/alpha" + sep + dir + "/beta/",
		"/nowhere/at/all" + sep + dir + "/alpi":          "/nowhere/at/all" + sep + dir + "/alpine/",
		dir + "/alpha" + sep + dir + "/beta" + sep + "~": dir + "/alpha" + sep + dir + "/beta" + sep + "~/",
	} {
		form := NewForm(&Env{ProjectDir: dir}, FormSpec{Fields: []Field{{ID: "path", Value: typed, Path: PathFolder}}})
		form.complete()
		if got := form.Values()["path"]; got != want {
			t.Errorf("%q completed to %q, want %q", typed, got, want)
		}
	}
	// A file is offered where files are asked for, and left out where folders are
	for kind, want := range map[PathKind]int{PathFile: 1, PathFolder: 0} {
		if got := len(PathOffers(dir+"/beta/no", dir, kind, 10)); got != want {
			t.Errorf("kind %d: %d offers for a file, want %d", kind, got, want)
		}
	}
}
