package home

import (
	"os"
	"syscall"
)

// Removes path like shutil.rmtree, chmodding a path that refuses an operation writable and retrying it once
func RmtreeForce(path string) error {
	retry := func(p string, op func() error) error {
		if op() == nil {
			return nil
		}
		_ = os.Chmod(p, 0o200)
		return op()
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return retry(path, func() error { _, err := os.Lstat(path); return err })
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Chmod(path, 0o200)
		return nil
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		children, err := readDirUnsorted(dir)
		if err != nil {
			if err := retry(dir, func() error { _, err := readDirUnsorted(dir); return err }); err != nil {
				return err
			}
			children = nil
		}
		for _, child := range children {
			p := dir + string(os.PathSeparator) + child.Name()
			if child.Type().IsDir() {
				if err := walk(p); err != nil {
					return err
				}
			} else if err := retry(p, func() error { return os.Remove(p) }); err != nil {
				return err
			}
		}
		return retry(dir, func() error { return os.Remove(dir) })
	}
	if !fi.IsDir() {
		_ = os.Chmod(path, 0o200)
		return &os.PathError{Op: "scandir", Path: path, Err: syscall.ENOTDIR}
	}
	return walk(path)
}
