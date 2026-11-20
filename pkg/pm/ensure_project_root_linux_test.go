package pm

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEnsureProjectRootOnPathSurvivesADeletedCwd(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(base, "scratch")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(PatchRepoRoot(base))
	swapImportPath(t, []string{"relative-entry", base})
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if got := realpathOrSelf("relative-entry"); got != "relative-entry" {
		t.Fatalf("relative entry resolved to %q", got)
	}
	EnsureProjectRootOnPath()
	if got, want := ImportPathEntries(), []string{base, "relative-entry"}; !slices.Equal(got, want) {
		t.Fatalf("import path %q want %q", got, want)
	}
}
