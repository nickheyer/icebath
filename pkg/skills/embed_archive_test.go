package skills

import (
	"io/fs"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Fails unless every manifest row names an archived file with the same digest and mode and nothing else is archived
func assertManifestMatchesArchive(t *testing.T, archive []byte, manifestPath string) {
	t.Helper()
	files, err := tarzst.Decode(archive)
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tarzst.ParseManifest(string(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(files) {
		t.Fatalf("%s lists %d files, the archive holds %d", manifestPath, len(rows), len(files))
	}
	for i, f := range files {
		if rows[i].Path != f.Path || rows[i].Sum != f.Sum || rows[i].Mode != f.Mode {
			t.Fatalf("%s row %+v, archive %s %s %04o", manifestPath, rows[i], f.Path, f.Sum, f.Mode)
		}
	}
	if string(text) != tarzst.Manifest(files) {
		t.Fatalf("%s is not the canonical manifest of its archive", manifestPath)
	}
}

func TestBundledManifestMatchesArchive(t *testing.T) {
	assertManifestMatchesArchive(t, bundledArchive, "bundled_manifest.txt")
}

func TestOptionalManifestMatchesArchive(t *testing.T) {
	assertManifestMatchesArchive(t, optionalArchive, "optional_manifest.txt")
}

func TestEmbeddedSkillTreesServeEveryArchivedFile(t *testing.T) {
	for name, tree := range map[string]struct {
		archive []byte
		fsys    fs.FS
	}{"bundled": {bundledArchive, BundledFS()}, "optional": {optionalArchive, OptionalFS()}} {
		files, err := tarzst.Decode(tree.archive)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			data, err := fs.ReadFile(tree.fsys, f.Path)
			if err != nil || string(data) != string(f.Data) {
				t.Fatalf("%s %s served %v", name, f.Path, err)
			}
		}
	}
}
