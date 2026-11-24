package ziputil

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// Longest symlink target Extract makes a link of
	maxLinkTarget = 4096

	// Limits Extract applies unless told otherwise
	defaultMaxMembers = 1 << 20
	defaultMaxBytes   = 16 << 30
)

// Limit Extract checks the whole archive against before it writes anything
type ExtractOption func(*extractLimits)

type extractLimits struct {
	members int
	bytes   int64
}

// Most members Extract accepts, 1<<20 unless set, none at zero or below
func MaxMembers(n int) ExtractOption { return func(l *extractLimits) { l.members = n } }

// Most uncompressed bytes the file and link members may declare in total, which their decoding is held to, 16 GiB unless set, none at zero or below
func MaxBytes(n int64) ExtractOption { return func(l *extractLimits) { l.bytes = n } }

// Member with the host path it extracts to
type planned struct {
	f    *zip.File
	rel  string
	ok   bool
	mode fs.FileMode
}

// Writes every member under dir, creating it, after checking the archive against the limits (1<<20 members, 16 GiB declared, ErrLimit beyond): names lose roots, drive letters and empty, . and .. elements and are made legal on the host; each file is written beside its path and renamed over it once its data checked, so a failing member leaves nothing and an old file or link there is replaced, never written through; files get mode 0o666 plus the member's execute bits, never setuid or setgid, and their recorded modification time, directories 0o777 and theirs once everything is written; symlinks whose target stays inside dir become links after all files, other symlinks files holding the target; of members with the same path the later one wins; a failure stops at that member; nothing is ever written outside dir
func (r *Reader) Extract(dir string, opts ...ExtractOption) error {
	limits := extractLimits{members: defaultMaxMembers, bytes: defaultMaxBytes}
	for _, opt := range opts {
		opt(&limits)
	}
	if err := limits.check(r.File); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	plan := make([]planned, len(r.File))
	last := make(map[string]int, len(r.File))
	for i, f := range r.File {
		rel, ok := localPath(f.Name)
		plan[i] = planned{f, rel, ok, f.Mode()}
		if ok {
			last[rel] = i
		}
	}
	var links, dirs []int
	for i, p := range plan {
		switch {
		case p.mode&fs.ModeSymlink != 0 && !p.mode.IsDir():
			links = append(links, i)
			continue
		case p.mode.IsDir() && p.ok:
			dirs = append(dirs, i)
		}
		if err := r.extractMember(root, p); err != nil {
			return fmt.Errorf("extracting zip member %q: %w", p.f.Name, err)
		}
	}
	for _, i := range links {
		p := plan[i]
		if p.ok && last[p.rel] != i {
			continue
		}
		if err := r.extractLink(root, p); err != nil {
			return fmt.Errorf("extracting zip member %q: %w", p.f.Name, err)
		}
	}
	for _, i := range dirs {
		p := plan[i]
		if last[p.rel] != i {
			continue
		}
		if t := memberTime(p.f); !t.IsZero() {
			if err := root.Chtimes(p.rel, time.Time{}, t); err != nil {
				return fmt.Errorf("extracting zip member %q: %w", p.f.Name, err)
			}
		}
	}
	return nil
}

// ErrLimit when the archive holds more members, or its file and link members declare more uncompressed bytes, than the limits allow
func (l extractLimits) check(files []*zip.File) error {
	if l.members > 0 && len(files) > l.members {
		return fmt.Errorf("zip archive has %d members, more than the %d allowed: %w", len(files), l.members, ErrLimit)
	}
	if l.bytes <= 0 {
		return nil
	}
	left := uint64(l.bytes)
	for _, f := range files {
		if f.Mode().IsDir() {
			continue
		}
		if f.UncompressedSize64 > left {
			return fmt.Errorf("zip archive declares more than the %d uncompressed bytes allowed: %w", l.bytes, ErrLimit)
		}
		left -= f.UncompressedSize64
	}
	return nil
}

// Writes one directory or file member
func (r *Reader) extractMember(root *os.Root, p planned) error {
	if p.mode.IsDir() {
		if !p.ok {
			return nil
		}
		return root.MkdirAll(p.rel, 0o777)
	}
	if !p.ok {
		return errNoName
	}
	src, err := r.openFile(p.f)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := makeParent(root, p.rel); err != nil {
		return err
	}
	return replaceFile(root, p.rel, src, 0o666|p.mode&0o111, memberTime(p.f))
}

// Makes a symlink member a link when its target stays inside the root, and a file holding the target otherwise or when the host refuses the link
func (r *Reader) extractLink(root *os.Root, p planned) error {
	if !p.ok {
		return errNoName
	}
	src, err := r.openFile(p.f)
	if err != nil {
		return err
	}
	defer src.Close()
	target, err := io.ReadAll(io.LimitReader(src, maxLinkTarget+1))
	if err != nil {
		return err
	}
	if err := makeParent(root, p.rel); err != nil {
		return err
	}
	if len(target) <= maxLinkTarget && linkStaysInside(root, p.rel, string(target)) &&
		replaceLink(root, p.rel, filepath.FromSlash(string(target))) == nil {
		return nil
	}
	return replaceFile(root, p.rel, io.MultiReader(bytes.NewReader(target), src), 0o666, memberTime(p.f))
}

// Makes the parent directories of rel
func makeParent(root *os.Root, rel string) error {
	if parent := filepath.Dir(rel); parent != "." {
		return root.MkdirAll(parent, 0o777)
	}
	return nil
}

// Puts a file holding src, with perm and, when it is not zero, modification time mtime, at rel through a fresh name beside it, so that rel keeps what it held unless the whole file is written
func replaceFile(root *os.Root, rel string, src io.Reader, perm fs.FileMode, mtime time.Time) error {
	var out *os.File
	tmp, err := claimTemp(rel, func(tmp string) (err error) {
		out, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		return err
	})
	if err != nil {
		return err
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && !mtime.IsZero() {
		err = root.Chtimes(tmp, time.Time{}, mtime)
	}
	return commit(root, tmp, rel, err)
}

// Puts a symlink to target at rel through a fresh name beside it
func replaceLink(root *os.Root, rel, target string) error {
	tmp, err := claimTemp(rel, func(tmp string) error { return root.Symlink(target, tmp) })
	if err != nil {
		return err
	}
	return commit(root, tmp, rel, nil)
}

// Fresh name in rel's directory that create made an entry at, a new name tried while the one tried is taken
func claimTemp(rel string, create func(tmp string) error) (string, error) {
	dir := filepath.Dir(rel)
	var err error
	for range 64 {
		tmp := filepath.Join(dir, ".zip-"+strconv.FormatUint(rand.Uint64(), 36))
		if err = create(tmp); !errors.Is(err, fs.ErrExist) {
			return tmp, err
		}
	}
	return "", err
}

// Renames tmp over rel when err is nil, and removes tmp when err is not or the rename fails, returning the failure
func commit(root *os.Root, tmp, rel string, err error) error {
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}

// Modification time a member records, the zero time when it records none; MS-DOS fields without an extended timestamp are local time
func memberTime(f *zip.File) time.Time {
	t := f.Modified
	switch {
	case t.Location() != time.UTC:
		return t
	case f.ModifiedDate == 0 && f.ModifiedTime == 0:
		if t.Equal(time.Date(1980, 0, 0, 0, 0, 0, 0, time.UTC)) {
			return time.Time{}
		}
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
}

// Host path, relative to the extraction root, that a member name maps to; false when no element is left
func localPath(name string) (string, bool) {
	name = name[len(filepath.VolumeName(name)):]
	parts := make([]string, 0, strings.Count(name, "/")+1)
	for el := range strings.SplitSeq(name, "/") {
		if el == "" || el == "." || el == ".." {
			continue
		}
		if el = legalElement(el); el != "" {
			parts = append(parts, el)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return filepath.Join(parts...), true
}

// Whether a link at rel to target resolves inside the root: the target is relative, its .. elements all come first and climb no higher than the link's directory, its other elements are already legal names on the host, and no element of that directory's path is itself a link
func linkStaysInside(root *os.Root, rel, target string) bool {
	if target == "" || strings.IndexByte(target, 0) >= 0 || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
		return false
	}
	slashed := filepath.ToSlash(target)
	if strings.HasPrefix(slashed, "/") {
		return false
	}
	depth := 0
	if parent := filepath.Dir(rel); parent != "." {
		walked := ""
		for el := range strings.SplitSeq(filepath.ToSlash(parent), "/") {
			walked = filepath.Join(walked, el)
			if info, err := root.Lstat(walked); err != nil || !info.IsDir() {
				return false
			}
			depth++
		}
	}
	climbing := true
	for el := range strings.SplitSeq(slashed, "/") {
		switch el {
		case "", ".":
		case "..":
			depth--
			if !climbing || depth < 0 {
				return false
			}
		default:
			if legalElement(el) != el {
				return false
			}
			climbing = false
		}
	}
	return true
}
