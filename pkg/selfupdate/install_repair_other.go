//go:build !windows

package selfupdate

import (
	"os"
	"strings"
)

// Entries of the user PATH, the process PATH on platforms without a registry
func windowsUserPathEntries() []string {
	out := []string{}
	for _, entry := range strings.Split(os.Getenv("PATH"), ";") {
		if strings.TrimSpace(entry) != "" {
			out = append(out, entry)
		}
	}
	return out
}

// Pending shim renames exist only in the Windows registry
func cleanupPendingShimRenames(string) int { return 0 }
