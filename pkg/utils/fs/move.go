package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

var (
	errMoveIntoItself = errors.New("cannot move a directory into itself")
	errMoveDenied     = fmt.Errorf("directory is immutable or cannot be emptied: %w", fs.ErrPermission)
)

// Moves the file or tree at src to dst, or into dst when that is a directory, with a rename or, where that fails, a copy keeping links and metadata and then the removal of src; gives back the path moved to, empty when src and dst already name the same directory; an existing destination fails with fs.ErrExist
func Move(src, dst string) (string, error) {
	realDst := dst
	if isDir(dst) {
		if sameFile(src, dst) && !isSymlink(src) {
			return "", moveRename(src, dst)
		}
		realDst = filepath.Join(dst, moveBasename(src))
		if _, err := os.Stat(realDst); err == nil {
			return "", &os.LinkError{Op: "move", Old: src, New: realDst, Err: fs.ErrExist}
		}
	}
	if err := moveRename(src, realDst); err == nil {
		return realDst, nil
	}
	if isSymlink(src) {
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
	if isDir(src) {
		inside, err := destInSrc(src, dst)
		if err != nil {
			return "", err
		}
		if inside {
			return "", &os.LinkError{Op: "move", Old: src, New: dst, Err: errMoveIntoItself}
		}
		refused, err := moveRefused(src)
		if err != nil {
			return "", err
		}
		if refused {
			return "", &os.LinkError{Op: "move", Old: src, New: dst, Err: errMoveDenied}
		}
		if err := CopyTree(src, realDst, CopyTreeOptions{Symlinks: true}); err != nil {
			return "", err
		}
		if err := os.RemoveAll(src); err != nil {
			return "", err
		}
		return realDst, nil
	}
	if err := CopyFile(src, realDst); err != nil {
		return "", err
	}
	if err := unlinkOnly(src); err != nil {
		return "", err
	}
	return realDst, nil
}

// Last element of a path once its trailing separators and any volume name are dropped
func moveBasename(p string) string {
	seps := string(os.PathSeparator)
	if runtime.GOOS == "windows" {
		seps = `\/`
	}
	name := strings.TrimRight(p, seps)
	name = name[len(filepath.VolumeName(name)):]
	if i := strings.LastIndexAny(name, seps); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Whether the tree is immutable, or on macOS has entries while the caller may not write to it
func moveRefused(src string) (bool, error) {
	immutable, err := isImmutable(src)
	if err != nil || immutable {
		return immutable, err
	}
	if Access(src, AccessWrite) {
		return false, nil
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return false, err
	}
	return len(entries) > 0 && runtime.GOOS == "darwin", nil
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&fs.ModeSymlink != 0
}

// Whether dst is src or lies inside it once both are resolved through symlinks
func destInSrc(src, dst string) (bool, error) {
	s, err := pathutil.Resolve(src)
	if err != nil {
		return false, err
	}
	d, err := pathutil.Resolve(dst)
	if err != nil {
		return false, err
	}
	_, inside := pathutil.RelativeTo(d, s)
	return inside, nil
}
