//go:build windows

package skills

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Marker for a working folder a snippet can't start in, none when it can
func badFolder(dir string) string {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return ""
	}
	return fmt.Sprintf("[inline-shell error: %s: %s]", windows.ERROR_DIRECTORY.Error(), dir)
}
