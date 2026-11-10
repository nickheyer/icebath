package pyshutil

import (
	"io"
	"os"
)

// shutil.copy2 of a file onto dst, content then permission bits, access and modification times and xattrs
func Copy2(src, dst string) error {
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		dst = dst + string(os.PathSeparator) + baseName(src)
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return copyStat(src, dst)
}

// Last path element of p split on the host separators
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if os.IsPathSeparator(p[i]) {
			return p[i+1:]
		}
	}
	return p
}

// shutil.copyfile content copy refusing to copy a file onto itself
func copyFile(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return &SameFileError{Src: src, Dst: dst}
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// CopyFile is shutil.copyfile, the content alone copied and a copy onto itself refused
func CopyFile(src, dst string) error { return copyFile(src, dst) }

// shutil.copystat of mode bits, atime and mtime with nanoseconds and extended attributes
func copyStat(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.Chtimes(dst, fileAtime(fi), fi.ModTime()); err != nil {
		return err
	}
	if err := copyXattr(src, dst); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm()|fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
}

// Error shutil raises when source and destination name the same file
type SameFileError struct{ Src, Dst string }

func (e *SameFileError) Error() string {
	return "'" + e.Src + "' and '" + e.Dst + "' are the same file"
}
