package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// File URI of a path, failing the test when the cwd cannot anchor it
func mustURI(t testing.TB, p string) string {
	t.Helper()
	uri, err := FileURI(p)
	if err != nil {
		t.Fatal(err)
	}
	return uri
}

// Scheduled edit check, failing the test when scheduling raises
func mustStartEditCheck(t testing.TB, svc *Service, path string, preText, postText *string, owner string, carry []Note, roots map[string]bool) *EditFuture {
	t.Helper()
	fut, err := svc.StartEditCheck(path, preText, postText, owner, carry, roots)
	if err != nil {
		t.Fatal(err)
	}
	return fut
}

func TestRelativePathsRaiseFileNotFoundInRemovedCwd(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	const want = "FileNotFoundError:[Errno 2] No such file or directory"
	check := func(name string, err error) {
		t.Helper()
		if err == nil || errTypeName(err)+":"+errText(err) != want {
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
	resolved, err := pypath.Realpath(filepath.Join(dir, "real", "node_modules"), pypath.Lax)
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) == 0 || trees[0] != resolved {
		t.Errorf("trees %v, first want %s", trees, resolved)
	}
}
