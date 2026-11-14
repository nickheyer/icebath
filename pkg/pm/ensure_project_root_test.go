package pm

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Replaces the import path for one test and restores it afterwards
func swapImportPath(t *testing.T, entries []string) {
	t.Helper()
	importPath.Lock()
	saved := importPath.entries
	importPath.entries = entries
	importPath.Unlock()
	t.Cleanup(func() {
		importPath.Lock()
		importPath.entries = saved
		importPath.Unlock()
	})
}

func TestEnsureProjectRootOnPathDedupesRealpathEquivalents(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	other := filepath.Join(base, "other")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(PatchRepoRoot(root))
	sep := string(os.PathSeparator)
	swapImportPath(t, []string{root + sep + "missing" + sep + "..", "", other, root, root + sep + "."})
	EnsureProjectRootOnPath()
	if got, want := ImportPathEntries(), []string{root, "", other}; !slices.Equal(got, want) {
		t.Fatalf("import path %q want %q", got, want)
	}
	EnsureProjectRootOnPath()
	if got, want := ImportPathEntries(), []string{root, "", other}; !slices.Equal(got, want) {
		t.Fatalf("second pass %q want %q", got, want)
	}
}
