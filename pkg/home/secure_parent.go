package home

import (
	"fmt"
	"os"
	"strconv"

	"github.com/nickheyer/icebath/pkg/errchain"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

var chmodPath = os.Chmod

// Restricts the resolved parent of path to 0700, never the root, a top level dir or the install tree; a parent that cannot be resolved is left as it is with a warning
func SecureParentDir(path string) {
	parent, err := resolve(pathutil.DirKeepDotDot(path))
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Not restricting permissions on the folder of %s: it cannot be resolved (%s)", strconv.Quote(path), errchain.OSText(err)))
		return
	}
	anchor, parts := pathutil.Parts(parent)
	depth := len(parts)
	if anchor != "" {
		depth++
	}
	if parent == "/" || depth < 3 {
		return
	}
	if root := InstallRoot(); root != "" {
		if _, inside := pathutil.RelativeTo(parent, root); inside {
			pkgLog().Warn(fmt.Sprintf("Not restricting permissions on %s: it is inside the "+
				"icebath-agent install directory (%s). Credential files are "+
				"normally stored under the icebath home directory instead.", parent, root))
			return
		}
	}
	_ = chmodPath(parent, 0o700)
}
