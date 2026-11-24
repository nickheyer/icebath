package ziputil

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

// Archive open for reading: the embedded zip.Reader with member names and comments decoded (Unicode extra fields, code page 437 for bytes from DOS-family systems that are not UTF-8, other bytes kept as stored) and decoders for Deflate, Deflate64, bzip2, LZMA, Zstandard and XZ members registered
type Reader struct {
	*zip.Reader
	file      *os.File
	size      int64
	byName    map[string]*zip.File
	placement func() *layout
}

// Archive at path opened for reading
func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReader(f, info.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	r.file = f
	return r, nil
}

// Archive of size bytes read through ra; members whose names are unsafe to extract are kept, Extract making every name safe
func NewReader(ra io.ReaderAt, size int64) (*Reader, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}
	r := &Reader{
		Reader: zr,
		size:   size,
		byName: make(map[string]*zip.File, len(zr.File)),
	}
	r.placement = sync.OnceValue(r.placeMembers)
	for _, f := range zr.File {
		decodeName(f)
		r.byName[f.Name] = f
	}
	zr.RegisterDecompressor(zip.Deflate, inflate)
	zr.RegisterDecompressor(Deflate64, inflate64)
	zr.RegisterDecompressor(Bzip2, unbzip2)
	zr.RegisterDecompressor(LZMA, r.unlzma)
	zr.RegisterDecompressor(Zstd, unzstd)
	zr.RegisterDecompressor(zstdPKWare, unzstd)
	zr.RegisterDecompressor(XZ, unxz)
	return r, nil
}

// Whether path holds an archive whose end record leads to a readable central directory, without reading any member
func IsZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	_, err = zip.NewReader(f, info.Size())
	return err == nil || errors.Is(err, zip.ErrInsecurePath)
}

// Closes the file OpenReader opened, nothing to do for a reader NewReader made
func (r *Reader) Close() error {
	if r.file == nil {
		return nil
	}
	return r.file.Close()
}

// Member names in central directory order
func (r *Reader) Names() []string {
	names := make([]string, len(r.File))
	for i, f := range r.File {
		names[i] = f.Name
	}
	return names
}

// Member of a name, the last one when the archive repeats the name
func (r *Reader) Lookup(name string) (*zip.File, bool) {
	f, ok := r.byName[name]
	return f, ok
}

// Data of the member of a name, the last one when the archive repeats it: refused with ErrEncrypted or zip.ErrAlgorithm when it cannot be decoded, and with damage, overlap with another member included, matching ErrCorrupt; this takes the place of the fs.FS Open, which r.Reader keeps offering
func (r *Reader) Open(name string) (io.ReadCloser, error) {
	f, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("zip member %q: %w", name, fs.ErrNotExist)
	}
	return r.openFile(f)
}

// Checked data stream of one member
func (r *Reader) openFile(f *zip.File) (io.ReadCloser, error) {
	switch {
	case f.Flags&(flagEncrypted|flagStrongEncryption) != 0:
		return nil, ErrEncrypted
	case f.Flags&flagPatched != 0:
		return nil, fmt.Errorf("zip member holds patch data: %w", zip.ErrAlgorithm)
	case r.placement().overlap[f]:
		return nil, &corruptError{errOverlap}
	}
	rc, err := f.Open()
	switch {
	case err == nil:
		return &memberReader{rc: rc}, nil
	case errors.Is(err, zip.ErrAlgorithm):
		return nil, err
	case errors.Is(err, io.EOF):
		err = io.ErrUnexpectedEOF
	}
	return nil, corrupt(err)
}

// Member data stream whose failures, other than operating system ones, match ErrCorrupt
type memberReader struct {
	rc     io.ReadCloser
	closed bool
}

// Member bytes, a failure that is not the operating system's marked ErrCorrupt, and an error wrapping fs.ErrClosed once closed
func (m *memberReader) Read(p []byte) (int, error) {
	if m.closed {
		return 0, errMemberClosed
	}
	n, err := m.rc.Read(p)
	return n, corrupt(err)
}

// Releases the member's decoder, once
func (m *memberReader) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	return m.rc.Close()
}
