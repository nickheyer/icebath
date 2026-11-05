package pyfs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// shutil.Error raised for a move whose destination exists or lies inside the source
type MoveError struct{ Msg string }

func (e *MoveError) Error() string { return e.Msg }

// Moves a file or tree like shutil.move, renaming when possible and copying then deleting across devices
func Move(src, dst string) (string, error) {
	realDst := dst
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		if sameFile(src, dst) && !isLink(src) {
			return dst, os.Rename(src, dst)
		}
		realDst = filepath.Join(dst, filepath.Base(strings.TrimRight(src, string(os.PathSeparator))))
		if _, err := os.Lstat(realDst); err == nil {
			return "", &MoveError{fmt.Sprintf("Destination path '%s' already exists", realDst)}
		}
	}
	if err := os.Rename(src, realDst); err == nil {
		return realDst, nil
	}
	if isLink(src) {
		target, err := os.Readlink(src)
		if err != nil {
			return "", err
		}
		if err := os.Symlink(target, realDst); err != nil {
			return "", err
		}
		return realDst, os.Remove(src)
	}
	if st, err := os.Stat(src); err == nil && st.IsDir() {
		if destInSrc(src, dst) {
			return "", &MoveError{fmt.Sprintf("Cannot move a directory '%s' into itself '%s'.", src, dst)}
		}
		if err := copyTree(src, realDst); err != nil {
			return "", err
		}
		return realDst, os.RemoveAll(src)
	}
	if err := copyFile2(src, realDst); err != nil {
		return "", err
	}
	return realDst, os.Remove(src)
}

// Reports whether both paths name the same file
func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// Reports whether the path is a symlink
func isLink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// Reports whether dst lies inside src after absolutizing both
func destInSrc(src, dst string) bool {
	s, _ := filepath.Abs(src)
	d, _ := filepath.Abs(dst)
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(s, sep) {
		s += sep
	}
	if !strings.HasSuffix(d, sep) {
		d += sep
	}
	return strings.HasPrefix(d, s)
}

// Copies a tree keeping symlinks as links and file data, modes and times like copytree with copy2
func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, st.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case isLink(s):
			target, err := os.Readlink(s)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, d); err != nil {
				return err
			}
		case e.IsDir():
			if err := copyTree(s, d); err != nil {
				return err
			}
		default:
			if err := copyFile2(s, d); err != nil {
				return err
			}
		}
	}
	os.Chmod(dst, st.Mode().Perm())
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// Copies file data, permission bits and modification time like shutil.copy2
func copyFile2(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Chmod(dst, st.Mode().Perm())
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}
