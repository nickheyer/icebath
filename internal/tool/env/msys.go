package env

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
)

var (
	msysDrivePath   = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])(/.*)?$`)
	nativeDrivePath = regexp.MustCompile(`^([a-zA-Z]):[\\/]*(.*)$`)
)

func MsysToWindowsPath(cwd string) string {
	if !windowsHost || cwd == "" {
		return cwd
	}
	m := msysDrivePath.FindStringSubmatch(cwd)
	if m == nil {
		return cwd
	}
	tail := strings.ReplaceAll(m[2], "/", `\`)
	if tail == "" {
		tail = `\`
	}
	return strings.ToUpper(m[1]) + ":" + tail
}

func windowsToMsysPath(cwd string) string {
	if !windowsHost || cwd == "" {
		return cwd
	}
	m := nativeDrivePath.FindStringSubmatch(cwd)
	if m == nil {
		return cwd
	}
	tail := strings.TrimLeft(strings.ReplaceAll(m[2], `\`, "/"), "/")
	return "/" + strings.ToLower(m[1]) + "/" + tail
}

func BashSafePath(p string) string {
	if !windowsHost || p == "" {
		return p
	}
	return strings.ReplaceAll(windowsToMsysPath(p), `\`, "/")
}

func quoteBashPath(p string) string {
	return shell.Quote(BashSafePath(p))
}

func pathParts(p string) []string {
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	var parts []string
	if vol != "" || strings.HasPrefix(rest, string(filepath.Separator)) || strings.HasPrefix(rest, "/") {
		root := vol
		if strings.HasPrefix(rest, string(filepath.Separator)) || strings.HasPrefix(rest, "/") {
			root += string(filepath.Separator)
		}
		parts = append(parts, root)
	}
	for _, seg := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if seg != "." {
			parts = append(parts, seg)
		}
	}
	return parts
}

func resolveLocalInitialCwd(cwd string) (string, error) {
	expanded := home.ExpandUser(cwd)
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		expanded = wd
	}
	if windowsHost {
		expanded = MsysToWindowsPath(expanded)
	}
	if filepath.IsAbs(expanded) {
		return expanded, nil
	}
	candidate, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if !isDir(candidate) {
		wanted, have := pathParts(expanded), pathParts(current)
		if len(wanted) > 0 && len(wanted) <= len(have) {
			tail := have[len(have)-len(wanted):]
			match := true
			for i := range wanted {
				if wanted[i] != tail[i] {
					match = false
					break
				}
			}
			if match {
				return current, nil
			}
		}
	}
	return candidate, nil
}

func cwdUsable(p string) bool {
	return isDir(p) && fileutil.Accessible(p, fileutil.AccessExec)
}

func parentDir(p string) string {
	vol := filepath.VolumeName(p)
	if !strings.ContainsAny(p[len(vol):], `/`+string(filepath.Separator)) {
		return vol
	}
	return filepath.Dir(p)
}

func SafeCwd(cwd string) string {
	cwd = MsysToWindowsPath(cwd)
	if cwd != "" && cwdUsable(cwd) {
		return cwd
	}
	if cwd != "" && isDir(cwd) {
		slog.Warn(fmt.Sprintf("Configured terminal cwd %s exists but is not accessible to "+
			"this user (uid=%s) — falling back to the nearest usable "+
			"directory. If this is a gateway/cron process, check for "+
			"root-owned paths leaking into terminal.cwd / TERMINAL_CWD "+
			"(#65583).", quotedPath(cwd), uidLabel()))
	}
	parent := ""
	if cwd != "" {
		parent = parentDir(cwd)
	}
	for parent != "" && !cwdUsable(parent) {
		next := parentDir(parent)
		if next == parent {
			return os.TempDir()
		}
		parent = next
	}
	if parent == "" {
		return os.TempDir()
	}
	return parent
}

func uidLabel() string {
	if uid := os.Getuid(); uid >= 0 {
		return fmt.Sprint(uid)
	}
	return "?"
}
