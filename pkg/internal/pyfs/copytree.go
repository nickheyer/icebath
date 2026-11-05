package pyfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Failures gathered by a tree copy as source, destination and reason triples
type CopyTreeError struct{ Failures [][3]string }

func (e *CopyTreeError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = "(" + pyrepr.StrRepr(f[0]) + ", " + pyrepr.StrRepr(f[1]) + ", " + pyrepr.StrRepr(f[2]) + ")"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Names of a directory listing a tree copy skips
type IgnoreFunc func(dir string, names []string) map[string]bool

// Copies file data, permission bits and times into dst or into dst when it is a directory
func Copy2(src, dst string) error {
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		dst = filepath.Join(dst, filepath.Base(src))
	}
	return copyFile2(src, dst)
}

// Copies a directory tree the way shutil.copytree does with copy2, failures gathered into one error
func CopyTree(src, dst string, symlinks, dirsExistOK bool, ignore IgnoreFunc) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	ignored := map[string]bool{}
	if ignore != nil {
		ignored = ignore(src, names)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o777); err != nil && !(dirsExistOK && isDirPath(dst)) {
		return err
	}
	var failures [][3]string
	fail := func(from, to string, err error) {
		var tree *CopyTreeError
		if e, ok := err.(*CopyTreeError); ok {
			tree = e
		}
		if tree != nil {
			failures = append(failures, tree.Failures...)
			return
		}
		failures = append(failures, [3]string{from, to, OSErrorText(err)})
	}
	for _, name := range names {
		if ignored[name] {
			continue
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		st, err := os.Lstat(from)
		if err != nil {
			fail(from, to, err)
			continue
		}
		if st.Mode()&fs.ModeSymlink != 0 && !isMountPoint(from) {
			linkto, linkErr := os.Readlink(from)
			if linkErr != nil {
				fail(from, to, linkErr)
				continue
			}
			if symlinks {
				if err := os.Symlink(linkto, to); err != nil {
					fail(from, to, err)
					continue
				}
				if err := copySymlinkTimes(from, to); err != nil {
					fail(from, to, err)
				}
				continue
			}
			if isDirPath(from) {
				err = CopyTree(from, to, symlinks, dirsExistOK, ignore)
			} else {
				err = Copy2(from, to)
			}
		} else if st.IsDir() || isMountPoint(from) {
			err = CopyTree(from, to, symlinks, dirsExistOK, ignore)
		} else {
			err = Copy2(from, to)
		}
		if err != nil {
			fail(from, to, err)
		}
	}
	if st, err := os.Stat(src); err == nil {
		if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
			failures = append(failures, [3]string{src, dst, OSErrorText(err)})
		} else if err := os.Chtimes(dst, st.ModTime(), st.ModTime()); err != nil {
			failures = append(failures, [3]string{src, dst, OSErrorText(err)})
		}
	}
	if len(failures) > 0 {
		return &CopyTreeError{failures}
	}
	return nil
}

func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Python str of an OS error, errno number and description followed by the quoted path
func OSErrorText(err error) string {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		var le *os.LinkError
		if errors.As(err, &le) {
			return errnoText(le.Err) + ": " + pyrepr.StrRepr(le.Old) + " -> " + pyrepr.StrRepr(le.New)
		}
		return err.Error()
	}
	return errnoText(pe.Err) + ": " + pyrepr.StrRepr(pe.Path)
}

// Errno number and capitalized description the way Python strerror spells them
func errnoText(err error) string {
	text := err.Error()
	if text != "" {
		text = strings.ToUpper(text[:1]) + text[1:]
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("[Errno %d] %s", int(errno), text)
	}
	return text
}
