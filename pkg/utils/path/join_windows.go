package pathutil

import "path/filepath"

// Elements joined like filepath.Join, except that an absolute element replaces everything before it, a rooted one keeping the drive
func Join(base string, elem ...string) string {
	if p := windowsJoinRaw(base, elem); p != "" {
		return filepath.Clean(p)
	}
	return ""
}
