package ziputil

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
)

// Size of the write buffer and of the copy buffer AddFile reads with
const bufSize = 256 << 10

// Archive written to a file member by member, each member's sizes and checksum put in its local header before the next member starts, so that members can be dropped again until Close
type Writer struct {
	f        *os.File
	off      int64
	members  []entry
	method   uint16
	level    int
	comment  string
	comps    map[uint16]zip.Compressor
	deflater *flate.Writer
	zstdComp zip.Compressor
	buf      *bufio.Writer
	copyBuf  []byte
	open     *fileWriter
	closed   bool
}

// Finished member: its header, where its local header starts and its MS-DOS date and time
type entry struct {
	fh          *zip.FileHeader
	offset      int64
	date, clock uint16
}

// New archive in the file at name, replacing any file there, whose members are compressed with method, one of zip.Store, zip.Deflate and Zstd, at level unless their header names another method
func Create(name string, method uint16, level int) (*Writer, error) {
	switch method {
	case zip.Store, Zstd:
	case zip.Deflate:
		if level < flate.HuffmanOnly || level > flate.BestCompression {
			return nil, fmt.Errorf("deflate level %d is outside %d to %d: %w", level, flate.HuffmanOnly, flate.BestCompression, fs.ErrInvalid)
		}
	default:
		return nil, zip.ErrAlgorithm
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, method: method, level: level}, nil
}

// Member named name compressed with the archive's method, with mode 0o600 (0o700 for a name ending in a slash, which makes a directory) and the 1980-01-01 date of members without a time
func (w *Writer) Create(name string) (io.WriteCloser, error) {
	fh := &zip.FileHeader{Name: name, Method: w.method}
	if strings.HasSuffix(name, "/") {
		fh.SetMode(fs.ModeDir | 0o700)
	} else {
		fh.SetMode(0o600)
	}
	return w.CreateHeader(fh)
}

// Member described by a copy of fh, so that fh stays the caller's to change or reuse, whose Method picks the compressor and whose UncompressedSize64, when set, is the expected size; the writer settles the flags, versions, sizes and checksum in its copy, and a name ending in a slash makes a directory that takes no data
func (w *Writer) CreateHeader(fh *zip.FileHeader) (io.WriteCloser, error) {
	m, err := w.begin(fh)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Adds the file or directory on disk at file as name, file itself when name is empty, made a relative slash path, with its modification time and mode; a symlink adds what it points to, other kinds of file are refused, and a failure leaves no trace of the member
func (w *Writer) AddFile(file, name string) error {
	if name == "" {
		name = file
	}
	name, err := memberName(name)
	if err != nil {
		return err
	}
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	fh := &zip.FileHeader{Name: name, Method: w.method, Modified: info.ModTime()}
	fh.SetMode(fileMode(info))
	switch {
	case info.IsDir():
		fh.Name += "/"
		_, err := w.begin(fh)
		return err
	case !info.Mode().IsRegular():
		return &fs.PathError{Op: "zip", Path: file, Err: errNotRegular}
	}
	src, err := os.Open(file)
	if err != nil {
		return err
	}
	defer src.Close()
	fh.UncompressedSize64 = uint64(info.Size())
	m, err := w.begin(fh)
	if err != nil {
		return err
	}
	if w.copyBuf == nil {
		w.copyBuf = make([]byte, bufSize)
	}
	if _, err := io.CopyBuffer(m, struct{ io.Reader }{src}, w.copyBuf); err != nil {
		return m.abort(err)
	}
	return m.Close()
}

// Number of members finished so far
func (w *Writer) Len() int { return len(w.members) }

// Drops every member after the first n, an unfinished one included, and cuts their bytes off the file
func (w *Writer) Truncate(n int) error {
	if w.closed {
		return errClosed
	}
	if n < 0 || n > len(w.members) {
		return fmt.Errorf("cannot keep %d of %d zip members: %w", n, len(w.members), fs.ErrInvalid)
	}
	if m := w.open; m != nil {
		m.done, m.result = true, errMemberClosed
		w.open = nil
	}
	if n < len(w.members) {
		w.off = w.members[n].offset
		clear(w.members[n:])
		w.members = w.members[:n]
	}
	return w.f.Truncate(w.off)
}

// Sets the archive comment Close writes
func (w *Writer) SetComment(comment string) error {
	if len(comment) > uint16max {
		return fmt.Errorf("zip comment of %d bytes is longer than %d: %w", len(comment), uint16max, fs.ErrInvalid)
	}
	w.comment = comment
	return nil
}

// Compressor for members of method, taking the place of the built-in one
func (w *Writer) RegisterCompressor(method uint16, comp zip.Compressor) {
	if w.comps == nil {
		w.comps = map[uint16]zip.Compressor{}
	}
	w.comps[method] = comp
}

// Finishes an unfinished member, writes the central directory and end records and closes the file, returning the first failure; a member that fails to finish is left out of an archive that stays valid
func (w *Writer) Close() error {
	if w.closed {
		return errClosed
	}
	var err error
	if w.open != nil {
		err = w.open.Close()
	}
	if derr := w.writeDirectory(); err == nil {
		err = derr
	}
	w.closed = true
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Compressing writer of method into dst, a registered compressor first
func (w *Writer) compressor(method uint16, dst io.Writer) (io.WriteCloser, error) {
	if comp, ok := w.comps[method]; ok {
		return comp(dst)
	}
	switch method {
	case zip.Store:
		return storeWriter{dst}, nil
	case zip.Deflate:
		if w.deflater != nil {
			w.deflater.Reset(dst)
			return w.deflater, nil
		}
		fw, err := flate.NewWriter(dst, w.level)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", err, fs.ErrInvalid)
		}
		w.deflater = fw
		return fw, nil
	case Zstd:
		if w.zstdComp == nil {
			var opts []zstd.EOption
			if w.level > 0 {
				opts = append(opts, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(w.level)))
			}
			w.zstdComp = zstd.ZipCompressor(opts...)
		}
		return w.zstdComp(dst)
	}
	return nil, zip.ErrAlgorithm
}

// Writer passing member data through unchanged
type storeWriter struct{ io.Writer }

// Nothing to flush
func (storeWriter) Close() error { return nil }

// Cuts the archive back to offset after a failed member, returning cause with any failure to cut
func (w *Writer) cut(offset int64, cause error) error {
	w.off = offset
	if err := w.f.Truncate(offset); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Archive name of a path: slash separated and cleaned, without a drive, leading slashes or anything from a NUL on
func memberName(name string) (string, error) {
	clean := name[len(filepath.VolumeName(name)):]
	if i := strings.IndexByte(clean, 0); i >= 0 {
		clean = clean[:i]
	}
	clean = strings.TrimLeft(path.Clean(filepath.ToSlash(clean)), "/")
	if clean == "" || clean == "." {
		return "", fmt.Errorf("zip member name %q names no file: %w", name, fs.ErrInvalid)
	}
	return clean, nil
}
