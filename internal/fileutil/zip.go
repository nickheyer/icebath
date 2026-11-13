package fileutil

import (
	"archive/zip"
	"bytes"
	"cmp"
	"compress/bzip2"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz/lzma"
	"golang.org/x/text/encoding/charmap"
)

// Zip method numbers readable beside stored and deflated
const (
	zipBzip2 = 12
	zipLZMA  = 14
	zipZstd  = 93
)

// Record sizes and signatures of the zip container
const (
	zipLocalSig        = 0x04034b50
	zipCentralSig      = 0x02014b50
	zipEndSig          = 0x06054b50
	zipEnd64Sig        = 0x06064b50
	zipEnd64LocSig     = 0x07064b50
	zipEndLen          = 22
	zipEnd64Len        = 56
	zipEnd64LocLen     = 20
	zipCentralLen      = 46
	zipLocalLen        = 30
	zip64Limit         = 1<<31 - 1
	zipMaxVersion      = 63
	zipUTF8Flag        = 0x800
	zipEncryptedFlag   = 0x1
	zipPatchedFlag     = 0x20
	zipStrongCryptFlag = 0x40
	zipUnicodePathTag  = 0x7075
	zipSymlinkType     = 0o120000
	zipTypeMask        = 0o170000
)

// Longest symlink target text a zip extraction reads from a symlink member
const maxZipLinkTarget = 64 << 10

// Error for a zip writer used after Close
var errZipClosed = errors.New("zip writer already closed")

// Zip archive with bzip2, LZMA and zstd members readable, names decoded per the format and members checked on open
type ZipReader struct {
	files   []*zip.File
	index   map[*zip.File]int
	byName  map[string]*zip.File
	dirName []string
	offsets []int64
	ends    []int64
	comment string
	r       io.ReaderAt
	closer  io.Closer
}

// Opens the zip archive at path
func OpenZip(path string) (*ZipReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, closeWith(err, f)
	}
	z, err := NewZipReader(f, fi.Size())
	if err != nil {
		return nil, closeWith(err, f)
	}
	z.closer = f
	return z, nil
}

// Zip archive read from r, refusing a corrupt directory, an unsupported format version or a corrupt extra field
func NewZipReader(r io.ReaderAt, size int64) (*ZipReader, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}
	zr.RegisterDecompressor(zipBzip2, func(r io.Reader) io.ReadCloser { return io.NopCloser(bzip2.NewReader(r)) })
	zr.RegisterDecompressor(zipLZMA, func(r io.Reader) io.ReadCloser { return &zipLZMAReader{src: r} })
	zr.RegisterDecompressor(zipZstd, newZipZstdReader)
	offsets, rawNames, dirStart, err := zipHeaderOffsets(r, size)
	if err != nil {
		return nil, err
	}
	if len(offsets) != len(zr.File) {
		return nil, zip.ErrFormat
	}
	z := &ZipReader{files: zr.File, index: map[*zip.File]int{}, byName: map[string]*zip.File{}, offsets: offsets,
		dirName: make([]string, len(zr.File)), ends: make([]int64, len(zr.File)), comment: zr.Comment, r: r}
	order := make([]int, len(offsets))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(offsets[b], offsets[a]) })
	end := dirStart
	for _, i := range order {
		z.ends[i], end = end, offsets[i]
	}
	for i, f := range zr.File {
		if rawNames[i] != f.Name {
			return nil, zip.ErrFormat
		}
		if v := f.ReaderVersion & 0xff; v > zipMaxVersion {
			return nil, fmt.Errorf("zip member %s needs format version %d.%d: %w", f.Name, v/10, v%10, zip.ErrAlgorithm)
		}
		decoded, err := decodeZipName([]byte(f.Name), f.Flags)
		if err != nil {
			return nil, err
		}
		name, err := zipExtraName(f, decoded)
		if err != nil {
			return nil, err
		}
		z.dirName[i] = decoded
		if cut := strings.IndexByte(name, 0); cut >= 0 {
			name = name[:cut]
		}
		f.Name = zipSlashes(name)
		z.index[f] = i
		z.byName[f.Name] = f
	}
	return z, nil
}

// Members in central directory order
func (z *ZipReader) Files() []*zip.File { return z.files }

// Last member stored under name
func (z *ZipReader) Lookup(name string) (*zip.File, bool) {
	f, ok := z.byName[name]
	return f, ok
}

// Archive comment text
func (z *ZipReader) Comment() string { return z.comment }

// Closes the archive file OpenZip opened, nothing for an archive read from a caller supplied reader
func (z *ZipReader) Close() error {
	if z.closer == nil {
		return nil
	}
	return z.closer.Close()
}

// Opens member f once its local header, name, extent and flags check out, its CRC verified as the stream ends
func (z *ZipReader) Open(f *zip.File) (io.ReadCloser, error) {
	i, ok := z.index[f]
	if !ok {
		return nil, fmt.Errorf("zip member %s is not in this archive", f.Name)
	}
	head := make([]byte, zipLocalLen)
	if n, err := z.r.ReadAt(head, z.offsets[i]); n != zipLocalLen {
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return nil, fmt.Errorf("zip member %s has a truncated local header: %w", f.Name, zip.ErrFormat)
	}
	le := binary.LittleEndian
	if le.Uint32(head) != zipLocalSig {
		return nil, fmt.Errorf("zip member %s has a bad local header signature: %w", f.Name, zip.ErrFormat)
	}
	if f.Flags&zipPatchedFlag != 0 || f.Flags&zipStrongCryptFlag != 0 {
		return nil, fmt.Errorf("zip member %s holds patched data or strong encryption: %w", f.Name, zip.ErrAlgorithm)
	}
	nameLen, extraLen := int64(le.Uint16(head[26:])), int64(le.Uint16(head[28:]))
	raw := make([]byte, nameLen)
	n, err := z.r.ReadAt(raw, z.offsets[i]+zipLocalLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	local, err := decodeZipName(raw[:n], le.Uint16(head[6:]))
	if err != nil {
		return nil, err
	}
	if local != z.dirName[i] {
		return nil, fmt.Errorf("zip member %s is named %q in its local header: %w", z.dirName[i], local, zip.ErrFormat)
	}
	if dataStart := z.offsets[i] + zipLocalLen + nameLen + extraLen; dataStart > z.ends[i] || f.CompressedSize64 > uint64(z.ends[i]-dataStart) {
		return nil, &UnsafeMemberError{Member: f.Name, Reason: reasonOverlap}
	}
	if f.Flags&zipEncryptedFlag != 0 {
		return nil, fmt.Errorf("zip member %s is encrypted: %w", f.Name, zip.ErrAlgorithm)
	}
	return f.Open()
}

// Member name decoded as UTF-8 under the language encoding flag and as code page 437 without it
func decodeZipName(raw []byte, flags uint16) (string, error) {
	if flags&zipUTF8Flag != 0 {
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("zip member name %q is not the UTF-8 its flag claims: %w", raw, zip.ErrFormat)
		}
		return string(raw), nil
	}
	return charmap.CodePage437.NewDecoder().String(string(raw))
}

// Name the Info-ZIP unicode path field gives f when its CRC matches the stored name, every extra field checked
func zipExtraName(f *zip.File, name string) (string, error) {
	le := binary.LittleEndian
	nameCRC := crc32.ChecksumIEEE([]byte(f.Name))
	for extra := f.Extra; len(extra) >= 4; {
		tag, fieldLen := le.Uint16(extra), int(le.Uint16(extra[2:]))
		if fieldLen+4 > len(extra) {
			return "", fmt.Errorf("zip member %s has a corrupt extra field %04x: %w", f.Name, tag, zip.ErrFormat)
		}
		data := extra[4 : 4+fieldLen]
		extra = extra[4+fieldLen:]
		if tag != zipUnicodePathTag {
			continue
		}
		if len(data) < 5 {
			return "", fmt.Errorf("zip member %s has a corrupt unicode path field: %w", f.Name, zip.ErrFormat)
		}
		if data[0] != 1 || le.Uint32(data[1:]) != nameCRC {
			continue
		}
		if !utf8.Valid(data[5:]) {
			return "", fmt.Errorf("zip member %s has a unicode path field that is not UTF-8: %w", f.Name, zip.ErrFormat)
		}
		if len(data) > 5 {
			name = string(data[5:])
		}
	}
	return name, nil
}

// Stream that fails every read with one error
type failingReader struct{ err error }

// Fails with the stored error
func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// Releases nothing
func (f failingReader) Close() error { return nil }

// Single threaded zstd decoder of one member
func newZipZstdReader(r io.Reader) io.ReadCloser {
	d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return failingReader{err: err}
	}
	return d.IOReadCloser()
}

// LZMA member stream, its zip property header turned into a classic header with an unknown size
type zipLZMAReader struct {
	src io.Reader
	dec io.Reader
	err error
}

// Decodes member bytes, an input ending without an end marker reported as the end so size and CRC checks decide
func (z *zipLZMAReader) Read(p []byte) (int, error) {
	if z.dec == nil && z.err == nil {
		z.dec, z.err = openZipLZMA(z.src)
	}
	if z.err != nil {
		return 0, z.err
	}
	n, err := z.dec.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

// Releases nothing
func (z *zipLZMAReader) Close() error { return nil }

// Classic LZMA reader over a zip LZMA member, whose header holds a version and a five byte property block
func openZipLZMA(src io.Reader) (io.Reader, error) {
	var head [4]byte
	if _, err := io.ReadFull(src, head[:]); err != nil {
		return nil, err
	}
	if size := binary.LittleEndian.Uint16(head[2:]); size != 5 {
		return nil, fmt.Errorf("zip LZMA member has %d property bytes, not 5", size)
	}
	props := make([]byte, 5, 13)
	if _, err := io.ReadFull(src, props); err != nil {
		return nil, err
	}
	header := append(props, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	return lzma.NewReader(io.MultiReader(bytes.NewReader(header), src))
}

// Local header offsets, raw names and directory start of the central directory as archive/zip reads it
func zipHeaderOffsets(r io.ReaderAt, size int64) ([]int64, []string, int64, error) {
	var buf []byte
	endAt := int64(-1)
	for i, blockLen := range []int64{1024, 65 * 1024} {
		blockLen = min(blockLen, size)
		buf = make([]byte, blockLen)
		if _, err := r.ReadAt(buf, size-blockLen); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, 0, err
		}
		if p := findZipEnd(buf); p >= 0 {
			buf, endAt = buf[p:], size-blockLen+int64(p)
			break
		}
		if i == 1 || blockLen == size {
			return nil, nil, 0, zip.ErrFormat
		}
	}
	le := binary.LittleEndian
	records, dirSize, dirOffset := uint64(le.Uint16(buf[10:])), uint64(le.Uint32(buf[12:])), uint64(le.Uint32(buf[16:]))
	if records == 0xffff || dirSize == 0xffff || dirOffset == 0xffffffff {
		if p, ok, err := findZip64End(r, endAt); err != nil {
			return nil, nil, 0, err
		} else if ok {
			endAt = p
			if dirSize, dirOffset, err = readZip64End(r, p); err != nil {
				return nil, nil, 0, err
			}
		}
	}
	if dirSize > 1<<63-1 || dirOffset > 1<<63-1 {
		return nil, nil, 0, zip.ErrFormat
	}
	base := endAt - int64(dirSize) - int64(dirOffset)
	if o := base + int64(dirOffset); o < 0 || o >= size {
		return nil, nil, 0, zip.ErrFormat
	}
	if base > 0 && centralEntryAt(r, int64(dirOffset), size) {
		base = 0
	}
	dirStart := base + int64(dirOffset)
	var offsets []int64
	var names []string
	for pos := dirStart; ; {
		offset, name, next, ok, err := readCentralEntry(r, pos, size)
		if err != nil {
			return nil, nil, 0, err
		}
		if !ok {
			break
		}
		offsets = append(offsets, offset+base)
		names = append(names, name)
		pos = next
	}
	return offsets, names, dirStart, nil
}

// Index of the end of central directory record in b whose comment fits, or -1
func findZipEnd(b []byte) int {
	for i := len(b) - zipEndLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(b[i:]) == zipEndSig {
			n := int(binary.LittleEndian.Uint16(b[i+zipEndLen-2:]))
			if n+zipEndLen+i > len(b) {
				return -1
			}
			return i
		}
	}
	return -1
}

// Offset of the zip64 end record named by a single disk locator just before the end record
func findZip64End(r io.ReaderAt, endAt int64) (int64, bool, error) {
	at := endAt - zipEnd64LocLen
	if at < 0 {
		return 0, false, nil
	}
	b := make([]byte, zipEnd64LocLen)
	if _, err := r.ReadAt(b, at); err != nil {
		return 0, false, err
	}
	le := binary.LittleEndian
	if le.Uint32(b) != zipEnd64LocSig || le.Uint32(b[4:]) != 0 || le.Uint32(b[16:]) != 1 {
		return 0, false, nil
	}
	return int64(le.Uint64(b[8:])), true, nil
}

// Central directory size and offset of the zip64 end record at offset
func readZip64End(r io.ReaderAt, offset int64) (uint64, uint64, error) {
	b := make([]byte, zipEnd64Len)
	if _, err := r.ReadAt(b, offset); err != nil {
		return 0, 0, err
	}
	le := binary.LittleEndian
	if le.Uint32(b) != zipEnd64Sig {
		return 0, 0, zip.ErrFormat
	}
	return le.Uint64(b[40:]), le.Uint64(b[48:]), nil
}

// Reports whether a whole central directory entry starts at pos
func centralEntryAt(r io.ReaderAt, pos, size int64) bool {
	_, _, _, ok, err := readCentralEntry(r, pos, size)
	return ok && err == nil
}

// Local header offset and raw name of the central directory entry at pos and where the next one starts
func readCentralEntry(r io.ReaderAt, pos, size int64) (int64, string, int64, bool, error) {
	if pos < 0 || pos+zipCentralLen > size {
		return 0, "", 0, false, nil
	}
	b := make([]byte, zipCentralLen)
	if _, err := r.ReadAt(b, pos); err != nil {
		return 0, "", 0, false, err
	}
	le := binary.LittleEndian
	if le.Uint32(b) != zipCentralSig {
		return 0, "", 0, false, nil
	}
	nameLen, extraLen, commentLen := int64(le.Uint16(b[28:])), int64(le.Uint16(b[30:])), int64(le.Uint16(b[32:]))
	next := pos + zipCentralLen + nameLen + extraLen + commentLen
	if next > size {
		return 0, "", 0, false, nil
	}
	variable := make([]byte, nameLen+extraLen)
	if _, err := r.ReadAt(variable, pos+zipCentralLen); err != nil {
		return 0, "", 0, false, err
	}
	offset := int64(le.Uint32(b[42:]))
	needU, needC, needOffset := le.Uint32(b[24:]) == 0xffffffff, le.Uint32(b[20:]) == 0xffffffff, offset == 0xffffffff
	for extra := variable[nameLen:]; len(extra) >= 4; {
		tag, fieldLen := le.Uint16(extra), int(le.Uint16(extra[2:]))
		extra = extra[4:]
		if len(extra) < fieldLen {
			break
		}
		field := extra[:fieldLen]
		extra = extra[fieldLen:]
		if tag != 0x0001 {
			continue
		}
		for _, need := range []*bool{&needU, &needC} {
			if *need {
				if len(field) < 8 {
					return 0, "", 0, false, nil
				}
				*need, field = false, field[8:]
			}
		}
		if needOffset {
			if len(field) < 8 {
				return 0, "", 0, false, nil
			}
			offset, needOffset = int64(le.Uint64(field)), false
		}
	}
	return offset, string(variable[:nameLen]), next, true, nil
}

// One member the writer finished, kept for the central directory
type zipEntry struct {
	name       []byte
	flags      uint16
	modTime    uint16
	modDate    uint16
	crc        uint32
	csize      uint64
	usize      uint64
	offset     int64
	extAttr    uint32
	zip64Local bool
}

// Deflating zip writer over a file that leaves no trace of a member whose source read fails
type ZipWriter struct {
	f       *os.File
	level   int
	end     int64
	entries []zipEntry
	closed  bool
}

// Error for a file whose modification time falls outside the 1980 to 2107 span DOS dates hold
var errZipTime = errors.New("zip does not support timestamps before 1980 or after 2107")

// Zip writer filling f from its first byte, members deflated at the flate level
func NewZipWriter(f *os.File, level int) (*ZipWriter, error) {
	if level < flate.HuffmanOnly || level > flate.BestCompression {
		return nil, fmt.Errorf("deflate level %d out of range", level)
	}
	if err := f.Truncate(0); err != nil {
		return nil, err
	}
	return &ZipWriter{f: f, level: level}, nil
}

// Member name stored for name, cleaned with drive and leading separators dropped, forward slashed and cut at NUL
func zipArcName(name string) (string, error) {
	n := filepath.Clean(name[len(filepath.VolumeName(name)):])
	n = strings.TrimLeft(n, "/"+string(filepath.Separator))
	if i := strings.IndexByte(n, 0); i >= 0 {
		n = n[:i]
	}
	n = filepath.ToSlash(n)
	if n == "" || n == "." || n == ".." || strings.HasPrefix(n, "../") || !utf8.ValidString(n) {
		return "", &UnsafeMemberError{Member: name, Reason: reasonUnsafePath}
	}
	return n, nil
}

// Adds the regular file at path as member name, a failure truncating the archive back to before the member
func (z *ZipWriter) AddFile(path, name string) error {
	if z.closed {
		return errZipClosed
	}
	member, err := zipArcName(name)
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	fi, err := src.Stat()
	if err != nil {
		return closeWith(err, src)
	}
	if !fi.Mode().IsRegular() {
		return closeWith(&fs.PathError{Op: "zip", Path: path, Err: errNotRegular}, src)
	}
	e := zipEntry{name: []byte(member), offset: z.end, extAttr: zipModeBits(fi.Mode()|synthesizedExec(path)) << 16}
	for i := 0; i < len(member); i++ {
		if member[i] >= 0x80 {
			e.flags = zipUTF8Flag
			break
		}
	}
	if e.modTime, e.modDate, err = dosTime(fi.ModTime()); err != nil {
		return closeWith(&fs.PathError{Op: "zip", Path: path, Err: err}, src)
	}
	e.zip64Local = float64(fi.Size())*1.05 > zip64Limit
	end, err := z.writeMember(&e, src)
	if err != nil {
		return joinErr(err, src.Close(), z.f.Truncate(z.end))
	}
	if err := src.Close(); err != nil {
		return joinErr(err, z.f.Truncate(z.end))
	}
	z.end = end
	z.entries = append(z.entries, e)
	return nil
}

// Writes the local header and deflated data of e from src, rewriting the header with the final sizes
func (z *ZipWriter) writeMember(e *zipEntry, src io.Reader) (int64, error) {
	header := e.localHeader()
	if _, err := z.f.WriteAt(header, e.offset); err != nil {
		return 0, err
	}
	dataStart := e.offset + int64(len(header))
	out := &offsetWriter{f: z.f, off: dataStart}
	fw, err := flate.NewWriter(out, z.level)
	if err != nil {
		return 0, err
	}
	sum := crc32.NewIEEE()
	n, err := io.Copy(io.MultiWriter(fw, sum), src)
	if err != nil {
		return 0, err
	}
	if err := fw.Close(); err != nil {
		return 0, err
	}
	e.crc, e.usize, e.csize = sum.Sum32(), uint64(n), uint64(out.off-dataStart)
	if !e.zip64Local && (e.usize > zip64Limit || e.csize > zip64Limit) {
		return 0, fmt.Errorf("zip member %s grew past the size its header allows", e.name)
	}
	if _, err := z.f.WriteAt(e.localHeader(), e.offset); err != nil {
		return 0, err
	}
	return out.off, nil
}

// Writes the central directory and end records, leaving f open for the caller to commit
func (z *ZipWriter) Close() error {
	if z.closed {
		return nil
	}
	z.closed = true
	var dir []byte
	for i := range z.entries {
		dir = append(dir, z.entries[i].centralHeader()...)
	}
	dir = append(dir, zipEndRecords(uint64(len(z.entries)), uint64(len(dir)), uint64(z.end))...)
	if _, err := z.f.WriteAt(dir, z.end); err != nil {
		return err
	}
	return z.f.Truncate(z.end + int64(len(dir)))
}

// Writer placing bytes at a moving offset of a file
type offsetWriter struct {
	f   *os.File
	off int64
}

// Writes p at the current offset and advances past it
func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}

// Version a header states, 45 once zip64 fields are involved and 20 otherwise
func zipVersion(zip64 bool) uint16 {
	if zip64 {
		return 45
	}
	return 20
}

// Local file header, a zip64 extra carrying the sizes when the member may outgrow 32 bit fields
func (e *zipEntry) localHeader() []byte {
	le := binary.LittleEndian
	csize, usize := uint32(e.csize), uint32(e.usize)
	var extra []byte
	if e.zip64Local {
		extra = le.AppendUint16(extra, 0x0001)
		extra = le.AppendUint16(extra, 16)
		extra = le.AppendUint64(extra, e.usize)
		extra = le.AppendUint64(extra, e.csize)
		csize, usize = 0xffffffff, 0xffffffff
	}
	b := le.AppendUint32(nil, zipLocalSig)
	b = le.AppendUint16(b, zipVersion(e.zip64Local))
	b = le.AppendUint16(b, e.flags)
	b = le.AppendUint16(b, zip.Deflate)
	b = le.AppendUint16(b, e.modTime)
	b = le.AppendUint16(b, e.modDate)
	b = le.AppendUint32(b, e.crc)
	b = le.AppendUint32(b, csize)
	b = le.AppendUint32(b, usize)
	b = le.AppendUint16(b, uint16(len(e.name)))
	b = le.AppendUint16(b, uint16(len(extra)))
	return append(append(b, e.name...), extra...)
}

// Central directory entry, zip64 values for sizes and offset that pass the 32 bit limit
func (e *zipEntry) centralHeader() []byte {
	le := binary.LittleEndian
	var values []uint64
	csize, usize, offset := uint32(e.csize), uint32(e.usize), uint32(e.offset)
	if e.usize > zip64Limit || e.csize > zip64Limit {
		values = append(values, e.usize, e.csize)
		csize, usize = 0xffffffff, 0xffffffff
	}
	if e.offset > zip64Limit {
		values = append(values, uint64(e.offset))
		offset = 0xffffffff
	}
	var extra []byte
	if len(values) > 0 {
		extra = le.AppendUint16(extra, 0x0001)
		extra = le.AppendUint16(extra, uint16(8*len(values)))
		for _, v := range values {
			extra = le.AppendUint64(extra, v)
		}
	}
	version := zipVersion(e.zip64Local || len(values) > 0)
	b := le.AppendUint32(nil, zipCentralSig)
	b = append(b, byte(version), zipCreatorSystem)
	b = le.AppendUint16(b, version)
	b = le.AppendUint16(b, e.flags)
	b = le.AppendUint16(b, zip.Deflate)
	b = le.AppendUint16(b, e.modTime)
	b = le.AppendUint16(b, e.modDate)
	b = le.AppendUint32(b, e.crc)
	b = le.AppendUint32(b, csize)
	b = le.AppendUint32(b, usize)
	b = le.AppendUint16(b, uint16(len(e.name)))
	b = le.AppendUint16(b, uint16(len(extra)))
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint32(b, e.extAttr)
	b = le.AppendUint32(b, offset)
	return append(append(b, e.name...), extra...)
}

// End of central directory records, zip64 ones first when the count, size or offset outgrow the classic fields
func zipEndRecords(count, size, offset uint64) []byte {
	le := binary.LittleEndian
	var b []byte
	if count > 0xffff || size > zip64Limit || offset > zip64Limit {
		b = le.AppendUint32(b, zipEnd64Sig)
		b = le.AppendUint64(b, zipEnd64Len-12)
		b = le.AppendUint16(b, 45)
		b = le.AppendUint16(b, 45)
		b = le.AppendUint32(b, 0)
		b = le.AppendUint32(b, 0)
		b = le.AppendUint64(b, count)
		b = le.AppendUint64(b, count)
		b = le.AppendUint64(b, size)
		b = le.AppendUint64(b, offset)
		b = le.AppendUint32(b, zipEnd64LocSig)
		b = le.AppendUint32(b, 0)
		b = le.AppendUint64(b, offset+size)
		b = le.AppendUint32(b, 1)
		count, size, offset = min(count, 0xffff), min(size, 0xffffffff), min(offset, 0xffffffff)
	}
	b = le.AppendUint32(b, zipEndSig)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, 0)
	b = le.AppendUint16(b, uint16(count))
	b = le.AppendUint16(b, uint16(count))
	b = le.AppendUint32(b, uint32(size))
	b = le.AppendUint32(b, uint32(offset))
	return le.AppendUint16(b, 0)
}

// POSIX st_mode of a regular file with the given mode, the value zip keeps in the upper external attribute half
func zipModeBits(m fs.FileMode) uint32 {
	bits := uint32(0o100000) | uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// DOS time and date words of t in local time, refused outside the 1980 to 2107 span DOS dates hold
func dosTime(t time.Time) (uint16, uint16, error) {
	t = t.Local()
	if t.Year() < 1980 || t.Year() > 2107 {
		return 0, 0, errZipTime
	}
	return uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2), uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day()), nil
}

// Extracts the zip archive at path into dest under policy, names placed as the zip extractor places them
func ExtractZip(ctx context.Context, path, dest string, policy ExtractPolicy) error {
	zr, err := OpenZip(path)
	if err != nil {
		return err
	}
	return closeWith(zr.Extract(ctx, dest, policy), zr)
}

// Symlink member held back until every other member has landed
type zipLink struct {
	name string
	text string
}

// Places every member under dest, FilesOnly refusing escapes and links up front and ContainedLinks linking last
func (z *ZipReader) Extract(ctx context.Context, dest string, policy ExtractPolicy) error {
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	root, err := resolveLax(dest)
	if err != nil {
		return err
	}
	if policy == FilesOnly {
		if err := z.refuseEscapes(root); err != nil {
			return err
		}
	}
	var links []zipLink
	for _, f := range z.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		mode := f.ExternalAttrs >> 16
		if policy == ContainedLinks && mode&zipTypeMask == zipSymlinkType {
			text, err := z.linkText(f)
			if err != nil {
				return err
			}
			links = append(links, zipLink{name: f.Name, text: text})
			continue
		}
		written, err := z.extractMember(f, root)
		if err != nil {
			return err
		}
		if policy == ContainedLinks && mode&0o111 != 0 {
			if fi, err := os.Stat(written); err == nil && fi.Mode().IsRegular() {
				if err := os.Chmod(written, fs.FileMode(mode&0o777)); err != nil {
					return err
				}
			}
		}
	}
	for _, l := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := placeZipLink(l, root); err != nil {
			return err
		}
	}
	return nil
}

// Refuses the archive before any member lands when a name reaches outside root or a member is a symlink
func (z *ZipReader) refuseEscapes(root string) error {
	for _, f := range z.files {
		reached, err := resolveLax(joinMember(root, f.Name))
		if err != nil {
			return err
		}
		if !within(reached, root) {
			return &UnsafeMemberError{Member: f.Name, Reason: reasonZipSlip, Target: reached}
		}
		if (f.ExternalAttrs>>16)&zipTypeMask == zipSymlinkType {
			return &UnsafeMemberError{Member: f.Name, Reason: reasonSymlinkMember}
		}
	}
	return nil
}

// Location the zip extractor gives a member under root, drive, empty, dot and parent components dropped
func zipMemberTarget(name, root string) (string, bool, error) {
	sep := string(filepath.Separator)
	isDir := strings.HasSuffix(name, "/")
	arc := strings.ReplaceAll(name, "/", sep)
	arc = arc[len(filepath.VolumeName(arc)):]
	var parts []string
	for _, p := range strings.Split(arc, sep) {
		if p != "" && p != "." && p != ".." {
			parts = append(parts, p)
		}
	}
	parts = legalNameParts(parts)
	if len(parts) == 0 && !isDir {
		return "", false, &UnsafeMemberError{Member: name, Reason: reasonEmptyName}
	}
	return filepath.Join(append([]string{root}, parts...)...), isDir, nil
}

// Writes one member where the zip extractor places it and returns that path
func (z *ZipReader) extractMember(f *zip.File, root string) (string, error) {
	target, isDir, err := zipMemberTarget(f.Name, root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return "", err
	}
	if isDir {
		if err := os.Mkdir(target, 0o777); err != nil {
			if fi, statErr := os.Stat(target); statErr != nil || !fi.IsDir() {
				return "", err
			}
		}
		return target, nil
	}
	rc, err := z.Open(f)
	if err != nil {
		return "", err
	}
	return target, closeWith(writeMember(target, rc), rc)
}

// Target text of a symlink member, CRC checked and refused when not UTF-8 or too long
func (z *ZipReader) linkText(f *zip.File) (string, error) {
	rc, err := z.Open(f)
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, maxZipLinkTarget+1))
	if err := closeWith(err, rc); err != nil {
		return "", err
	}
	if len(raw) > maxZipLinkTarget || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return "", &UnsafeMemberError{Member: f.Name, Reason: reasonBadLink}
	}
	return string(raw), nil
}

// Creates a held back symlink staying inside root with its target, writing its text where symlinks are refused
func placeZipLink(l zipLink, root string) error {
	link, err := resolveLax(joinMember(root, l.name))
	if err != nil {
		return err
	}
	if !within(link, root) || filepath.IsAbs(filepath.FromSlash(l.text)) {
		return nil
	}
	resolved, err := resolveLax(joinMember(filepath.Dir(link), l.text))
	if err != nil {
		return err
	}
	if !within(resolved, root) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o777); err != nil {
		return err
	}
	if os.Symlink(l.text, link) == nil {
		return nil
	}
	return writeLinkText(link, l.text)
}

// Reports whether path ends in a zip end record whose central directory opens where it says, empty archives counting
func IsZip(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return zipEndPlausible(f, fi.Size())
}

// Locates the end record the way the zip reader does and checks the central directory signature it points at
func zipEndPlausible(r io.ReaderAt, size int64) bool {
	end, at := zipEndRecord(r, size)
	if end == nil {
		return false
	}
	le := binary.LittleEndian
	disk, dirDisk, total := le.Uint16(end[4:]), le.Uint16(end[6:]), uint64(le.Uint16(end[10:]))
	dirSize, dirOffset := uint64(le.Uint32(end[12:])), uint64(le.Uint32(end[16:]))
	start := at
	if locAt := at - zipEnd64LocLen; locAt >= 0 {
		loc := make([]byte, zipEnd64LocLen)
		if _, err := r.ReadAt(loc, locAt); err == nil && le.Uint32(loc) == zipEnd64LocSig {
			if le.Uint32(loc[4:]) != 0 || le.Uint32(loc[16:]) > 1 {
				return false
			}
			rec := make([]byte, zipEnd64Len)
			if _, err := r.ReadAt(rec, locAt-zipEnd64Len); err == nil && le.Uint32(rec) == zipEnd64Sig {
				disk, dirDisk = uint16(le.Uint32(rec[16:])), uint16(le.Uint32(rec[20:]))
				total, dirSize, dirOffset = le.Uint64(rec[32:]), le.Uint64(rec[40:]), le.Uint64(rec[48:])
				start = locAt - zipEnd64Len
			}
		}
	}
	if total == 0 && dirSize == 0 && dirOffset == 0 {
		return true
	}
	if disk != dirDisk || dirSize < zipCentralLen || dirSize > uint64(start) {
		return false
	}
	head := make([]byte, zipCentralLen)
	if _, err := r.ReadAt(head, start-int64(dirSize)); err != nil {
		return false
	}
	return le.Uint32(head) == zipCentralSig
}

// End of central directory record and offset, a comment-free tail tried before the last signature in 64 KiB
func zipEndRecord(r io.ReaderAt, size int64) ([]byte, int64) {
	if size < zipEndLen {
		return nil, 0
	}
	tail := make([]byte, zipEndLen)
	if _, err := r.ReadAt(tail, size-zipEndLen); err != nil {
		return nil, 0
	}
	if binary.LittleEndian.Uint32(tail) == zipEndSig && tail[zipEndLen-2] == 0 && tail[zipEndLen-1] == 0 {
		return tail, size - zipEndLen
	}
	from := max(size-(1<<16)-zipEndLen, 0)
	window := make([]byte, size-from)
	if _, err := r.ReadAt(window, from); err != nil {
		return nil, 0
	}
	i := bytes.LastIndex(window, []byte{0x50, 0x4b, 0x05, 0x06})
	if i < 0 || len(window)-i < zipEndLen {
		return nil, 0
	}
	return window[i : i+zipEndLen], from + int64(i)
}
