package pyshutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// shutil.copytree with a copy_function, links kept when symlinks, an existing dst merged when dirsExistOK
func CopyTreeFunc(src, dst string, symlinks, dirsExistOK bool, copyFunction func(src, dst string) error) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil && !dirsExistOK {
		return fmt.Errorf("[Errno 17] File exists: '%s'", dst)
	}
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		from, to := filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())
		info, err := os.Lstat(from)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0 && symlinks:
			target, err := os.Readlink(from)
			if err == nil {
				err = os.Symlink(target, to)
			}
			if err == nil {
				err = CopyLinkStat(from, to)
			}
			if err != nil {
				failures = append(failures, err)
			}
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := os.Stat(from)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if resolved.IsDir() {
				err = CopyTreeFunc(from, to, symlinks, dirsExistOK, copyFunction)
			} else {
				err = copyFunction(from, to)
			}
			if err != nil {
				failures = append(failures, err)
			}
		case info.IsDir():
			if err := CopyTreeFunc(from, to, symlinks, dirsExistOK, copyFunction); err != nil {
				failures = append(failures, err)
			}
		default:
			if err := copyFunction(from, to); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := copyStat(src, dst); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// Access time of a stat result with the nanoseconds the platform records
func FileAtime(fi os.FileInfo) time.Time { return fileAtime(fi) }
