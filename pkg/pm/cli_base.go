package pm

import (
	"context"
	"io"
	"os"
	"runtime"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// LockArgs are pm lock's arguments, a nil Name or Version when not given
type LockArgs struct {
	Name    *string
	Version *string
}

// InstallArgs are pm install's arguments, a nil Target or TestEnvironment when not given
type InstallArgs struct {
	Names           []string
	Extra           []string
	Without         []string
	ToolsOnly       bool
	TrustRecorded   bool
	TestEnvironment *string
	Target          *string
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

// Reports whether two paths are equal as Python Path objects compare, lowercased on Windows
func samePath(a, b string) bool {
	a, b = pypath.PurePathStr(a), pypath.PurePathStr(b)
	if runtime.GOOS == "windows" {
		return pystr.Lower(a) == pystr.Lower(b)
	}
	return a == b
}

// Sorts sibling names the way Python sorts their paths, by code point and lowercased on Windows
func sortPathNames(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		if runtime.GOOS == "windows" {
			return pystr.Lower(names[i]) < pystr.Lower(names[j])
		}
		return tarNameLess(names[i], names[j])
	})
}
