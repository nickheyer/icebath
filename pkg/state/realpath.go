package state

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// Resolves symlinks component by component like Python's non strict Path.resolve, keeping missing tails literal
func realpathLax(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return realpathWalk(string(filepath.Separator), strings.Split(abs, string(filepath.Separator)), 0)
}

// Walks path components under a resolved prefix, following at most 40 nested links per component chain
func realpathWalk(resolved string, parts []string, depth int) string {
	if vol := filepath.VolumeName(parts[0]); vol != "" {
		resolved, parts = vol+string(filepath.Separator), parts[1:]
	}
	for i, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		info, err := os.Lstat(next)
		if err != nil {
			rest := append([]string{next}, parts[i+1:]...)
			return filepath.Clean(filepath.Join(rest...))
		}
		if info.Mode()&os.ModeSymlink == 0 || depth >= 40 {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			resolved = next
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(resolved, target)
		}
		resolved = realpathWalk(string(filepath.Separator), strings.Split(filepath.Clean(target), string(filepath.Separator)), depth+1)
	}
	return resolved
}

// Expands a leading tilde to the user's home like Python's expanduser
func expandUser(path string) string {
	windows := runtime.GOOS == "windows"
	if path != "~" && !strings.HasPrefix(path, "~/") && !(windows && strings.HasPrefix(path, `~\`)) {
		return path
	}
	home, ok := pyUserHome()
	if !ok {
		return path
	}
	if windows {
		return home + path[1:]
	}
	if out := strings.TrimRight(home, "/") + path[1:]; out != "" {
		return out
	}
	return "/"
}

// Home Python's expanduser substitutes for a bare tilde, false when it leaves the path untouched
func pyUserHome() (string, bool) {
	if runtime.GOOS == "windows" {
		if profile, ok := os.LookupEnv("USERPROFILE"); ok {
			return profile, true
		}
		homePath, ok := os.LookupEnv("HOMEPATH")
		if !ok {
			return "", false
		}
		if drive := os.Getenv("HOMEDRIVE"); drive != "" {
			return filepath.Join(drive, homePath), true
		}
		return homePath, true
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		return home, true
	}
	u, err := user.Current()
	if err != nil {
		return "", false
	}
	return u.HomeDir, true
}
