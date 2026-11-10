package home

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// WSL probe, swapped by tests that simulate a WSL host
var isWSL = hostinfo.IsWSL

var (
	windowsDrivePath = regexp.MustCompile(`^([A-Za-z]):[\\/]([^\n]*)$`)
	wslUNCPath       = regexp.MustCompile(`(?i)^\\\\wsl(?:\.localhost|\$)\\[^\\]+\\([^\n]*)$`)
)

// The /mnt drive form of a Windows drive path, false when path is not one
func WindowsPathToWSL(path string) (string, bool) {
	m := windowsDrivePath.FindStringSubmatch(pystr.Strip(path))
	if m == nil {
		return "", false
	}
	return "/mnt/" + strings.ToLower(m[1]) + "/" + strings.ReplaceAll(m[2], `\`, "/"), true
}

// The POSIX path inside a wsl.localhost or wsl$ UNC path, false when path is not one
func WSLUNCPathToPosix(path string) (string, bool) {
	normalized := strings.ReplaceAll(pystr.Strip(path), "/", `\`)
	m := wslUNCPath.FindStringSubmatch(normalized)
	if m == nil {
		return "", false
	}
	return "/" + strings.ReplaceAll(m[1], `\`, "/"), true
}

// A Windows host cwd mapped to POSIX when icebath runs inside WSL, unchanged elsewhere or when already POSIX
func TranslateCwdForWSLBackend(cwd string) string {
	if !isWSL() {
		return cwd
	}
	for _, translate := range []func(string) (string, bool){WSLUNCPathToPosix, WindowsPathToWSL} {
		if translated, ok := translate(cwd); ok {
			return translated
		}
	}
	return cwd
}
