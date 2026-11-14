package tool

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const maxSymlinks = 255

type PathEscapeError struct {
	Path   string
	Root   string
	Reason error
}

func (e *PathEscapeError) Error() string {
	if e.Reason != nil {
		return "Path escapes allowed directory: " + e.Reason.Error()
	}
	return fmt.Sprintf("Path escapes allowed directory: '%s' is not in the subpath of '%s'", e.Path, e.Root)
}

func (e *PathEscapeError) Unwrap() error {
	return e.Reason
}

func pathComponents(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}

func realpath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + string(filepath.Separator) + p
	}
	vol := filepath.VolumeName(p)
	resolved := vol + string(filepath.Separator)
	pending := pathComponents(p[len(vol):])
	links := 0
	for len(pending) > 0 {
		comp := pending[0]
		pending = pending[1:]
		switch comp {
		case ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, comp)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if links++; links > maxSymlinks {
			return "", fmt.Errorf("symlink loop resolving %s", p)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			tv := filepath.VolumeName(target)
			resolved = tv + string(filepath.Separator)
			pending = append(pathComponents(target[len(tv):]), pending...)
			continue
		}
		pending = append(pathComponents(target), pending...)
	}
	return resolved, nil
}

func WithinDir(path, root string) error {
	p, err := realpath(path)
	if err != nil {
		return &PathEscapeError{Path: path, Root: root, Reason: err}
	}
	r, err := realpath(root)
	if err != nil {
		return &PathEscapeError{Path: path, Root: root, Reason: err}
	}
	rel, err := filepath.Rel(r, p)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &PathEscapeError{Path: p, Root: r}
	}
	return nil
}

func HasTraversal(path string) bool {
	return slices.Contains(pathComponents(path), "..")
}

func HasUnsafePathChars(path string) bool {
	return strings.ContainsFunc(path, func(r rune) bool {
		return r <= 0x1f || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029
	})
}
