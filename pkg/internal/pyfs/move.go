package pyfs

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

// shutil.Error raised for a move whose destination exists or lies inside the source
type MoveError struct{ Msg string }

func (e *MoveError) Error() string { return e.Msg }

// PermissionError raised for a tree that is immutable or that macOS will not let the caller empty
type MovePermissionError struct{ Msg string }

func (e *MovePermissionError) Error() string { return e.Msg }

// Lets errors.Is match the permission class
func (e *MovePermissionError) Unwrap() error { return os.ErrPermission }

// Python class name of the error
func (e *MovePermissionError) PyTypeName() string { return "PermissionError" }

// Moves a file or tree like shutil.move with copy2, the result empty for the same file rename
func Move(src, dst string) (string, error) {
	realDst := dst
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		if sameFile(src, dst) && !isLink(src) {
			return "", osRename(src, dst)
		}
		realDst = pypath.Join(dst, moveBasename(src))
		if _, err := os.Stat(realDst); err == nil {
			return "", &MoveError{fmt.Sprintf("Destination path '%s' already exists", realDst)}
		}
	}
	if err := osRename(src, realDst); err == nil {
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
		if err := unlinkOnly(src); err != nil {
			return "", err
		}
		return realDst, nil
	}
	if st, err := os.Stat(src); err == nil && st.IsDir() {
		inside, err := destInSrc(src, dst)
		if err != nil {
			return "", err
		}
		if inside {
			return "", &MoveError{fmt.Sprintf("Cannot move a directory '%s' into itself '%s'.", src, dst)}
		}
		refused, err := moveRefused(src)
		if err != nil {
			return "", err
		}
		if refused {
			return "", &MovePermissionError{fmt.Sprintf("Cannot move the non-empty directory '%s': Lacking write permission to '%s'.", src, src)}
		}
		if err := CopyTree(src, realDst, true, false, nil); err != nil {
			return "", err
		}
		if err := pyshutil.Rmtree(src); err != nil {
			return "", err
		}
		return realDst, nil
	}
	if err := pyshutil.Copy2(src, realDst); err != nil {
		return "", err
	}
	if err := unlinkOnly(src); err != nil {
		return "", err
	}
	return realDst, nil
}

// Last component of a path after its trailing separators are dropped
func moveBasename(p string) string {
	seps := string(os.PathSeparator)
	if runtime.GOOS == "windows" {
		seps = `\/`
	}
	return pypath.Basename(strings.TrimRight(p, seps))
}

// Whether the tree is immutable, or on macOS holds entries the caller may not remove
func moveRefused(src string) (bool, error) {
	immutable, err := isImmutable(src)
	if err != nil || immutable {
		return immutable, err
	}
	if Access(src, 2) {
		return false, nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return false, err
	}
	return len(entries) > 0 && runtime.GOOS == "darwin", nil
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

// Reports whether dst lies inside src once both are resolved through symlinks
func destInSrc(src, dst string) (bool, error) {
	s, err := pypath.Realpath(src, pypath.Lax)
	if err != nil {
		return false, err
	}
	d, err := pypath.Realpath(dst, pypath.Lax)
	if err != nil {
		return false, err
	}
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(s, sep) {
		s += sep
	}
	if !strings.HasSuffix(d, sep) {
		d += sep
	}
	return strings.HasPrefix(d, s), nil
}
