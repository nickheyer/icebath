package pypath

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// Candidate temp roots in tempfile order, the env vars then the platform defaults then the cwd
func tempdirCandidates() []string {
	var dirs []string
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		if v := os.Getenv(name); v != "" {
			dirs = append(dirs, v)
		}
	}
	if runtime.GOOS == "windows" {
		home, _ := os.UserHomeDir()
		dirs = append(dirs, filepath.Join(home, "AppData", "Local", "Temp"), os.ExpandEnv(`${SYSTEMROOT}\Temp`), `c:\temp`, `c:\tmp`, `\temp`, `\tmp`)
	} else {
		dirs = append(dirs, "/tmp", "/var/tmp", "/usr/tmp")
	}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	} else {
		dirs = append(dirs, ".")
	}
	return dirs
}

// tempfile.gettempdir, the first candidate where a probe file can be created, written and removed
func Gettempdir() (string, error) {
	dirs := tempdirCandidates()
	for _, dir := range dirs {
		if dir != "." {
			if abs, err := filepath.Abs(dir); err == nil {
				dir = abs
			}
		}
	probes:
		for range 100 {
			var raw [6]byte
			_, _ = rand.Read(raw[:])
			name := filepath.Join(dir, fmt.Sprintf("%x", raw))
			f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			switch {
			case err == nil:
				_, _ = f.Write([]byte("blat"))
				f.Close()
				os.Remove(name)
				return dir, nil
			case errors.Is(err, fs.ErrExist):
				continue
			case errors.Is(err, fs.ErrPermission) && runtime.GOOS == "windows":
				if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
					continue
				}
				break probes
			default:
				break probes
			}
		}
	}
	return "", &fs.PathError{Op: "gettempdir", Path: fmt.Sprintf("No usable temporary directory found in %q", dirs), Err: syscall.ENOENT}
}
