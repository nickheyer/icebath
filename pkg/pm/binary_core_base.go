package pm

import (
	"path/filepath"
)

// Whether a tree ships neither pyproject nor uv.lock, a binary install whose core inputs the binary embeds
func binaryCoreTree(root string) bool {
	return runtimeOnlyProject(root) && !isRegularFile(filepath.Join(root, "uv.lock"))
}

// Whether this install is the binary alone, its plugin Python environment built only for a selection declaring packages
func BinaryInstall() bool { return binaryCoreTree(RepoRoot()) }
