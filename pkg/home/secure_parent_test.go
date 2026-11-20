package home

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

type chmodCall struct {
	path string
	mode os.FileMode
}

// Records chmod calls instead of applying them
func recordChmod(t *testing.T) *[]chmodCall {
	t.Helper()
	calls := &[]chmodCall{}
	swap(t, &chmodPath, func(p string, m os.FileMode) error { *calls = append(*calls, chmodCall{p, m}); return nil })
	return calls
}

func touchACL(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSecureParentDir(t *testing.T) {
	t.Run("safe_path_calls_chmod", func(t *testing.T) {
		safe := filepath.Join(t.TempDir(), "home", "user", ".icebath")
		mkdirAll(t, safe)
		touchACL(t, filepath.Join(safe, "auth.json"))
		real, _ := filepath.EvalSymlinks(safe)
		calls := recordChmod(t)
		SecureParentDir(filepath.Join(safe, "auth.json"))
		if !slices.Equal(*calls, []chmodCall{{real, 0o700}}) {
			t.Fatal(*calls)
		}
	})
	t.Run("root_dir_skipped", func(t *testing.T) {
		calls := recordChmod(t)
		SecureParentDir("/foo")
		if len(*calls) != 0 {
			t.Fatal(*calls)
		}
	})
	t.Run("install_tree_skipped", func(t *testing.T) {
		root := InstallRoot()
		calls := recordChmod(t)
		SecureParentDir(filepath.Join(root, "auth.json"))
		SecureParentDir(filepath.Join(root, "subdir", "auth.json"))
		if len(*calls) != 0 {
			t.Fatal("must not chmod the install root or dirs inside it", *calls)
		}
	})
	t.Run("install_tree_siblings_still_hardened", func(t *testing.T) {
		root := InstallRoot()
		prefixSibling := root + "-data"
		calls := recordChmod(t)
		SecureParentDir(filepath.Join(prefixSibling, "auth.json"))
		if !slices.Equal(*calls, []chmodCall{{prefixSibling, 0o700}}) {
			t.Fatal("prefix-named siblings of the install root must still be hardened", *calls)
		}
		sibling := filepath.Join(filepath.Dir(root), "unrelated-dir")
		if pathDepth(sibling) >= 3 {
			*calls = nil
			SecureParentDir(filepath.Join(sibling, "auth.json"))
			if !slices.Equal(*calls, []chmodCall{{sibling, 0o700}}) {
				t.Fatal("siblings of the install root must still be hardened", *calls)
			}
		}
	})
	t.Run("symlink_resolved", func(t *testing.T) {
		tmp := t.TempDir()
		realDir := mkdirAll(t, filepath.Join(tmp, "a", "b"))
		touchACL(t, filepath.Join(realDir, "file.json"))
		link := filepath.Join(tmp, "link")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		resolved, _ := filepath.EvalSymlinks(realDir)
		calls := recordChmod(t)
		SecureParentDir(filepath.Join(link, "file.json"))
		if !slices.Equal(*calls, []chmodCall{{resolved, 0o700}}) {
			t.Fatal(*calls)
		}
	})
}

// Number of pathlib parts including the anchor
func pathDepth(p string) int {
	anchor, parts := pypath.PathParts(p)
	if anchor != "" {
		return len(parts) + 1
	}
	return len(parts)
}
