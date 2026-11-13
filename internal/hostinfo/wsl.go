package hostinfo

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var (
	windowsDrivePath = regexp.MustCompile(`^([A-Za-z]):[\\/]([^\n]*)$`)
	wslUNCPath       = regexp.MustCompile(`(?i)^\\\\wsl(?:\.localhost|\$)\\[^\\]+\\([^\n]*)$`)
)

// Mount form under /mnt of a Windows drive path, false when path is not one
func WindowsPathToWSL(path string) (string, bool) {
	m := windowsDrivePath.FindStringSubmatch(strings.TrimSpace(path))
	if m == nil {
		return "", false
	}
	return "/mnt/" + strings.ToLower(m[1]) + "/" + strings.ReplaceAll(m[2], `\`, "/"), true
}

// POSIX path inside a wsl.localhost or wsl$ UNC path, false when path is not one
func WSLUNCPathToPOSIX(path string) (string, bool) {
	m := wslUNCPath.FindStringSubmatch(strings.ReplaceAll(strings.TrimSpace(path), "/", `\`))
	if m == nil {
		return "", false
	}
	return "/" + strings.ReplaceAll(m[1], `\`, "/"), true
}

// Windows host cwd mapped to POSIX when this process runs inside WSL, unchanged elsewhere or when already POSIX
func (h *Host) TranslateCwdForWSL(cwd string) string {
	if !h.WSL() {
		return cwd
	}
	if translated, ok := WSLUNCPathToPOSIX(cwd); ok {
		return translated
	}
	if translated, ok := WindowsPathToWSL(cwd); ok {
		return translated
	}
	return cwd
}

// Windows form of a path inside WSL as wslpath prints it, ErrAbsent where wslpath does not exist
func WSLPathToWindows(ctx context.Context, path string) (string, error) {
	return wslpath(ctx, "-w", path)
}

// WSL form of a Windows path as wslpath prints it, ErrAbsent where wslpath does not exist
func WSLPathFromWindows(ctx context.Context, path string) (string, error) {
	return wslpath(ctx, "-u", path)
}

// Converted path the WSL path tool prints for path in the direction flag selects
func wslpath(ctx context.Context, flag, path string) (string, error) {
	out, _, err := runProgram(ctx, "wslpath", []string{"/usr/bin/wslpath", "/bin/wslpath"}, flag, path)
	if err != nil {
		return "", fmt.Errorf("wslpath %s %s: %w", flag, path, err)
	}
	converted := strings.TrimSpace(string(out))
	if converted == "" {
		return "", fmt.Errorf("wslpath %s %s printed no path", flag, path)
	}
	return converted, nil
}
