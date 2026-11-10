package pyzip

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"

	"github.com/ulikunitz/xz/lzma"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Compression methods the reader decodes beside stored and deflated
const (
	Bzip2     = 12
	LZMA      = 14
	Zstandard = 93
)

const (
	sizeEndCentDir          = 22
	sizeEndCentDir64        = 56
	sizeEndCentDir64Locator = 20
	sizeCentralDir          = 46
	sizeFileHeader          = 30
	maxComment              = 1<<16 - 1
	maxExtractVersion       = 63
	minReadSize             = 4096
	maskEncrypted           = 1 << 0
	maskCompressedPatch     = 1 << 5
	maskStrongEncryption    = 1 << 6
	maskUTFFilename         = 1 << 11
)

var (
	stringEndArchive          = []byte("PK\x05\x06")
	stringEndArchive64        = []byte("PK\x06\x06")
	stringEndArchive64Locator = []byte("PK\x06\x07")
	stringCentralDir          = []byte("PK\x01\x02")
	stringFileHeader          = []byte("PK\x03\x04")
)

// zipfile.BadZipFile raised for a damaged archive or member
type BadZipFile struct{ Msg string }

func (e *BadZipFile) Error() string      { return e.Msg }
func (e *BadZipFile) PyTypeName() string { return "BadZipFile" }

// EOFError a member read raises when the archive ends inside its data
type EOFError struct{ Msg string }

func (e *EOFError) Error() string      { return e.Msg }
func (e *EOFError) PyTypeName() string { return "EOFError" }

// lzma.LZMAError of a damaged LZMA member
type LZMAError struct{ Msg string }

func (e *LZMAError) Error() string      { return e.Msg }
func (e *LZMAError) PyTypeName() string { return "LZMAError" }

// compression.zstd.ZstdError of a damaged Zstandard member
type ZstdError struct{ Msg string }

func (e *ZstdError) Error() string      { return e.Msg }
func (e *ZstdError) PyTypeName() string { return "ZstdError" }

// OSError the bz2 decompressor raises for a damaged stream
type OSError struct{ Msg string }

func (e *OSError) Error() string      { return e.Msg }
func (e *OSError) PyTypeName() string { return "OSError" }

// KeyError of a name the archive does not hold
type KeyError struct{ Msg string }

func (e *KeyError) Error() string      { return pyrepr.StrRepr(e.Msg) }
func (e *KeyError) PyTypeName() string { return "KeyError" }

// Archive opened for reading with zipfile.ZipFile mode r semantics
type ZipReader struct {
	fp                 *os.File
	size               int64
	Filelist           []*ZipInfo
	NameToInfo         map[string]*ZipInfo
	StartDir           int64
	orig               map[*ZipInfo]string
	endOffset          map[*ZipInfo]int64
	IgnoreInvalidNames bool
}

// End of central directory fields with the record location
type endRecord struct {
	diskNumber, diskStart, entriesThisDisk, entriesTotal uint64
	size, offset, location                               int64
}

// Reports whether path holds an end of central directory record whose first entry has the right magic
func IsZipfile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	end, err := endRecData(f, st.Size())
	if err != nil || end == nil {
		return false
	}
	if end.entriesTotal == 0 && end.size == 0 && end.offset == 0 {
		return true
	}
	if end.diskNumber != end.diskStart {
		return false
	}
	if end.size < sizeCentralDir {
		return false
	}
	data := make([]byte, sizeCentralDir)
	n, _ := f.ReadAt(data, end.offset+(end.location-end.size-end.offset))
	return n == sizeCentralDir && bytes.Equal(data[:4], stringCentralDir)
}

// Reads exactly len(buf) bytes at off, the count read when the file is shorter
func readAt(f *os.File, buf []byte, off int64) int {
	if off < 0 {
		return 0
	}
	n, _ := f.ReadAt(buf, off)
	return n
}

// End of central directory record of the archive, nil when none is found
func endRecData(f *os.File, filesize int64) (*endRecord, error) {
	if filesize < sizeEndCentDir {
		return nil, nil
	}
	data := make([]byte, sizeEndCentDir)
	n := readAt(f, data, filesize-sizeEndCentDir)
	if n == sizeEndCentDir && bytes.Equal(data[:4], stringEndArchive) && data[20] == 0 && data[21] == 0 {
		end := unpackEndRecord(data, filesize-sizeEndCentDir)
		return endRecData64(f, filesize-sizeEndCentDir, end)
	}
	maxCommentStart := max(filesize-maxComment-sizeEndCentDir, 0)
	tail := make([]byte, min(filesize-maxCommentStart, maxComment+sizeEndCentDir))
	tail = tail[:readAt(f, tail, maxCommentStart)]
	start := bytes.LastIndex(tail, stringEndArchive)
	if start < 0 {
		return nil, nil
	}
	rec := tail[start:]
	if len(rec) < sizeEndCentDir {
		return nil, nil
	}
	end := unpackEndRecord(rec[:sizeEndCentDir], maxCommentStart+int64(start))
	return endRecData64(f, maxCommentStart+int64(start), end)
}

// Decodes the fixed fields of an end of central directory record
func unpackEndRecord(data []byte, location int64) *endRecord {
	le := binary.LittleEndian
	return &endRecord{
		diskNumber: uint64(le.Uint16(data[4:])), diskStart: uint64(le.Uint16(data[6:])),
		entriesThisDisk: uint64(le.Uint16(data[8:])), entriesTotal: uint64(le.Uint16(data[10:])),
		size: int64(le.Uint32(data[12:])), offset: int64(le.Uint32(data[16:])), location: location,
	}
}

// Updates the record from the zip64 end of archive records when a locator precedes it
func endRecData64(f *os.File, offset int64, end *endRecord) (*endRecord, error) {
	offset -= sizeEndCentDir64Locator
	if offset < 0 {
		return end, nil
	}
	data := make([]byte, sizeEndCentDir64Locator)
	if readAt(f, data, offset) != sizeEndCentDir64Locator {
		return nil, &OSError{"Unknown I/O error"}
	}
	if !bytes.Equal(data[:4], stringEndArchive64Locator) {
		return end, nil
	}
	le := binary.LittleEndian
	diskno, reloff, disks := le.Uint32(data[4:]), int64(le.Uint64(data[8:])), le.Uint32(data[16:])
	if diskno != 0 || disks > 1 {
		return nil, &BadZipFile{"zipfiles that span multiple disks are not supported"}
	}
	offset -= sizeEndCentDir64
	if reloff > offset {
		return nil, &BadZipFile{"Corrupt zip64 end of central directory locator"}
	}
	extrasz := offset - reloff
	rec := make([]byte, sizeEndCentDir64)
	if readAt(f, rec, reloff) != sizeEndCentDir64 {
		return nil, &OSError{"Unknown I/O error"}
	}
	if !bytes.HasPrefix(rec, stringEndArchive64) && reloff != offset {
		extrasz = 0
		if readAt(f, rec, offset) != sizeEndCentDir64 {
			return nil, &OSError{"Unknown I/O error"}
		}
	}
	if !bytes.HasPrefix(rec, stringEndArchive64) {
		return nil, &BadZipFile{"Zip64 end of central directory record not found"}
	}
	sz := int64(le.Uint64(rec[4:]))
	dirsize, diroffset := int64(le.Uint64(rec[40:])), int64(le.Uint64(rec[48:]))
	if diroffset+dirsize != reloff || sz+12 != sizeEndCentDir64+extrasz {
		return nil, &BadZipFile{"Corrupt zip64 end of central directory record"}
	}
	return &endRecord{
		diskNumber: uint64(le.Uint32(rec[16:])), diskStart: uint64(le.Uint32(rec[20:])),
		entriesThisDisk: le.Uint64(rec[24:]), entriesTotal: le.Uint64(rec[32:]),
		size: dirsize, offset: diroffset, location: offset - extrasz,
	}, nil
}

// Opens an archive and reads its table of contents as zipfile.ZipFile does
func OpenReader(path string) (*ZipReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &ZipReader{fp: f, NameToInfo: map[string]*ZipInfo{}, orig: map[*ZipInfo]string{}, endOffset: map[*ZipInfo]int64{}}
	if err := r.realGetContents(); err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

// Decodes a stored member name as UTF-8 under the flag and cp437 otherwise
func decodeName(raw []byte, flags uint16) (string, error) {
	if flags&maskUTFFilename != 0 {
		return pycodecs.Decode(raw, "utf-8")
	}
	return pycodecs.Decode(raw, "cp437")
}

// Parses the central directory into the member list and name table
func (r *ZipReader) realGetContents() error {
	st, err := r.fp.Stat()
	if err != nil {
		return &BadZipFile{"File is not a zip file"}
	}
	r.size = st.Size()
	end, err := endRecData(r.fp, r.size)
	if err != nil {
		var osErr *OSError
		if errors.As(err, &osErr) {
			return &BadZipFile{"File is not a zip file"}
		}
		return err
	}
	if end == nil {
		return &BadZipFile{"File is not a zip file"}
	}
	concat := end.location - end.size - end.offset
	r.StartDir = end.offset + concat
	if r.StartDir < 0 {
		return &BadZipFile{"Bad offset for central directory"}
	}
	data := make([]byte, max(min(end.size, r.size-r.StartDir), 0))
	data = data[:readAt(r.fp, data, r.StartDir)]
	le := binary.LittleEndian
	pos, total := 0, int64(0)
	read := func(n int) []byte {
		b := data[min(pos, len(data)):min(pos+n, len(data))]
		pos += n
		return b
	}
	for total < end.size {
		cd := read(sizeCentralDir)
		if len(cd) != sizeCentralDir {
			return &BadZipFile{"Truncated central directory"}
		}
		if !bytes.Equal(cd[:4], stringCentralDir) {
			return &BadZipFile{"Bad magic number for central directory"}
		}
		fnLen, exLen, cmLen := int(le.Uint16(cd[28:])), int(le.Uint16(cd[30:])), int(le.Uint16(cd[32:]))
		rawName := read(fnLen)
		flags := le.Uint16(cd[8:])
		name, err := decodeName(rawName, flags)
		if err != nil {
			return err
		}
		x := &ZipInfo{Filename: sanitizeFilename(name)}
		x.Extra = append([]byte(nil), read(exLen)...)
		x.Comment = append([]byte(nil), read(cmLen)...)
		x.CreateVersion, x.CreateSystem, x.ExtractVersion, x.Reserved = int(cd[4]), int(cd[5]), int(cd[6]), int(cd[7])
		x.FlagBits, x.CompressType = int(flags), int(le.Uint16(cd[10:]))
		t, d := int(le.Uint16(cd[12:])), int(le.Uint16(cd[14:]))
		x.CRC, x.CompressSize, x.FileSize = le.Uint32(cd[16:]), int64(le.Uint32(cd[20:])), int64(le.Uint32(cd[24:]))
		x.HeaderOffset = int64(le.Uint32(cd[42:]))
		if x.ExtractVersion > maxExtractVersion {
			return &NotImplementedError{fmt.Sprintf("zip file version %.1f", float64(x.ExtractVersion)/10)}
		}
		x.Volume, x.InternalAttr, x.ExternalAttr = int(le.Uint16(cd[34:])), int(le.Uint16(cd[36:])), le.Uint32(cd[38:])
		x.DateTime = [6]int{(d >> 9) + 1980, (d >> 5) & 0xF, d & 0x1F, t >> 11, (t >> 5) & 0x3F, (t & 0x1F) * 2}
		if err := decodeExtra(x, crc32.ChecksumIEEE(rawName)); err != nil {
			return err
		}
		x.HeaderOffset += concat
		r.Filelist = append(r.Filelist, x)
		r.NameToInfo[x.Filename] = x
		r.orig[x] = name
		total += int64(sizeCentralDir + fnLen + exLen + cmLen)
	}
	sorted := append([]*ZipInfo(nil), r.Filelist...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].HeaderOffset < sorted[j].HeaderOffset })
	endOffset := r.StartDir
	for i := len(sorted) - 1; i >= 0; i-- {
		r.endOffset[sorted[i]] = endOffset
		endOffset = sorted[i].HeaderOffset
	}
	return nil
}

// Applies the zip64 sizes and the unicode path name of the extra field
func decodeExtra(x *ZipInfo, filenameCRC uint32) error {
	extra := x.Extra
	le := binary.LittleEndian
	for len(extra) >= 4 {
		tp, ln := le.Uint16(extra), int(le.Uint16(extra[2:]))
		if ln+4 > len(extra) {
			return &BadZipFile{fmt.Sprintf("Corrupt extra field %04x (size=%d)", tp, ln)}
		}
		data := extra[4 : ln+4]
		switch tp {
		case 0x0001:
			field := ""
			take := func(name string) (int64, bool) {
				field = name
				if len(data) < 8 {
					return 0, false
				}
				v := int64(le.Uint64(data))
				data = data[8:]
				return v, true
			}
			ok := true
			if x.FileSize == -1 || x.FileSize == 0xFFFF_FFFF {
				x.FileSize, ok = take("File size")
			}
			if ok && x.CompressSize == 0xFFFF_FFFF {
				x.CompressSize, ok = take("Compress size")
			}
			if ok && x.HeaderOffset == 0xFFFF_FFFF {
				x.HeaderOffset, ok = take("Header offset")
			}
			if !ok {
				return &BadZipFile{"Corrupt zip64 extra field. " + field + " not found."}
			}
		case 0x7075:
			if len(data) < 5 {
				return &BadZipFile{"Corrupt unicode path extra field (0x7075)"}
			}
			if data[0] == 1 && le.Uint32(data[1:]) == filenameCRC {
				name, err := pycodecs.Decode(data[5:], "utf-8")
				if err != nil {
					return &BadZipFile{"Corrupt unicode path extra field (0x7075): invalid utf-8 bytes"}
				}
				if name != "" {
					x.Filename = sanitizeFilename(name)
				}
			}
		}
		extra = extra[ln+4:]
	}
	return nil
}

// Closes the archive file
func (r *ZipReader) Close() error { return r.fp.Close() }

// Member names in central directory order
func (r *ZipReader) Namelist() []string {
	names := make([]string, len(r.Filelist))
	for i, x := range r.Filelist {
		names[i] = x.Filename
	}
	return names
}

// Member of a name, the last one when the archive repeats it
func (r *ZipReader) GetInfo(name string) (*ZipInfo, error) {
	x, ok := r.NameToInfo[name]
	if !ok {
		return nil, &KeyError{fmt.Sprintf("There is no item named %s in the archive", pyrepr.StrRepr(name))}
	}
	return x, nil
}

// Opens a member for reading, checking its local header like zipfile.ZipFile.open
func (r *ZipReader) Open(name string) (io.ReadCloser, error) {
	x, err := r.GetInfo(name)
	if err != nil {
		return nil, err
	}
	return r.openInfo(x, pyrepr.StrRepr(name))
}

// Member stream of zipfile.ZipFile.open, label being the repr the encrypted member error shows
func (r *ZipReader) openInfo(x *ZipInfo, label string) (pyFile, error) {
	header := make([]byte, sizeFileHeader)
	if readAt(r.fp, header, x.HeaderOffset) != sizeFileHeader {
		return nil, &BadZipFile{"Truncated file header"}
	}
	if !bytes.Equal(header[:4], stringFileHeader) {
		return nil, &BadZipFile{"Bad magic number for file header"}
	}
	le := binary.LittleEndian
	fnLen, exLen := int(le.Uint16(header[26:])), int64(le.Uint16(header[28:]))
	fname := make([]byte, fnLen)
	fname = fname[:readAt(r.fp, fname, x.HeaderOffset+sizeFileHeader)]
	if x.FlagBits&maskCompressedPatch != 0 {
		return nil, &NotImplementedError{"compressed patched data (flag bit 5)"}
	}
	if x.FlagBits&maskStrongEncryption != 0 {
		return nil, &NotImplementedError{"strong encryption (flag bit 6)"}
	}
	fnameStr, err := decodeName(fname, le.Uint16(header[6:]))
	if err != nil {
		return nil, err
	}
	if fnameStr != r.orig[x] {
		return nil, &BadZipFile{fmt.Sprintf("File name in directory %s and header %s differ.", pyrepr.StrRepr(r.orig[x]), pyrepr.BytesRepr(fname))}
	}
	dataStart := x.HeaderOffset + sizeFileHeader + int64(fnLen) + exLen
	if end, ok := r.endOffset[x]; ok && dataStart+x.CompressSize > end && end != x.HeaderOffset {
		return nil, &BadZipFile{"Overlapped entries: " + pyrepr.StrRepr(r.orig[x]) + " (possible zip bomb)"}
	}
	if x.FlagBits&maskEncrypted != 0 {
		return nil, &RuntimeError{fmt.Sprintf("File %s is encrypted, password required for extraction", label)}
	}
	if err := checkReadCompression(x.CompressType); err != nil {
		return nil, err
	}
	switch x.CompressType {
	case Bzip2:
		return newExtFile(r.fp, dataStart, x, newBZ2Decompressor()), nil
	case Zstandard:
		return newExtFile(r.fp, dataStart, x, newZstdDecompressor()), nil
	}
	m := &memberFile{name: x.Filename, method: x.CompressType, left: x.FileSize, expected: x.CRC,
		open: func() *compressedSource { return &compressedSource{fp: r.fp, pos: dataStart, left: x.CompressSize} }}
	m.src = m.open()
	return m, nil
}

// Rejects a compression method zipfile has no decompressor for
func checkReadCompression(method int) error {
	switch method {
	case Stored, Deflated, Bzip2, LZMA, Zstandard:
		return nil
	}
	return &NotImplementedError{"That compression method is not supported"}
}

// Compressed bytes of one member, io.EOF once its size is consumed and EOFError when the file ends first
type compressedSource struct {
	fp   *os.File
	pos  int64
	left int64
}

func (s *compressedSource) Read(p []byte) (int, error) {
	if s.left <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	n = min(max(n, minReadSize), s.left, int64(len(p)))
	got, err := s.fp.ReadAt(p[:n], s.pos)
	s.pos += int64(got)
	s.left -= int64(got)
	if got == 0 {
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return 0, &EOFError{}
	}
	return got, nil
}

func (s *compressedSource) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := s.Read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

// Decompressed stream of one member with the zipfile CRC check at its end
type memberFile struct {
	name      string
	method    int
	src       *compressedSource
	open      func() *compressedSource
	dec       io.Reader
	closer    func()
	left      int64
	eof       bool
	crc       uint32
	expected  uint32
	delivered int64
	slow      bool
	lzmaHead  []byte
	closed    bool
}

// Builds the decompressor of the member on first read
func (m *memberFile) decoder() (io.Reader, error) {
	if m.dec != nil || m.method == Stored {
		return m.dec, nil
	}
	switch m.method {
	case Deflated:
		m.dec = flate.NewReader(m.src)
	case LZMA:
		for len(m.lzmaHead) <= 4 || len(m.lzmaHead) <= 4+int(binary.LittleEndian.Uint16(m.lzmaHead[2:])) {
			b, err := m.src.ReadByte()
			if err != nil {
				return nil, err
			}
			m.lzmaHead = append(m.lzmaHead, b)
		}
		psize := int(binary.LittleEndian.Uint16(m.lzmaHead[2:]))
		props := m.lzmaHead[4 : 4+psize]
		if psize != 5 {
			return nil, &LZMAError{"Invalid or unsupported options"}
		}
		header := append(append([]byte{}, props...), 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
		rest := m.lzmaHead[4+psize:]
		d, err := lzma.NewReader(io.MultiReader(bytes.NewReader(header), bytes.NewReader(rest), m.src))
		if err != nil {
			var eof *EOFError
			if errors.As(err, &eof) {
				return nil, err
			}
			return nil, &LZMAError{"Invalid or unsupported options"}
		}
		m.dec = d
	}
	return m.dec, nil
}

// Python exception a decoder failure of the method surfaces as
func (m *memberFile) decodeError(err error) error {
	var eof *EOFError
	if errors.As(err, &eof) {
		return err
	}
	if pe, ok := err.(*os.PathError); ok {
		return pe
	}
	if m.method == LZMA {
		return &LZMAError{"Corrupt input data"}
	}
	return err
}

// Reports whether a decoder error only means its compressed input ran out
func inputExhausted(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func (m *memberFile) Read(p []byte) (int, error) {
	if m.closed {
		return 0, &ValueError{"read from closed file."}
	}
	if len(p) == 0 {
		return 0, nil
	}
	for !m.eof {
		n, err := m.read1(p)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, io.EOF
}

// One decode step, the CRC checked once the member ends
func (m *memberFile) read1(p []byte) (int, error) {
	if m.method == Stored {
		n, err := m.src.Read(p)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		m.eof = m.src.left <= 0
		return m.finish(p, n)
	}
	dec, err := m.decoder()
	if err != nil {
		if inputExhausted(err) {
			m.eof = true
			return m.finish(p, 0)
		}
		return 0, m.decodeError(err)
	}
	n, err := dec.Read(p)
	var corrupt flate.CorruptInputError
	if errors.As(err, &corrupt) && !m.slow {
		return m.switchToZlib(p)
	}
	m.delivered += int64(n)
	if err == nil && int64(n) >= m.left {
		err = m.drain(dec)
	}
	if err != nil {
		if !inputExhausted(err) {
			return 0, m.decodeError(err)
		}
		m.eof = true
	}
	return m.finish(p, n)
}

// Lets the decoder finish the block holding the last member byte, surfacing the check that block fails
func (m *memberFile) drain(dec io.Reader) error {
	var scratch [minReadSize]byte
	_, err := dec.Read(scratch[:])
	var corrupt flate.CorruptInputError
	if errors.As(err, &corrupt) && !m.slow {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(inflateRaw(m.open(), pw)) }()
		defer pr.Close()
		if _, err := io.Copy(io.Discard, pr); err != nil {
			return err
		}
		return nil
	}
	return err
}

// Truncates the step to the member size, folds it into the CRC and checks it at the end
func (m *memberFile) finish(p []byte, n int) (int, error) {
	if int64(n) > m.left {
		n = int(max(m.left, 0))
	}
	m.left -= int64(n)
	if m.left <= 0 {
		m.eof = true
	}
	m.crc = crc32.Update(m.crc, crc32.IEEETable, p[:n])
	if m.eof && m.crc != m.expected {
		return 0, &BadZipFile{"Bad CRC-32 for file " + pyrepr.StrRepr(m.name)}
	}
	return n, nil
}

// Restarts a deflate member on the zlib faithful decoder after the fast decoder rejected its data
func (m *memberFile) switchToZlib(p []byte) (int, error) {
	m.slow = true
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(inflateRaw(m.open(), pw)) }()
	m.dec = pr
	m.closer = func() { pr.Close() }
	if _, err := io.CopyN(io.Discard, pr, m.delivered); err != nil {
		if inputExhausted(err) {
			m.eof = true
			return m.finish(p, 0)
		}
		return 0, err
	}
	return m.read1(p)
}

// ZipExtFile.read of n bytes, everything up to the end when n is negative
func (m *memberFile) read(n int) ([]byte, error) {
	if n < 0 {
		data, err := io.ReadAll(m)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(m, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:got], nil
}

// shutil.copyfileobj of the member into w
func (m *memberFile) WriteTo(w io.Writer) (int64, error) { return copyFileObj(m.read, w) }

// Releases the decoder of the member
func (m *memberFile) Close() error {
	m.closed = true
	if m.closer != nil {
		m.closer()
		m.closer = nil
	}
	return nil
}
