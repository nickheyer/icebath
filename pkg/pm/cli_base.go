package pm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// LockArgs are pm lock's arguments, a nil Name or Version when not given
type LockArgs struct {
	Name    *string
	Version *string
}

// InstallArgs are pm install's arguments, a nil Target when not given
type InstallArgs struct {
	Names         []string
	Extra         []string
	Without       []string
	ToolsOnly     bool
	TrustRecorded bool
	Target        *string
}

// EnvironmentArgs are pm environment's arguments
type EnvironmentArgs struct {
	Name        string
	Extra       []string
	Requirement []string
}

type cliStderrKey struct{}

// Writer pm commands print their errors to
func cliStderr(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(cliStderrKey{}).(io.Writer); ok {
		return w
	}
	return os.Stderr
}

// Install steps of the install command, replaced by tests the way Python patches the cli and install modules
var (
	installSyncVenv func(ctx context.Context, opts SyncVenvOptions) error
)

// Reports whether two paths are spelled alike once empty and dot elements are dropped, dot-dot kept because it may climb out of a symlink, case and separators folded on Windows
func samePath(a, b string) bool {
	return lexicalPathKey(a) == lexicalPathKey(b)
}

// Comparison key of a path from its anchor and its elements without empty and dot elements, dot-dot kept, case folded on Windows
func lexicalPathKey(p string) string {
	anchor, elems := pathutil.Parts(p)
	return pathutil.FoldCase(anchor + strings.Join(elems, string(filepath.Separator)))
}

// Sorts sibling names the way Python sorts their paths, by code point and lowercased on Windows
func sortPathNames(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		if runtime.GOOS == "windows" {
			return strings.ToLower(names[i]) < strings.ToLower(names[j])
		}
		return tarNameLess(names[i], names[j])
	})
}
