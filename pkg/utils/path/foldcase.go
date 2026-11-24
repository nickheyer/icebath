package pathutil

import "runtime"

// Comparison key of a path for a host whose file names ignore case: lower case with backslash separators on Windows, the path itself elsewhere
func FoldCase(p string) string {
	if runtime.GOOS == "windows" {
		return WindowsFoldCase(p)
	}
	return p
}
