package home

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

var chmodPath = os.Chmod

// Restricts the resolved parent of path to 0700, never the root, a top level dir or the install tree
func SecureParentDir(path string) {
	parent := resolve(pypath.PathParent(path))
	anchor, parts := pypath.PathParts(parent)
	depth := len(parts)
	if anchor != "" {
		depth++
	}
	if parent == "/" || depth < 3 {
		return
	}
	if root := InstallRoot(); root != "" {
		if _, inside := pypath.PathRelativeTo(parent, root); inside {
			slog.Warn(fmt.Sprintf("Not restricting permissions on %s: it is inside the "+
				"icebath-agent install directory (%s). Credential files are "+
				"normally stored under the icebath home directory instead.", parent, root))
			return
		}
	}
	_ = chmodPath(parent, 0o700)
}
