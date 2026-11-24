package pathutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Directories a temporary directory is chosen from, in order: TMPDIR, TEMP and TMP when set, the platform's usual places, then the working directory
func TempDirCandidates() []string {
	dirs := make([]string, 0, 10)
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		if v := os.Getenv(name); v != "" {
			dirs = append(dirs, v)
		}
	}
	if runtime.GOOS == "windows" {
		if h, err := Home(); err == nil {
			dirs = append(dirs, filepath.Join(h, "AppData", "Local", "Temp"))
		}
		if root := os.Getenv("SYSTEMROOT"); root != "" {
			dirs = append(dirs, filepath.Join(root, "Temp"))
		}
		dirs = append(dirs, `c:\temp`, `c:\tmp`, `\temp`, `\tmp`)
	} else {
		dirs = append(dirs, "/tmp", "/var/tmp", "/usr/tmp")
	}
	if cwd, err := os.Getwd(); err == nil {
		return append(dirs, cwd)
	}
	return append(dirs, ".")
}

// First of the TempDirCandidates in which this process can create, write and remove a file, made absolute
func TempDir() (string, error) {
	dirs := TempDirCandidates()
	for _, dir := range dirs {
		if dir != "." {
			if abs, err := filepath.Abs(dir); err == nil {
				dir = abs
			}
		}
		if writableDir(dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no usable temporary directory among %q: %w", dirs, fs.ErrNotExist)
}

// Whether a probe file can be created, written and removed in dir
func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-")
	if err != nil {
		return false
	}
	_, werr := f.WriteString("probe")
	cerr := f.Close()
	rerr := os.Remove(f.Name())
	return werr == nil && cerr == nil && rerr == nil
}
