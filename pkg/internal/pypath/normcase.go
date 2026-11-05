package pypath

import (
	"runtime"
	"strings"
)

// Host os.path.normcase, lowercased with backslash separators on Windows and unchanged elsewhere
func Normcase(p string) string {
	if runtime.GOOS == "windows" {
		return NtNormcase(p)
	}
	return p
}

// ntpath.normcase, forward slashes become backslashes and the text is lowercased
func NtNormcase(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "/", `\`))
}
