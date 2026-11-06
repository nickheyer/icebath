package pyshutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Error shutil.rmtree raises when its target is a symbolic link
var ErrRmtreeSymlink = errors.New("Cannot call rmtree on a symbolic link")

// shutil.rmtree without onexc, refusing a symlink root and stopping at the first failing entry
func Rmtree(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return ErrRmtreeSymlink
	}
	return rmtreeDir(path)
}

// Removes the entries of a directory in listing order, recursing into real directories, then the directory
func rmtreeDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		child := filepath.Join(path, name)
		fi, err := os.Lstat(child)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if fi.IsDir() {
			if err := rmtreeDir(child); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(child); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return syscallRmdir(path)
}

// Path error of a failed removal
type osPathError = fs.PathError
