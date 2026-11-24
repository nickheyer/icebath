package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
)

// Names in one directory listing that a tree copy skips
type IgnoreFunc func(dir string, names []string) map[string]bool

// IgnoreFunc skipping the names that match any of the glob patterns, case folded on Windows
func IgnorePatterns(patterns ...string) IgnoreFunc {
	return func(_ string, names []string) map[string]bool {
		skip := make(map[string]bool)
		for _, pattern := range patterns {
			for _, name := range fnmatch.Filter(names, pattern) {
				skip[name] = true
			}
		}
		return skip
	}
}

// Settings of CopyTree: Symlinks recreates symbolic links as links rather than copying what they point at; DirsExistOK merges into directories that already exist; Ignore picks the names to skip in each directory; Check vets each directory listing before anything in it is copied, its error stopping the whole copy and coming back unchanged; CopyFile copies each regular file, a copy keeping content and metadata when nil
type CopyTreeOptions struct {
	Symlinks    bool
	DirsExistOK bool
	Ignore      IgnoreFunc
	Check       func(dir string, names []string) error
	CopyFile    func(src, dst string) error
}

// One entry a tree copy could not copy and why
type CopyFailure struct {
	Src, Dst string
	Err      error
}

// The entry and the reason, as in copy /a/x to /b/x: open /a/x: permission denied
func (f *CopyFailure) Error() string { return "copy " + f.Src + " to " + f.Dst + ": " + f.Err.Error() }

// The reason
func (f *CopyFailure) Unwrap() error { return f.Err }

// Every entry a tree copy went on past, in the order it met them
type CopyTreeError struct{ Failures []CopyFailure }

// Each failure, joined by semicolons
func (e *CopyTreeError) Error() string {
	var b strings.Builder
	for i := range e.Failures {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(e.Failures[i].Error())
	}
	return b.String()
}

// Each failure as an error of its own, so errors.Is and errors.As look into every one
func (e *CopyTreeError) Unwrap() []error {
	errs := make([]error, len(e.Failures))
	for i := range e.Failures {
		errs[i] = &e.Failures[i]
	}
	return errs
}

// Copies the directory tree at src to dst, making dst and its parents: each file with its content, permission bits, times and on Linux extended attributes, then each directory's own; a Windows junction is descended into; an entry that fails is skipped and gathered into a *CopyTreeError, while an unreadable src, a dst that exists without DirsExistOK and a Check error stop the copy at once
func CopyTree(src, dst string, o CopyTreeOptions) error {
	err := copyTree(src, dst, &o)
	if a, ok := err.(*checkAbort); ok {
		return a.err
	}
	return err
}

// Check error on its way out of the recursion, kept apart from the failures gathered on the way
type checkAbort struct{ err error }

func (a *checkAbort) Error() string { return a.err.Error() }

func copyTree(src, dst string, o *CopyTreeOptions) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if o.Check != nil {
		if err := o.Check(src, names); err != nil {
			return &checkAbort{err}
		}
	}
	var ignored map[string]bool
	if o.Ignore != nil {
		ignored = o.Ignore(src, names)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o777); err != nil && !(o.DirsExistOK && isDir(dst)) {
		return err
	}
	var failures []CopyFailure
	for _, name := range names {
		if ignored[name] {
			continue
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		err := copyTreeEntry(from, to, o)
		if a, ok := err.(*checkAbort); ok {
			return a
		}
		if tree, ok := err.(*CopyTreeError); ok {
			failures = append(failures, tree.Failures...)
		} else if err != nil {
			failures = append(failures, CopyFailure{Src: from, Dst: to, Err: err})
		}
	}
	if err := CopyMetadata(src, dst); err != nil {
		failures = append(failures, CopyFailure{Src: src, Dst: dst, Err: err})
	}
	if len(failures) > 0 {
		return &CopyTreeError{Failures: failures}
	}
	return nil
}

func copyTreeEntry(from, to string, o *CopyTreeOptions) error {
	st, err := os.Lstat(from)
	if err != nil {
		return err
	}
	link := st.Mode()&fs.ModeSymlink != 0
	switch {
	case st.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 && isMountPoint(from):
		return copyTree(from, to, o)
	case link && o.Symlinks:
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, to); err != nil {
			return err
		}
		return CopyLinkMetadata(from, to)
	case link:
		resolved, err := os.Stat(from)
		if err != nil {
			return err
		}
		if resolved.IsDir() {
			return copyTree(from, to, o)
		}
		return o.copyRegular(from, to, resolved)
	case st.IsDir():
		return copyTree(from, to, o)
	}
	return o.copyRegular(from, to, st)
}

// Copies one non-directory entry, a fifo, socket or device recorded as a failure by the content copy since reading one would block or never end
func (o *CopyTreeOptions) copyRegular(from, to string, fi fs.FileInfo) error {
	if o.CopyFile != nil {
		return o.CopyFile(from, to)
	}
	return copyFileFrom(from, to, fi)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Copies the regular files below src into the same places below dst, content only, skipping symlinks and every other kind of entry; gives back how many it copied, zero without error when src is no directory
func CopyRegularFiles(src, dst string) (int, error) {
	if !isDir(src) {
		return 0, nil
	}
	var files []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != src && d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	slices.Sort(files)
	copied := 0
	for _, p := range files {
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return copied, err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return copied, err
		}
		if err := copyContent(p, target); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}
