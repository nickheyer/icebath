package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Python str of an uninstall removal error, the bare rmtree symlink refusal or the OS error text
func uninstallErrText(err error) string {
	if errors.Is(err, pyfs.ErrRmTreeSymlink) {
		return pyfs.ErrRmTreeSymlink.Error()
	}
	return pyfs.OSErrorText(err)
}

// Deletes the node tree and uv binaries a pre split install left in the home, returning what it removed
func RemoveLegacyRuntimeTrees(home string, w io.Writer) []string {
	warn := func(msg string) { fmt.Fprintf(w, "%s %s\n", cliui.Color("⚠", cliui.Yellow), msg) }
	removed := []string{}
	nodeTree := filepath.Join(home, "node")
	if st, err := os.Stat(nodeTree); err == nil && st.IsDir() {
		if err := pyfs.RmTree(nodeTree, nil); err != nil {
			warn(fmt.Sprintf("Could not remove %s: %s", nodeTree, uninstallErrText(err)))
		} else {
			removed = append(removed, nodeTree)
		}
	}
	for _, uvName := range []string{"uv", "uv.exe"} {
		uvBinary := filepath.Join(home, "bin", uvName)
		if st, err := os.Stat(uvBinary); err == nil && st.Mode().IsRegular() {
			if err := os.Remove(uvBinary); err != nil {
				warn(fmt.Sprintf("Could not remove %s: %s", uvBinary, uninstallErrText(err)))
			} else {
				removed = append(removed, uvBinary)
			}
		}
	}
	return removed
}
