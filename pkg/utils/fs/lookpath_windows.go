//go:build windows

package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Extensions of PATHEXT, lower cased with a leading dot, .com .exe .bat and .cmd when it is unset or empty
func executableExts() []string {
	var exts []string
	for e := range strings.SplitSeq(strings.ToLower(os.Getenv("PATHEXT")), ";") {
		if e == "" {
			continue
		}
		if e[0] != '.' {
			e = "." + e
		}
		exts = append(exts, e)
	}
	if len(exts) == 0 {
		return []string{".com", ".exe", ".bat", ".cmd"}
	}
	return exts
}

func hasPathSeparator(cmd string) bool { return strings.ContainsAny(cmd, `:\/`) }

// The file itself when it already has an extension, else the first file of its name plus one of exts, that exists and is no directory
func findExecutable(file string, exts []string) (string, error) {
	if len(exts) == 0 || filepath.Ext(file) != "" {
		err := existingFile(file)
		if err == nil {
			return file, nil
		}
		if len(exts) == 0 {
			return "", err
		}
	}
	for _, ext := range exts {
		if err := existingFile(file + ext); err == nil {
			return file + ext, nil
		}
	}
	return "", fs.ErrNotExist
}

func existingFile(file string) error {
	st, err := os.Stat(file)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return syscall.EISDIR
	}
	return nil
}
