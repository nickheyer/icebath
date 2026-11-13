package fileutil

import (
	"os"
	"path/filepath"
)

// Creates path with missing parents, a new leaf owner-only and an existing leaf chmodded to 0700 on POSIX
func MkdirPrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return mkdirPrivate(path)
}
