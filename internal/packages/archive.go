package packages

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"golang.org/x/text/encoding/charmap"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Desktop metadata that never decides a package's layout or digest
var junk = map[string]bool{".DS_Store": true, "Thumbs.db": true, "desktop.ini": true, ".localized": true}

// Folders a lone top folder of an archive is kept as, being the layout itself
var layoutDirs = map[string]bool{"bin": true, "cmd": true, "lib": true, "libexec": true, "share": true, "etc": true, "usr": true}

// Empties dest and extracts an archive into it by its file name: a tarball, a zip or a Debian package; nothing escapes dest
func Extract(archive, dest string) error {
	removeQuiet(dest)
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	name := strings.ToLower(filepath.Base(archive))
	switch {
	case strings.HasSuffix(name, ".zip"):
		return extractZip(archive, dest)
	case strings.HasSuffix(name, ".deb"):
		return extractDeb(archive, dest)
	}
	for _, suffix := range []string{".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz2", ".tar.zst", ".tzst", ".tar"} {
		if strings.HasSuffix(name, suffix) {
			f, err := os.Open(archive)
			if err != nil {
				return err
			}
			defer f.Close()
			r, err := decompressed(f, suffix)
			if err != nil {
				return fmt.Errorf("%s: %w", filepath.Base(archive), err)
			}
			return extractTar(r, dest)
		}
	}
	return fmt.Errorf("unsupported archive: %s", filepath.Base(archive))
}

// Stream of a compressed tarball by the suffix of its name
func decompressed(r io.Reader, suffix string) (io.Reader, error) {
	switch {
	case strings.HasSuffix(suffix, "gz"):
		return gzip.NewReader(r)
	case strings.HasSuffix(suffix, "xz"):
		return xz.NewReader(r)
	case strings.HasSuffix(suffix, "bz2"):
		return bzip2.NewReader(r), nil
	case strings.HasSuffix(suffix, "zst"):
		dec, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return dec.IOReadCloser(), nil
	case strings.HasSuffix(suffix, ".lzma"):
		return nil, errors.New("lzma compressed data is not supported")
	}
	return r, nil
}

// Member refused because it would land outside the destination or is no file, folder or link
type UnsafeMember struct {
	Member string
	Reason string
}

func (e *UnsafeMember) Error() string {
	return fmt.Sprintf("unsafe member %s: %s", strconv.Quote(e.Member), e.Reason)
}

// Extracts a tar stream under the one containment rule: names, link targets and the folders on the way stay inside dest
func extractTar(r io.Reader, dest string) error {
	root := resolved(dest)
	tr := tar.NewReader(r)
	type dirTime struct {
		path string
		time time.Time
	}
	var dirs []dirTime
	var links []*tar.Header
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target, err := place(root, h.Name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o777); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{target, h.ModTime})
		case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
			if err := writeMember(target, tr, fileMode(fs.FileMode(h.Mode)), h.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			links = append(links, h)
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
		default:
			return &UnsafeMember{Member: h.Name, Reason: "is a special file"}
		}
	}
	for _, h := range links {
		if err := placeLink(root, h); err != nil {
			return err
		}
	}
	slices.SortFunc(dirs, func(a, b dirTime) int { return strings.Compare(b.path, a.path) })
	for _, d := range dirs {
		_ = os.Chtimes(d.path, d.time, d.time)
	}
	return nil
}

// Where a member lands under root, the folders on the way made; empty for the root itself; refusing a name or a linked folder leaving root
func place(root, name string) (string, error) {
	clean, err := relative(name)
	if err != nil || clean == "." {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return "", err
	}
	if !fsx.Within(resolved(parent), root) {
		return "", &UnsafeMember{Member: name, Reason: "would be extracted outside the destination through a link"}
	}
	if st, err := os.Lstat(target); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		if err := os.Remove(target); err != nil {
			return "", err
		}
	}
	return target, nil
}

// Slash separated member name relative to the destination, leading slashes dropped; refusing a drive or a name climbing out of it
func relative(name string) (string, error) {
	slashed := name
	if runtime.GOOS == "windows" {
		slashed = strings.ReplaceAll(name, `\`, "/")
	}
	slashed = strings.TrimLeft(slashed, "/")
	if filepath.VolumeName(slashed) != "" || filepath.IsAbs(slashed) {
		return "", &UnsafeMember{Member: name, Reason: "has an absolute path"}
	}
	clean := path.Clean(slashed)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", &UnsafeMember{Member: name, Reason: "would be extracted outside the destination"}
	}
	return clean, nil
}

// Permission bits a member keeps: readable and writable by its owner, runnable only when its owner could run it
func fileMode(mode fs.FileMode) fs.FileMode {
	mode &= 0o755
	if mode&0o100 == 0 {
		mode &^= 0o111
	}
	return mode | 0o600
}

func writeMember(target string, r io.Reader, mode fs.FileMode, modified time.Time) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(target, mode); err != nil {
		return err
	}
	if !modified.IsZero() {
		_ = os.Chtimes(target, modified, modified)
	}
	return nil
}

// Places a symlink or hard link whose target stays inside root, a copy of the target where links can't be made
func placeLink(root string, h *tar.Header) error {
	target, err := place(root, h.Name)
	if err != nil || target == "" {
		return err
	}
	if filepath.IsAbs(h.Linkname) || strings.HasPrefix(h.Linkname, "/") || filepath.VolumeName(h.Linkname) != "" {
		return &UnsafeMember{Member: h.Name, Reason: "is a link to an absolute path"}
	}
	var source string
	if h.Typeflag == tar.TypeLink {
		clean, err := relative(h.Linkname)
		if err != nil {
			return &UnsafeMember{Member: h.Name, Reason: "would link outside the destination"}
		}
		source = filepath.Join(root, filepath.FromSlash(clean))
	} else {
		source = filepath.Join(filepath.Dir(target), filepath.FromSlash(h.Linkname))
	}
	if !fsx.Within(resolved(source), root) {
		return &UnsafeMember{Member: h.Name, Reason: "would link outside the destination"}
	}
	os.Remove(target)
	if h.Typeflag == tar.TypeLink {
		if err := os.Link(source, target); err == nil {
			return nil
		}
		return fsx.Copy(source, target)
	}
	if err := os.Symlink(filepath.FromSlash(h.Linkname), target); err == nil {
		return nil
	}
	if st, err := os.Stat(source); err == nil && st.Mode().IsRegular() {
		return fsx.Copy(source, target)
	}
	return &UnsafeMember{Member: h.Name, Reason: "is a link this system can't make and whose target is no file to copy"}
}

// Extracts a zip, keeping executable bits and placing symlinks that stay inside dest last
func extractZip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	root := resolved(dest)
	type link struct{ member, target string }
	var links []link
	for _, f := range zr.File {
		name := f.Name
		if f.NonUTF8 {
			if decoded, err := charmap.CodePage437.NewDecoder().String(name); err == nil {
				name = decoded
			}
		}
		if i := strings.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		mode := fs.FileMode(f.ExternalAttrs >> 16)
		if mode&0o170000 == 0o120000 {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			links = append(links, link{name, string(data)})
			continue
		}
		name = zipName(name)
		if name == "" {
			continue
		}
		target, err := place(root, name)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		if strings.HasSuffix(f.Name, "/") {
			if err := os.MkdirAll(target, 0o777); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		perm := fs.FileMode(0o644)
		if mode&0o111 != 0 {
			perm = fileMode(mode)
		}
		err = writeMember(target, rc, perm, f.Modified)
		rc.Close()
		if err != nil {
			return err
		}
	}
	for _, l := range links {
		name := zipName(l.member)
		if name == "" || filepath.IsAbs(l.target) || strings.HasPrefix(l.target, "/") {
			continue
		}
		target, err := place(root, name)
		if err != nil || target == "" {
			continue
		}
		if !fsx.Within(resolved(filepath.Join(filepath.Dir(target), filepath.FromSlash(l.target))), root) {
			continue
		}
		os.Remove(target)
		if err := os.Symlink(filepath.FromSlash(l.target), target); err != nil {
			if err := os.WriteFile(target, []byte(l.target), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Member name with the characters Windows refuses in a file name replaced and its empty and dot parts dropped
func zipName(name string) string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(name, `\`, "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		if runtime.GOOS == "windows" {
			part = strings.TrimRight(strings.Map(func(r rune) rune {
				if strings.ContainsRune(`:<>|"?*`, r) {
					return '_'
				}
				return r
			}, part), ". ")
			if part == "" {
				continue
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "/")
}

// Extracts the data of a Debian package by ar and tar, never running dpkg or the package's scripts, and leaves its tree readable and
// runnable by everyone
func extractDeb(archive, dest string) error {
	data, member, err := debData(archive)
	if err != nil {
		return err
	}
	suffix := strings.TrimPrefix(member, "data")
	r, err := decompressed(bytes.NewReader(data), suffix)
	if err != nil {
		return fmt.Errorf("unsupported data compression %s in %s", member, filepath.Base(archive))
	}
	if err := extractTar(r, dest); err != nil {
		return err
	}
	root := resolved(dest)
	return filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		st, err := d.Info()
		if err != nil || !fsx.Within(resolved(path), root) {
			return nil
		}
		mode := fs.FileMode(0o644)
		if st.Mode().Perm()&0o111 != 0 {
			mode |= 0o111
		}
		return os.Chmod(path, mode)
	})
}

// Data member of an ar archive and its name
func debData(archive string) ([]byte, string, error) {
	raw, err := os.ReadFile(archive)
	if err != nil {
		return nil, "", err
	}
	base := filepath.Base(archive)
	if len(raw) < 8 || string(raw[:8]) != "!<arch>\n" {
		return nil, "", fmt.Errorf("not an ar archive: %s", base)
	}
	for offset := 8; offset+60 <= len(raw); {
		header := raw[offset : offset+60]
		member := strings.TrimRight(string(header[0:16]), " /")
		size, err := strconv.Atoi(strings.TrimSpace(string(header[48:58])))
		if err != nil || size < 0 {
			return nil, "", fmt.Errorf("bad ar member size in %s", base)
		}
		start := offset + 60
		end := min(start+size, len(raw))
		if strings.HasPrefix(member, "data.tar") {
			if strings.HasSuffix(member, ".lzma") {
				return nil, "", fmt.Errorf("unsupported data compression %s in %s", member, base)
			}
			return raw[start:end], member, nil
		}
		offset = start + size + size%2
	}
	return nil, "", fmt.Errorf("no data.tar member in %s", base)
}

// Inert desktop metadata: a junk name or an AppleDouble file that is no program
func metadata(path string) bool {
	name := filepath.Base(path)
	if !junk[name] && !strings.HasPrefix(name, "._") {
		return false
	}
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 == 0
}

// Hoists the contents of a lone top folder unless the folder is the layout itself, leaving everything when a name would collide
func Flatten(dest string) error {
	all, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	var entries []string
	for _, e := range all {
		if path := filepath.Join(dest, e.Name()); !metadata(path) {
			entries = append(entries, path)
		}
	}
	if len(entries) != 1 || layoutDirs[filepath.Base(entries[0])] || !fsx.IsDir(entries[0]) {
		return nil
	}
	inner := entries[0]
	items, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	for _, item := range items {
		target := filepath.Join(dest, item.Name())
		if fsx.Lexists(target) && !(metadata(filepath.Join(inner, item.Name())) && metadata(target)) {
			return nil
		}
	}
	for _, item := range items {
		target := filepath.Join(dest, item.Name())
		if fsx.Lexists(target) {
			if err := fsx.Retry(func() error { return os.Remove(target) }); err != nil {
				return err
			}
		}
		if err := fsx.Retry(func() error { return os.Rename(filepath.Join(inner, item.Name()), target) }); err != nil {
			return err
		}
	}
	return fsx.Retry(func() error { return os.Remove(inner) })
}

// Moves the files of src into dst keeping both layouts, a file in both failing
func Merge(src, dst string) error {
	var files []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, file := range files {
		rel, err := filepath.Rel(src, file)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fsx.Lexists(target) {
			return fmt.Errorf("archives disagree about %s", rel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		if err := fsx.Retry(func() error { return os.Rename(file, target) }); err != nil {
			return err
		}
	}
	return nil
}
