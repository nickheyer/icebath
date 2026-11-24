package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Executable cmd names, searched in each directory of path, a list in the form of PATH, by exec.LookPath's rules except that POSIX judges execute permission for the real user: a cmd holding a separator is checked as given, Windows tries the PATHEXT extensions and skips empty entries where POSIX reads them as the working directory, an empty list finds nothing, and a first hit through a relative directory comes back with an error wrapping exec.ErrDot, which on Windows a later absolute directory holding that same file turns into a plain hit
func LookPathIn(cmd, path string) (string, error) {
	switch cmd {
	case "", ".", "..":
		return "", &exec.Error{Name: cmd, Err: exec.ErrNotFound}
	}
	exts := executableExts()
	if hasPathSeparator(cmd) {
		found, err := findExecutable(cmd, exts)
		if err != nil {
			return "", &exec.Error{Name: cmd, Err: err}
		}
		return found, nil
	}
	windows := runtime.GOOS == "windows"
	dot := ""
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			if windows {
				continue
			}
			dir = "."
		}
		found, err := findExecutable(filepath.Join(dir, cmd), exts)
		if err != nil {
			continue
		}
		if dot != "" && !sameEntry(dot, found) {
			break
		}
		if filepath.IsAbs(found) {
			return found, nil
		}
		if !windows {
			return found, &exec.Error{Name: cmd, Err: exec.ErrDot}
		}
		if dot == "" {
			dot = found
		}
	}
	if dot != "" {
		return dot, &exec.Error{Name: cmd, Err: exec.ErrDot}
	}
	return "", &exec.Error{Name: cmd, Err: exec.ErrNotFound}
}

// Whether two paths name the same directory entry, a final symlink not followed
func sameEntry(a, b string) bool {
	ai, err := os.Lstat(a)
	if err != nil {
		return false
	}
	bi, err := os.Lstat(b)
	return err == nil && os.SameFile(ai, bi)
}
