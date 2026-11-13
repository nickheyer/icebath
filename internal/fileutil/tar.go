package fileutil

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// Size of one tar header block
const tarBlockSize = 512

// Bytes a GNU header holds for a user or group name, longer names cut to fit
const tarNameField = 32

// Errors for input that is not in the compression a tar reader was asked for
var (
	errNotTar   = errors.New("not a tar archive in a known compression format")
	errNotGzip  = errors.New("not a gzip file")
	errNotBzip2 = errors.New("not a bzip2 file")
)

// Compression a tar reader accepts
type TarCompression int

const (
	// Gzip, bzip2, xz, legacy lzma, zstd or none, detected from the first bytes
	AnyCompression TarCompression = iota
	// Gzip only, anything else refused
	GzipOnly
	// Bzip2 only, anything else refused
	Bzip2Only
)

// Tar stream inside the compression the reader was opened for
type TarReader struct {
	*tar.Reader
	closeFn func() error
}

// Reader over the tar stream inside r, its compression checked against comp from the first bytes
func NewTarReader(r io.Reader, comp TarCompression) (*TarReader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, err := br.Peek(tarBlockSize)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	isGzip := bytes.HasPrefix(head, []byte{0x1f, 0x8b})
	isBzip2 := bytes.HasPrefix(head, []byte("BZh"))
	switch {
	case comp == GzipOnly && !isGzip:
		return nil, errNotGzip
	case comp == Bzip2Only && !isBzip2:
		return nil, errNotBzip2
	case len(head) == 0:
		return nil, errNotTar
	case isGzip:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		return &TarReader{Reader: tar.NewReader(gz), closeFn: gz.Close}, nil
	case isBzip2:
		return &TarReader{Reader: tar.NewReader(bzip2.NewReader(br))}, nil
	case bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		xr, err := xz.NewReader(br)
		if err != nil {
			return nil, err
		}
		return &TarReader{Reader: tar.NewReader(xr)}, nil
	case bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		return &TarReader{Reader: tar.NewReader(zr), closeFn: func() error { zr.Close(); return nil }}, nil
	case plainTarHeader(head):
		return &TarReader{Reader: tar.NewReader(br)}, nil
	}
	lr, err := lzma.NewReader(br)
	if err != nil {
		return nil, errNotTar
	}
	return &TarReader{Reader: tar.NewReader(lr)}, nil
}

// Releases the decompressor, leaving the underlying reader open
func (t *TarReader) Close() error {
	if t.closeFn == nil {
		return nil
	}
	return t.closeFn()
}

// Reports whether block is an end of archive block or a tar header whose checksum holds
func plainTarHeader(block []byte) bool {
	if len(block) < tarBlockSize {
		return false
	}
	block = block[:tarBlockSize]
	if !slices.ContainsFunc(block, func(b byte) bool { return b != 0 }) {
		return true
	}
	field := strings.Trim(string(block[148:156]), " \x00")
	stored, err := strconv.ParseInt(field, 8, 64)
	if err != nil {
		return false
	}
	var unsigned, signed int64
	for i, b := range block {
		if i >= 148 && i < 156 {
			b = ' '
		}
		unsigned += int64(b)
		signed += int64(int8(b))
	}
	return stored == unsigned || stored == signed
}

// Next member header, skipping global extended headers and leaving path safety to the extraction policy
func nextMember(tr *TarReader) (*tar.Header, error) {
	for {
		h, err := tr.Next()
		if errors.Is(err, tar.ErrInsecurePath) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeXGlobalHeader {
			return h, nil
		}
	}
}

// Reports whether a member type carries regular file data
func regularMember(h *tar.Header) bool {
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeCont, tar.TypeGNUSparse:
		return true
	}
	return false
}

// Extracts the tar stream r in compression comp into dest under policy
func ExtractTar(ctx context.Context, r io.Reader, dest string, policy ExtractPolicy, comp TarCompression) error {
	tr, err := NewTarReader(r, comp)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return closeWith(err, tr)
	}
	if policy == FilesOnly {
		err = extractTarFiles(ctx, tr, dest)
	} else {
		err = extractTarContained(ctx, tr, dest)
	}
	return closeWith(err, tr)
}

// Extracts the tar archive at path in compression comp into dest under policy
func ExtractTarFile(ctx context.Context, path, dest string, policy ExtractPolicy, comp TarCompression) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return closeWith(ExtractTar(ctx, f, dest, policy, comp), f)
}

// Directories and regular files with their member permission bits where the filesystem keeps them, any other member refused
func extractTarFiles(ctx context.Context, tr *TarReader, dest string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := nextMember(tr)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		parts, err := MemberParts(h.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(append([]string{dest}, parts...)...)
		switch {
		case h.Typeflag == tar.TypeDir:
			if err := os.MkdirAll(target, 0o777); err != nil {
				return err
			}
		case regularMember(h):
			if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
				return err
			}
			if err := writeMember(target, tr); err != nil {
				return err
			}
			_ = os.Chmod(target, fs.FileMode(h.Mode&0o777))
		default:
			return &UnsafeMemberError{Member: h.Name, Reason: reasonUnsupported}
		}
	}
}

// Member the contained links policy admits, with where it lands, its final mode and what a link names
type containedMember struct {
	name     string
	key      string
	place    string
	mode     fs.FileMode
	link     string
	linkKey  string
	typeflag byte
	mtime    time.Time
	seq      int
}

// Members already extracted by cleaned name, every occurrence in archive order
type memberIndex map[string][]containedMember

// Last occurrence of key, among members before seq when seq is not negative
func (ix memberIndex) last(key string, before int) (containedMember, bool) {
	occ := ix[key]
	for i := len(occ) - 1; i >= 0; i-- {
		if before < 0 || occ[i].seq < before {
			return occ[i], true
		}
	}
	return containedMember{}, false
}

// Directory awaiting its modification time once everything inside it exists
type dirStamp struct {
	name  string
	path  string
	mtime time.Time
}

// Hops a link standing in for a refused one follows through member links before the chain counts as a loop
const maxMemberHops = 40

// Files, directories and contained links with group and other write stripped, refused links stood in for once all members exist
func extractTarContained(ctx context.Context, tr *TarReader, dest string) error {
	root, err := resolveLax(dest)
	if err != nil {
		return err
	}
	index := memberIndex{}
	var dirs []dirStamp
	var refused []containedMember
	for seq := 0; ; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := nextMember(tr)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		m, err := admitContained(h, root)
		if err != nil {
			return err
		}
		m.seq = seq
		if err := os.MkdirAll(filepath.Dir(m.place), 0o777); err != nil {
			return err
		}
		switch {
		case m.typeflag == tar.TypeDir:
			if err := os.Mkdir(m.place, 0o777); err != nil {
				if fi, statErr := os.Stat(m.place); statErr != nil || !fi.IsDir() {
					return err
				}
			}
			dirs = append(dirs, dirStamp{name: m.name, path: m.place, mtime: m.mtime})
		case m.typeflag == tar.TypeSymlink:
			if err := clearPlace(m.place, "symlink"); err != nil {
				return err
			}
			if os.Symlink(m.link, m.place) != nil {
				refused = append(refused, m)
			}
		case m.typeflag == tar.TypeLink:
			deferred, err := placeHardlink(m, root, index)
			if err != nil {
				return err
			}
			if deferred {
				refused = append(refused, m)
			}
		default:
			if err := writeMember(m.place, tr); err != nil {
				return err
			}
			stampMember(m.place, m.mode, m.mtime)
		}
		index[m.key] = append(index[m.key], m)
	}
	for _, m := range refused {
		if err := standInForLink(m, index); err != nil {
			return err
		}
	}
	sort.SliceStable(dirs, func(i, j int) bool { return dirs[i].name > dirs[j].name })
	for _, d := range dirs {
		_ = os.Chtimes(d.path, d.mtime, d.mtime)
	}
	return nil
}

// Hard links a member to the resolved file it names, true when the link waits for a stand-in
func placeHardlink(m containedMember, root string, index memberIndex) (bool, error) {
	if err := clearPlace(m.place, "link"); err != nil {
		return false, err
	}
	source, err := resolveLax(filepath.Join(root, m.link))
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(source); statErr != nil {
		if _, ok := index.last(m.linkKey, m.seq); !ok {
			return false, &UnsafeMemberError{Member: m.name, Reason: reasonLinkMissing}
		}
		return true, nil
	}
	if os.Link(source, m.place) == nil {
		stampMember(m.place, m.mode, m.mtime)
		return false, nil
	}
	return true, nil
}

// Copies what a refused link names to its place, an empty directory for a directory, nothing when absent
func standInForLink(m containedMember, index memberIndex) error {
	key, before := m.linkKey, -1
	if m.typeflag == tar.TypeLink {
		before = m.seq
	}
	for range maxMemberHops {
		t, ok := index.last(key, before)
		if !ok {
			return nil
		}
		switch t.typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(m.place, 0o777); err != nil {
				if fi, statErr := os.Stat(m.place); statErr != nil || !fi.IsDir() {
					return err
				}
			}
			return nil
		case tar.TypeSymlink:
			key, before = t.linkKey, -1
		case tar.TypeLink:
			key, before = t.linkKey, t.seq
		default:
			if fi, err := os.Lstat(t.place); err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			if err := copyData(t.place, m.place); err != nil {
				return err
			}
			stampMember(m.place, t.mode, t.mtime)
			return nil
		}
	}
	return &fs.PathError{Op: "link", Path: m.place, Err: syscall.ELOOP}
}

// Checks a member against the contained links policy and works out where and how it lands
func admitContained(h *tar.Header, root string) (containedMember, error) {
	sep := string(filepath.Separator)
	name := h.Name
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, sep) {
		name = strings.TrimLeft(name, "/"+sep)
	}
	native := filepath.FromSlash(name)
	if vol := filepath.VolumeName(native); vol != "" && !filepath.IsAbs(native) {
		if !samePathText(vol, filepath.VolumeName(root)) {
			return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonOutside, Target: native}
		}
		native = native[len(vol):]
	}
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonAbsolute}
	}
	if slices.ContainsFunc(strings.Split(native, sep), platformDotName) {
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonUnsafePath}
	}
	if h.Typeflag != tar.TypeDir && strings.HasSuffix(native, sep) {
		return containedMember{}, &fs.PathError{Op: "open", Path: root + sep + native, Err: syscall.EISDIR}
	}
	full, err := resolveLax(root + sep + native)
	if err != nil {
		return containedMember{}, err
	}
	if !within(full, root) {
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonOutside, Target: full}
	}
	place, err := placement(root, native)
	if err != nil {
		return containedMember{}, err
	}
	if !within(filepath.Dir(place), root) && !samePathText(place, root) {
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonOutside, Target: place}
	}
	m := containedMember{name: name, key: filepath.Clean(native), place: place, mode: fs.FileMode(h.Mode & 0o755),
		typeflag: h.Typeflag, mtime: h.ModTime}
	switch {
	case regularMember(h) || h.Typeflag == tar.TypeLink:
		if m.mode&0o100 == 0 {
			m.mode &^= 0o111
		}
		m.mode |= 0o600
	case h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeSymlink:
	default:
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonSpecial}
	}
	if h.Typeflag != tar.TypeLink && h.Typeflag != tar.TypeSymlink {
		return m, nil
	}
	linkNative := filepath.FromSlash(h.Linkname)
	if filepath.IsAbs(linkNative) || filepath.VolumeName(linkNative) != "" {
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonAbsoluteLink}
	}
	check := root + sep + linkNative
	m.linkKey = filepath.Clean(linkNative)
	if h.Typeflag == tar.TypeSymlink {
		check = root + sep + rawDir(native) + sep + linkNative
		m.linkKey = filepath.Clean(rawDir(native) + sep + linkNative)
	}
	resolved, err := resolveLax(check)
	if err != nil {
		return containedMember{}, err
	}
	if !within(resolved, root) {
		return containedMember{}, &UnsafeMemberError{Member: h.Name, Reason: reasonLinkOutside, Target: resolved}
	}
	m.link = filepath.Clean(linkNative)
	return m, nil
}

// Top level directory names of the gzip tar archive at path, counted through a nested member or a directory member
func TarRootDirs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr, err := NewTarReader(f, GzipOnly)
	if err != nil {
		return nil, err
	}
	defer tr.Close()
	roots := map[string]bool{}
	for {
		h, err := nextMember(tr)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		parts, err := MemberParts(h.Name)
		if err != nil {
			return nil, err
		}
		if len(parts) > 1 || h.Typeflag == tar.TypeDir {
			roots[parts[0]] = true
		}
	}
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Gzip compressed tar stream that stores links as links and hard links once
type TarWriter struct {
	gz     *gzip.Writer
	tw     *tar.Writer
	format tar.Format
	links  map[FileID]string
}

// Writer of a gzip tar stream into w at the gzip level with headers in format
func NewTarGzWriter(w io.Writer, level int, format tar.Format) (*TarWriter, error) {
	gz, err := gzip.NewWriterLevel(w, level)
	if err != nil {
		return nil, err
	}
	return &TarWriter{gz: gz, tw: tar.NewWriter(gz), format: format, links: map[FileID]string{}}, nil
}

// Adds path under name, directories recursed in name order, keep returning false leaving an entry and its children out
func (t *TarWriter) Add(path, name string, keep func(*tar.Header) bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket != 0 {
		return nil
	}
	link := ""
	if fi.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}
	h, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	h.Name = filepath.ToSlash(name)
	if fi.IsDir() && !strings.HasSuffix(h.Name, "/") {
		h.Name += "/"
	}
	h.Format = t.format
	h.AccessTime, h.ChangeTime = time.Time{}, time.Time{}
	if t.format != tar.FormatPAX {
		h.ModTime = h.ModTime.Truncate(time.Second)
		h.Uname, h.Gname = h.Uname[:min(len(h.Uname), tarNameField)], h.Gname[:min(len(h.Gname), tarNameField)]
	}
	if fi.Mode().IsRegular() {
		if err := t.noteHardLink(path, fi, h); err != nil {
			return err
		}
	}
	if keep != nil && !keep(h) {
		return nil
	}
	if err := t.tw.WriteHeader(h); err != nil {
		return err
	}
	if h.Typeflag == tar.TypeReg {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(t.tw, f, h.Size); err != nil {
			return closeWith(fmt.Errorf("archiving %s: %w", path, err), f)
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if !fi.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.ToSlash(name), "/")
	for _, e := range entries {
		if err := t.Add(filepath.Join(path, e.Name()), base+"/"+e.Name(), keep); err != nil {
			return err
		}
	}
	return nil
}

// Turns h into a hard link to the first member stored for its file, else records h as that member
func (t *TarWriter) noteHardLink(path string, fi fs.FileInfo, h *tar.Header) error {
	id, links, err := linkIdentity(path, fi)
	if err != nil || links <= 1 {
		return err
	}
	if first, ok := t.links[id]; ok && first != h.Name {
		h.Typeflag, h.Linkname, h.Size = tar.TypeLink, first, 0
		return nil
	}
	t.links[id] = h.Name
	return nil
}

// Writes the tar end blocks and the gzip trailer, leaving the underlying writer open
func (t *TarWriter) Close() error {
	return errors.Join(t.tw.Close(), t.gz.Close())
}

// Publishes archive owner-only as a best compression gzip GNU tar of root/base stored under base
func WriteTarGz(ctx context.Context, archive, root, base string) error {
	s, err := Stage(archive, TempPrefix(".archive_"), TempSuffix(".tar.gz.tmp"), Perm(0o600))
	if err != nil {
		return err
	}
	tw, err := NewTarGzWriter(s, gzip.BestCompression, tar.FormatGNU)
	if err != nil {
		return joinErr(err, s.Discard())
	}
	if err := tw.Add(filepath.Join(root, base), base, nil); err != nil {
		return joinErr(err, tw.Close(), s.Discard())
	}
	if err := tw.Close(); err != nil {
		return joinErr(err, s.Discard())
	}
	_, err = s.Commit(ctx)
	return err
}
