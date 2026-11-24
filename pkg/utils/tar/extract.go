package tarutil

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Typeflag of a regular file in archives older than POSIX ustar
const typeOldRegular = '\x00'

// Settings of Extract: Links extracts symbolic and hard links that resolve inside the destination, refusing them with ErrUnsupportedType when false
type ExtractOptions struct {
	Links bool
}

// Extracts a tar stream below dest, made when missing: directories, regular files with owner read and write added and no setuid, setgid or write bits for others, owner execute deciding execute for all, and links when allowed; every name must pass MemberParts and resolve inside dest, a link's target too, judged where the system will follow it, a refused member stopping the extraction with a *MemberError; content that would not fit in the space left on dest's volume is refused with ErrTooLarge before it is written; modification times are restored, directories' last
func Extract(r io.Reader, dest string, o ExtractOptions) error {
	return extract(tar.NewReader(r), dest, o)
}

// Extracts the gzip compressed tar file at archive below dest like Extract with links refused, so only directories and regular files come out
func ExtractTarGz(archive, dest string) error {
	return withTarGz(archive, func(tr *tar.Reader) error { return extract(tr, dest, ExtractOptions{}) })
}

// Top level directory names of the gzip compressed tar file at archive, a name counting through a directory member or any member below it; an unsafe member name fails it with a *MemberError
func RootDirs(archive string) (map[string]bool, error) {
	roots := map[string]bool{}
	err := withTarGz(archive, func(tr *tar.Reader) error {
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if skipMember(h) {
				continue
			}
			parts, err := MemberParts(h.Name)
			if err != nil {
				return err
			}
			if len(parts) > 1 || h.Typeflag == tar.TypeDir {
				roots[parts[0]] = true
			}
		}
	})
	return roots, err
}

// Names of the members of a tar stream in stream order, a directory's trailing slash dropped
func MemberNames(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return names, err
		}
		names = append(names, strings.TrimSuffix(h.Name, "/"))
	}
}

func withTarGz(archive string, read func(*tar.Reader) error) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return read(tar.NewReader(gz))
}

// Whether a member carries nothing to extract: a global header, or a directory entry for the archive root
func skipMember(h *tar.Header) bool {
	return h.Typeflag == tar.TypeXGlobalHeader || (h.Typeflag == tar.TypeDir && isRootName(h.Name))
}

type dirTime struct {
	path  string
	mtime time.Time
}

// Symbolic link an extraction made and the member it came from
type madeLink struct {
	path, name string
}

// State of one extraction below the resolved destination root
type extraction struct {
	root  string
	links bool
	// Bytes of content still allowed, negative when the volume reports no limit
	room int64
	dirs []dirTime
	made []madeLink
}

func extract(tr *tar.Reader, dest string, o ExtractOptions) error {
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	root, err := pathutil.Resolve(dest)
	if err != nil {
		return err
	}
	x := &extraction{root: root, links: o.Links, room: roomOn(root)}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if skipMember(h) {
			continue
		}
		if err := x.member(tr, h); err != nil {
			return err
		}
	}
	if err := x.checkLinks(); err != nil {
		return err
	}
	slices.SortFunc(x.dirs, func(a, b dirTime) int { return strings.Compare(b.path, a.path) })
	for _, d := range x.dirs {
		// A later member may have replaced an emptied directory with a link or a file, whose times are not the directory's to set
		if st, err := os.Lstat(d.path); err != nil || !st.IsDir() {
			continue
		}
		if err := os.Chtimes(d.path, d.mtime, d.mtime); err != nil {
			return err
		}
	}
	return nil
}

// Bytes of content an extraction below root may write: what an unprivileged user may still store on its volume, so a decompression bomb or a sparse member claiming a huge size is refused before the volume fills; unlimited when the volume reports nothing
func roomOn(root string) int64 {
	total, _, free, err := fsutil.DiskUsage(root)
	if err != nil || total == 0 {
		return -1
	}
	return int64(min(free, math.MaxInt64))
}

// Writes one member below the root, a directory's time recorded to be set once its entries are in
func (x *extraction) member(tr *tar.Reader, h *tar.Header) error {
	parts, err := MemberParts(h.Name)
	if err != nil {
		return err
	}
	target := filepath.Join(x.root, filepath.Join(parts...))
	if !within(target, x.root) {
		return &MemberError{Name: h.Name, Err: ErrOutsideDest}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, 0o777); err != nil {
			return err
		}
		x.dirs = append(x.dirs, dirTime{target, h.ModTime})
		return nil
	case tar.TypeReg, typeOldRegular, tar.TypeCont, tar.TypeGNUSparse:
		if x.room >= 0 {
			if h.Size > x.room {
				return &MemberError{Name: h.Name, Err: ErrTooLarge}
			}
			x.room -= h.Size
		}
		return writeFile(tr, h, target)
	case tar.TypeSymlink:
		if !x.links {
			return &MemberError{Name: h.Name, Err: ErrUnsupportedType}
		}
		if isAbsLink(h.Linkname) || !linkWithin(filepath.Dir(target), h.Linkname, x.root) {
			return &MemberError{Name: h.Name, Err: ErrOutsideDest}
		}
		if err := clearTarget(target); err != nil {
			return err
		}
		if err := os.Symlink(h.Linkname, target); err != nil {
			return err
		}
		x.made = append(x.made, madeLink{target, h.Name})
		return nil
	case tar.TypeLink:
		if !x.links {
			return &MemberError{Name: h.Name, Err: ErrUnsupportedType}
		}
		linkParts, err := MemberParts(h.Linkname)
		if err != nil {
			return &MemberError{Name: h.Name, Err: ErrOutsideDest}
		}
		// Linked through every symlink resolved: Linux links a symlink source itself, and the copy of a relative link at another place could lead outside, where the chmod below would then land
		source, err := pathutil.Resolve(filepath.Join(x.root, filepath.Join(linkParts...)))
		if err != nil {
			return err
		}
		if _, ok := pathutil.RelativeTo(source, x.root); !ok {
			return &MemberError{Name: h.Name, Err: ErrOutsideDest}
		}
		// The source is looked at before anything is removed, so a link to a missing member or onto the entry it already is never deletes what stands at target
		si, err := os.Lstat(source)
		if err != nil {
			return err
		}
		// A symlink left at the end of a loop, a directory or a special file is never given a second name
		if !si.Mode().IsRegular() {
			return &MemberError{Name: h.Name, Err: ErrUnsupportedType}
		}
		if ti, err := os.Lstat(target); err != nil || !os.SameFile(si, ti) {
			if err := clearTarget(target); err != nil {
				return err
			}
			if err := os.Link(source, target); err != nil {
				return err
			}
		}
		if err := os.Chmod(target, safeFileMode(h.Mode)); err != nil {
			return err
		}
		return os.Chtimes(target, h.ModTime, h.ModTime)
	}
	return &MemberError{Name: h.Name, Err: ErrUnsupportedType}
}

// Writes a regular member's content to target, created owner only and given its final permission bits before any byte lands so the content is never more exposed than the archive allows, then its modification time
func writeFile(tr *tar.Reader, h *tar.Header, target string) error {
	if err := makeParent(target); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = out.Chmod(safeFileMode(h.Mode))
	if err == nil {
		_, err = io.Copy(out, tr)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chtimes(target, h.ModTime, h.ModTime)
}

// Refuses the extraction when a symbolic link it made leads outside the root once every member is in, which a later member can bring about by replacing an emptied directory the link's target passes through with a link of its own; every such link is removed first
func (x *extraction) checkLinks() error {
	var first error
	for _, l := range x.made {
		text, err := os.Readlink(l.path)
		if err != nil {
			continue
		}
		if !linkWithin(filepath.Dir(l.path), text, x.root) {
			os.Remove(l.path)
			if first == nil {
				first = &MemberError{Name: l.name, Err: ErrOutsideDest}
			}
		}
	}
	return first
}

// Whether p, its symbolic links resolved as far as it exists, is root or lies below it
func within(p, root string) bool {
	resolved, err := pathutil.Resolve(p)
	if err != nil {
		return false
	}
	_, ok := pathutil.RelativeTo(resolved, root)
	return ok
}

// Whether a symbolic link in dir holding the text link leads to root or below it, followed the way the system follows it: on POSIX each dot-dot applies to what the links before it resolve to, on Windows the text is joined onto the resolved dir
func linkWithin(dir, link, root string) bool {
	resolved, err := pathutil.ResolveLink(dir, link)
	if err != nil {
		return false
	}
	_, ok := pathutil.RelativeTo(resolved, root)
	return ok
}

// Whether a link target is absolute or on a drive or share on any host
func isAbsLink(link string) bool {
	return filepath.IsAbs(link) || strings.HasPrefix(link, "/") || strings.HasPrefix(link, `\`) || hasDrive(link)
}

func makeParent(target string) error { return os.MkdirAll(filepath.Dir(target), 0o777) }

// Removes whatever an earlier member or an older extraction left where a link goes, after making its parent
func clearTarget(target string) error {
	if err := makeParent(target); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return os.Remove(target)
	}
	return nil
}

// Permission bits an extracted file gets: no setuid, setgid or sticky, no write for group and others, execute only where the owner had it, owner read and write always
func safeFileMode(mode int64) fs.FileMode {
	m := fs.FileMode(mode) & 0o755
	if m&0o100 == 0 {
		m &^= 0o111
	}
	return m | 0o600
}
