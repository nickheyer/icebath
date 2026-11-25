package lsp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Relative paths fail as not found, reported as the working directory lookup that failed, once the working directory is removed
func TestRelativePathsFailNotFoundInRemovedCwd(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	const want = "getwd: no such file or directory"
	check := func(name string, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, fs.ErrNotExist) || errReport(err) != want {
			t.Errorf("%s = %v want %s", name, err, want)
		}
	}
	_, err := FileURI("x.py")
	check("FileURI", err)
	_, err = folder("rel")
	check("folder", err)
	c := &Client{docs: newDocMap()}
	_, _, err = c.documentText("x.py")
	check("documentText", err)
	_, err = c.diagnosticsFor("x.py", false)
	check("diagnosticsFor", err)
	svc := &Service{enabled: true}
	_, err = svc.ReleaseWorkspace("rel")
	check("ReleaseWorkspace", err)
	if uri, err := FileURI("/abs/x.py"); err != nil || uri != "file:///abs/x.py" {
		t.Errorf("absolute FileURI = %q, %v", uri, err)
	}
}

func TestNodeModulesTreesResolveExistingPrefixOfMissingBinary(t *testing.T) {
	rt, _, _ := testRuntime(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "node_modules", ".bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	trees, err := nodeModulesTrees(rt, filepath.Join(dir, "link", "missing-server"), "")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := pathutil.Resolve(filepath.Join(dir, "real", "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) == 0 || trees[0] != resolved {
		t.Errorf("trees %v, first want %s", trees, resolved)
	}
}
