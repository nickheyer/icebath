package fileutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Error for a symlink or junction where a real file or directory is required
var ErrSymlink = errors.New("path is a symlink or junction")

// Error for a copy whose source and destination are one file
var ErrSameFile = errors.New("source and destination are the same file")

// Error a copy reports for a source that is a pipe, socket or device instead of a regular file
var errNotRegular = errors.New("not a regular file")

// Copies the regular file src to dst with its permission bits, timestamps and platform metadata
func CopyFile(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return &fs.PathError{Op: "copy", Path: src, Err: errNotRegular}
	}
	if dfi, err := os.Stat(dst); err == nil && os.SameFile(fi, dfi) {
		return &fs.PathError{Op: "copy", Path: dst, Err: ErrSameFile}
	}
	return copyFile(src, dst)
}

// Streams the bytes of src into dst, creating or truncating dst, a named pipe at dst refused
func copyData(src, dst string) error {
	if fi, err := os.Stat(dst); err == nil && fi.Mode()&fs.ModeNamedPipe != 0 {
		return &fs.PathError{Op: "copy", Path: dst, Err: errNotRegular}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return closeWith(err, in)
	}
	if _, err := io.Copy(out, in); err != nil {
		return joinErr(err, out.Close(), in.Close())
	}
	return errors.Join(out.Close(), in.Close())
}

// One entry a tree copy could not copy, with its cause
type CopyFailure struct {
	Src string
	Dst string
	Err error
}

// Every entry a tree copy could not copy while the rest of the tree was copied
type CopyTreeError struct {
	Failures []CopyFailure
}

// Each failed source and destination with its cause
func (e *CopyTreeError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = fmt.Sprintf("%s -> %s: %v", f.Src, f.Dst, f.Err)
	}
	return "copy tree failed for " + strings.Join(parts, "; ")
}

// Every failure cause, for errors.Is and errors.As
func (e *CopyTreeError) Unwrap() []error {
	errs := make([]error, len(e.Failures))
	for i, f := range e.Failures {
		errs[i] = f.Err
	}
	return errs
}

// Tuning for CopyTree, Ignore naming entries of dir to leave out or failing to abort the copy
type CopyTreeOptions struct {
	Symlinks  bool
	MergeInto bool
	Ignore    func(dir string, names []string) ([]string, error)
}

// Error an Ignore callback returned, carried out of the recursion unchanged
type ignoreAbort struct{ err error }

// Message of the Ignore error
func (a *ignoreAbort) Error() string { return a.err.Error() }

// Copies the tree at src to dst with CopyFile, junctions copied as directories, failures gathered in a CopyTreeError
func CopyTree(src, dst string, o CopyTreeOptions) error {
	err := copyTree(src, dst, o)
	var abort *ignoreAbort
	if errors.As(err, &abort) {
		return abort.err
	}
	return err
}

// One directory level of CopyTree, its own metadata copied after its entries
func copyTree(src, dst string, o CopyTreeOptions) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	var skip []string
	if o.Ignore != nil {
		if skip, err = o.Ignore(src, names); err != nil {
			return &ignoreAbort{err: err}
		}
	}
	if o.MergeInto {
		err = os.MkdirAll(dst, 0o777)
	} else if err = os.MkdirAll(filepath.Dir(dst), 0o777); err == nil {
		err = os.Mkdir(dst, 0o777)
	}
	if err != nil {
		return err
	}
	var failures []CopyFailure
	for _, name := range names {
		if slices.Contains(skip, name) {
			continue
		}
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		err := copyTreeEntry(from, to, o)
		var abort *ignoreAbort
		var nested *CopyTreeError
		switch {
		case errors.As(err, &abort):
			return abort
		case errors.As(err, &nested):
			failures = append(failures, nested.Failures...)
		case err != nil:
			failures = append(failures, CopyFailure{Src: from, Dst: to, Err: err})
		}
	}
	if err := copyMeta(src, dst, true); err != nil && dirMetaReported {
		failures = append(failures, CopyFailure{Src: src, Dst: dst, Err: err})
	}
	if len(failures) > 0 {
		return &CopyTreeError{Failures: failures}
	}
	return nil
}

// Copies one entry of a tree, a symlink recreated or followed and a directory or junction recursed into
func copyTreeEntry(from, to string, o CopyTreeOptions) error {
	fi, err := os.Lstat(from)
	if err != nil {
		return err
	}
	junction := isMountPoint(from)
	if fi.Mode()&fs.ModeSymlink != 0 && !junction {
		if o.Symlinks {
			target, err := os.Readlink(from)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, to); err != nil {
				return err
			}
			return copyMeta(from, to, false)
		}
		if st, err := os.Stat(from); err == nil && st.IsDir() {
			return copyTree(from, to, o)
		}
		return CopyFile(from, to)
	}
	if fi.IsDir() || junction {
		return copyTree(from, to, o)
	}
	return CopyFile(from, to)
}

// Moves src to exactly dst, renaming within a filesystem and copying then deleting src across filesystems
func Move(src, dst string) error {
	renameErr := os.Rename(src, dst)
	if renameErr == nil {
		return nil
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return renameErr
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		return os.Remove(src)
	case fi.IsDir() || isMountPoint(src):
		realSrc, err := resolveLax(src)
		if err != nil {
			return err
		}
		realDst, err := resolveLax(dst)
		if err != nil {
			return err
		}
		if within(realDst, realSrc) {
			return fmt.Errorf("Cannot move a directory '%s' into itself '%s'.", src, dst)
		}
		refused, err := moveRefused(src)
		if err != nil {
			return err
		}
		if refused {
			return &moveRefusedError{dir: src}
		}
		if err := CopyTree(src, dst, CopyTreeOptions{Symlinks: true}); err != nil {
			return err
		}
		return removeDir(src, false)
	}
	if err := CopyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// Directory move refused up front because the tree is immutable or, on macOS, not writable while holding entries
type moveRefusedError struct {
	dir string
}

// Reason the move of the directory was refused
func (e *moveRefusedError) Error() string {
	return fmt.Sprintf("Cannot move the non-empty directory '%s': Lacking write permission to '%s'.", e.dir, e.dir)
}

// Matches fs.ErrPermission
func (e *moveRefusedError) Unwrap() error { return fs.ErrPermission }

// Renames src to dst, Windows refusing an existing dst, retrying a permission or sharing refusal after each delay
func RenameRetrying(ctx context.Context, src, dst string, onRetry func(attempt int, delay time.Duration, err error), delays ...time.Duration) error {
	for i, d := range delays {
		err := renameNoReplace(src, dst)
		if err == nil || !(errors.Is(err, fs.ErrPermission) || sharingViolation(err)) {
			return err
		}
		if onRetry != nil {
			onRetry(i+1, d, err)
		}
		if err := sleepContext(ctx, d); err != nil {
			return err
		}
	}
	return renameNoReplace(src, dst)
}

// Waits d unless ctx ends first
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Deletes the tree at path removing all it can past a failure, force first lifting read-only, permission and immutable blocks
func RemoveTree(path string, force bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || isMountPoint(path) {
		return &fs.PathError{Op: "removetree", Path: path, Err: ErrSymlink}
	}
	if !fi.IsDir() {
		return &fs.PathError{Op: "removetree", Path: path, Err: syscall.ENOTDIR}
	}
	return removeDir(path, force)
}

// Removes every entry of dir depth first and then dir itself, never descending through a link, force lifting blocks
func removeDir(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil && force && errors.Is(err, fs.ErrPermission) {
		unblock(dir)
		entries, err = os.ReadDir(dir)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if realDir(e) {
			errs = append(errs, removeDir(p, force))
			continue
		}
		errs = append(errs, removeEntry(p, force))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return removeEntry(dir, force)
}

// Removes one entry, under force retrying once after unblocking it and its parent when permission is refused
func removeEntry(p string, force bool) error {
	err := os.Remove(p)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !force || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	unblock(filepath.Dir(p))
	unblock(p)
	err = os.Remove(p)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Copies the regular files under src into dst skipping links, specials and unreadable directories, a non-directory src copying nothing
func CopyRegularFiles(src, dst string) (int, error) {
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return 0, nil
	}
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return 0, nil
	}
	copied := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		if err := copyData(p, target); err != nil {
			return err
		}
		copied++
		return nil
	})
	return copied, err
}

// Deletes the regular files in dir matching pattern whose mtime is more than age before now and counts them
func RemoveOlderThan(dir, pattern string, age time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-age)
	removed := 0
	var errs []error
	for _, e := range entries {
		ok, err := matchName(pattern, e.Name())
		if err != nil {
			return removed, err
		}
		if !ok {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if !fi.Mode().IsRegular() || !fi.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
