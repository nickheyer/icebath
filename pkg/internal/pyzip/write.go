package pyzip

import (
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	Stored         = 0
	Deflated       = 8
	Zip64Limit     = 1<<31 - 1
	FileCountLimit = 1<<16 - 1

	defaultVersion         = 20
	zip64Version           = 45
	maskUseDataDescriptor  = 1 << 3
	utfFilenameFlag        = 1 << 11
	ddSignature            = 0x08074b50
	endCentDir64Size       = 56
	defaultCompressLevel   = -1
	copyChunk              = 8 * 1024
	fileHeaderSig          = "PK\x03\x04"
	centralDirSig          = "PK\x01\x02"
	endArchiveSig          = "PK\x05\x06"
	endArchive64Sig        = "PK\x06\x06"
	endArchive64LocSig     = "PK\x06\x07"
	errClosedWriteAttempt  = "Attempt to write to ZIP archive that was already closed"
	errClosedArchiveAccess = "Attempt to write ZIP archive that was already closed"
)

// ValueError zipfile raises for a refused argument or state
type ValueError struct{ Msg string }

func (e *ValueError) Error() string      { return e.Msg }
func (e *ValueError) PyTypeName() string { return "ValueError" }

// RuntimeError zipfile raises when a member outgrows its header format
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string      { return e.Msg }
func (e *RuntimeError) PyTypeName() string { return "RuntimeError" }

// struct.error raised when a header field does not fit its packed width
type StructError struct{ Msg string }

func (e *StructError) Error() string      { return e.Msg }
func (e *StructError) PyTypeName() string { return "error" }

// NotImplementedError for a compression method this writer has no compressor for
type NotImplementedError struct{ Msg string }

func (e *NotImplementedError) Error() string      { return e.Msg }
func (e *NotImplementedError) PyTypeName() string { return "NotImplementedError" }

// zlib.error raised for a compression level zlib refuses
type ZlibError struct{ Msg string }

func (e *ZlibError) Error() string      { return e.Msg }
func (e *ZlibError) PyTypeName() string { return "error" }

// IndexError of an arcname that normalizes to nothing
type IndexError struct{ Msg string }

func (e *IndexError) Error() string      { return e.Msg }
func (e *IndexError) PyTypeName() string { return "IndexError" }

// AttributeError of a seek on a stream that only counts its position
type AttributeError struct{ Msg string }

func (e *AttributeError) Error() string      { return e.Msg }
func (e *AttributeError) PyTypeName() string { return "AttributeError" }

// UnicodeEncodeError of a file name holding undecodable bytes, a ValueError subclass in Python
type UnicodeEncodeError struct {
	Start, End int
	Char       rune
}

func (e *UnicodeEncodeError) Error() string {
	if e.End-e.Start == 1 {
		return fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", e.Char, e.Start)
	}
	return fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", e.Start, e.End-1)
}
func (e *UnicodeEncodeError) PyTypeName() string { return "UnicodeEncodeError" }

// Reports whether err is a ValueError or one of its subclasses
func IsValueError(err error) bool {
	switch err.(type) {
	case *ValueError, *UnicodeEncodeError:
		return true
	}
	return false
}

// Attributes of one archive member, the zipfile.ZipInfo fields the writer packs
type ZipInfo struct {
	Filename       string
	DateTime       [6]int
	CompressType   int
	CompressLevel  *int
	Comment        []byte
	Extra          []byte
	CreateSystem   int
	CreateVersion  int
	ExtractVersion int
	Reserved       int
	FlagBits       int
	Volume         int
	InternalAttr   int
	ExternalAttr   uint32
	HeaderOffset   int64
	CRC            uint32
	CompressSize   int64
	FileSize       int64
}

// Name cut at the first NUL with host separators turned into forward slashes
func sanitizeFilename(name string) string {
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	if runtime.GOOS == "windows" {
		name = strings.ReplaceAll(name, `\`, "/")
	}
	return name
}

// Member description with the zipfile defaults, refusing a date before 1980
func NewZipInfo(filename string, dateTime [6]int) (*ZipInfo, error) {
	if dateTime[0] < 1980 {
		return nil, &ValueError{"ZIP does not support timestamps before 1980"}
	}
	system := 3
	if runtime.GOOS == "windows" {
		system = 0
	}
	return &ZipInfo{
		Filename: sanitizeFilename(filename), DateTime: dateTime, CompressType: Stored,
		CreateSystem: system, CreateVersion: defaultVersion, ExtractVersion: defaultVersion,
	}, nil
}

// Default member date of 1980-01-01 midnight
var defaultDateTime = [6]int{1980, 1, 1, 0, 0, 0}

// Member description of a file on disk under arcname, its local mtime, mode bits and size
func ZipInfoFromFile(filename, arcname string, strictTimestamps bool) (*ZipInfo, error) {
	st, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	isdir := st.IsDir()
	m := st.ModTime().Local()
	dt := [6]int{m.Year(), int(m.Month()), m.Day(), m.Hour(), m.Minute(), m.Second()}
	if !strictTimestamps && dt[0] < 1980 {
		dt = defaultDateTime
	} else if !strictTimestamps && dt[0] > 2107 {
		dt = [6]int{2107, 12, 31, 23, 59, 59}
	}
	if arcname == "" {
		arcname = filename
	}
	if runtime.GOOS == "windows" {
		_, arcname = pypath.NtSplitdrive(arcname)
	}
	arcname = pypath.Normpath(arcname)
	for {
		if arcname == "" {
			return nil, &IndexError{"string index out of range"}
		}
		if c := arcname[0]; c != os.PathSeparator && !(runtime.GOOS == "windows" && c == '/') {
			break
		}
		arcname = arcname[1:]
	}
	if isdir {
		arcname += "/"
	}
	zinfo, err := NewZipInfo(arcname, dt)
	if err != nil {
		return nil, err
	}
	zinfo.ExternalAttr = (statMode(st) & 0xFFFF) << 16
	if isdir {
		zinfo.FileSize = 0
		zinfo.ExternalAttr |= 0x10
	} else {
		zinfo.FileSize = st.Size()
	}
	return zinfo, nil
}

// Reports whether the member names a directory
func (z *ZipInfo) IsDir() bool { return strings.HasSuffix(z.Filename, "/") }

// DOS date and time words of a member timestamp
func dosDateTime(dt [6]int) (dosdate, dostime int) {
	return (dt[0]-1980)<<9 | dt[1]<<5 | dt[2], dt[3]<<11 | dt[4]<<5 | dt[5]/2
}

// Encoded name and flag bits, cp437 or ascii first and utf-8 with the unicode flag otherwise
func (z *ZipInfo) encodeFilenameFlags() ([]byte, int, error) {
	if z.FlagBits&utfFilenameFlag != 0 {
		if isASCII(z.Filename) {
			return []byte(z.Filename), z.FlagBits &^ utfFilenameFlag, nil
		}
	} else if utf8.ValidString(z.Filename) {
		if b, err := charmap.CodePage437.NewEncoder().String(z.Filename); err == nil {
			return []byte(b), z.FlagBits &^ utfFilenameFlag, nil
		}
	}
	if err := surrogateError(z.Filename); err != nil {
		return nil, 0, err
	}
	return []byte(z.Filename), z.FlagBits | utfFilenameFlag, nil
}

// Reports whether every byte is seven bit
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Encode error of the first run of undecodable bytes, which Python holds as lone surrogates
func surrogateError(s string) error {
	pos := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			start, first := pos, rune(0xDC00+int(s[i]))
			for i < len(s) {
				if r, size := utf8.DecodeRuneInString(s[i:]); r != utf8.RuneError || size != 1 {
					break
				}
				i++
				pos++
			}
			return &UnicodeEncodeError{Start: start, End: pos, Char: first}
		}
		i += size
		pos++
	}
	return nil
}

// Little endian packer that fails like struct.pack on a value outside its field width
type packer struct {
	buf []byte
	err error
}

func (p *packer) bytes(b []byte) { p.buf = append(p.buf, b...) }

func (p *packer) u8(v int) {
	if v < 0 || v > 0xFF {
		p.fail("'B' format requires 0 <= number <= 255")
		return
	}
	p.buf = append(p.buf, byte(v))
}

func (p *packer) u16(v int64) {
	if v < 0 || v > 0xFFFF {
		p.fail("'H' format requires 0 <= number <= 65535")
		return
	}
	p.buf = binary.LittleEndian.AppendUint16(p.buf, uint16(v))
}

func (p *packer) u32(v int64) {
	if v < 0 || v > 0xFFFFFFFF {
		p.fail("'L' format requires 0 <= number <= 4294967295")
		return
	}
	p.buf = binary.LittleEndian.AppendUint32(p.buf, uint32(v))
}

func (p *packer) u64(v int64) {
	if v < 0 {
		p.fail("'Q' format requires 0 <= number <= 18446744073709551615")
		return
	}
	p.buf = binary.LittleEndian.AppendUint64(p.buf, uint64(v))
}

// Records the first packing failure
func (p *packer) fail(msg string) {
	if p.err == nil {
		p.err = &StructError{msg}
	}
}

// Local file header bytes, a ZIP64 extra carrying the sizes when zip64 is set
func (z *ZipInfo) FileHeader(zip64 bool) ([]byte, error) {
	dosdate, dostime := dosDateTime(z.DateTime)
	crc, compressSize, fileSize := int64(z.CRC), z.CompressSize, z.FileSize
	if z.FlagBits&maskUseDataDescriptor != 0 {
		crc, compressSize, fileSize = 0, 0, 0
	}
	extra := append([]byte{}, z.Extra...)
	minVersion := 0
	if zip64 {
		x := &packer{}
		x.u16(1)
		x.u16(16)
		x.u64(fileSize)
		x.u64(compressSize)
		if x.err != nil {
			return nil, x.err
		}
		extra = append(extra, x.buf...)
		fileSize, compressSize = 0xffffffff, 0xffffffff
		minVersion = zip64Version
	}
	z.ExtractVersion = max(minVersion, z.ExtractVersion)
	z.CreateVersion = max(minVersion, z.CreateVersion)
	filename, flagBits, err := z.encodeFilenameFlags()
	if err != nil {
		return nil, err
	}
	p := &packer{}
	p.bytes([]byte(fileHeaderSig))
	p.u8(z.ExtractVersion)
	p.u8(z.Reserved)
	p.u16(int64(flagBits))
	p.u16(int64(z.CompressType))
	p.u16(int64(dostime))
	p.u16(int64(dosdate))
	p.u32(crc)
	p.u32(compressSize)
	p.u32(fileSize)
	p.u16(int64(len(filename)))
	p.u16(int64(len(extra)))
	if p.err != nil {
		return nil, p.err
	}
	return append(append(p.buf, filename...), extra...), nil
}

// Extra field data without the records of the given header ids
func stripExtra(data []byte, ids map[int]bool) []byte {
	var out []byte
	i := 0
	for i+4 <= len(data) {
		xid := int(binary.LittleEndian.Uint16(data[i:]))
		xlen := int(binary.LittleEndian.Uint16(data[i+2:]))
		j := i + 4 + xlen
		if !ids[xid] {
			out = append(out, data[i:min(j, len(data))]...)
		}
		i = j
	}
	if i < len(data) {
		out = append(out, data[i:]...)
	}
	return out
}

// Archive opened for writing with zipfile.ZipFile mode w semantics over one file
type ZipFile struct {
	Fp            *os.File
	Filelist      []*ZipInfo
	NameToInfo    map[string]*ZipInfo
	StartDir      int64
	compression   int
	compresslevel *int
	strict        bool
	seekable      bool
	tellPos       int64
	writing       bool
	didModify     bool
	comment       []byte
	closed        bool
}

// Rejects a compression method the writer has no compressor for
func checkCompression(method int) error {
	if method == Stored || method == Deflated {
		return nil
	}
	return &NotImplementedError{"That compression method is not supported"}
}

// New archive at path, truncating it, opened read write and then write only when that open fails
func Create(path string, compression int, compresslevel *int) (*ZipFile, error) {
	if err := checkCompression(compression); err != nil {
		return nil, err
	}
	fp, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		if fp, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
			return nil, err
		}
	}
	z := &ZipFile{Fp: fp, NameToInfo: map[string]*ZipInfo{}, compression: compression,
		compresslevel: compresslevel, strict: true, seekable: true, didModify: true}
	if pos, err := fp.Seek(0, io.SeekCurrent); err != nil {
		z.seekable = false
	} else {
		z.StartDir = pos
		if _, err := fp.Seek(z.StartDir, io.SeekStart); err != nil {
			z.seekable = false
		}
	}
	return z, nil
}

// Reports whether the archive file supports seeking back to rewrite headers
func (z *ZipFile) Seekable() bool { return z.seekable }

// Archive position, the counted bytes when the file cannot seek
func (z *ZipFile) tell() (int64, error) {
	if !z.seekable {
		return z.tellPos, nil
	}
	return z.Fp.Seek(0, io.SeekCurrent)
}

// Moves to an absolute position, refused on a counting stream as Python's missing seek attribute
func (z *ZipFile) seek(pos int64) error {
	if !z.seekable {
		return &AttributeError{"'_Tellable' object has no attribute 'seek'"}
	}
	_, err := z.Fp.Seek(pos, io.SeekStart)
	return err
}

// Moves to pos and cuts the archive file there
func (z *ZipFile) TruncateAt(pos int64) error {
	if err := z.seek(pos); err != nil {
		return err
	}
	return z.Fp.Truncate(pos)
}

// Writes bytes at the current position, counting them on a stream that cannot seek
func (z *ZipFile) write(b []byte) error {
	n, err := z.Fp.Write(b)
	z.tellPos += int64(n)
	return err
}

// Duplicate warning and state checks run before any member bytes are written
func (z *ZipFile) writecheck(zinfo *ZipInfo) error {
	if _, dup := z.NameToInfo[zinfo.Filename]; dup {
		fmt.Fprintf(os.Stderr, "UserWarning: Duplicate name: %s\n", pyrepr.StrRepr(zinfo.Filename))
	}
	if z.closed {
		return &ValueError{errClosedArchiveAccess}
	}
	return checkCompression(zinfo.CompressType)
}

// Adds a file or directory from disk under arcname, a failed read still finalizing the partial member
func (z *ZipFile) Write(filename, arcname string) error {
	if z.closed {
		return &ValueError{errClosedWriteAttempt}
	}
	if z.writing {
		return &ValueError{"Can't write to ZIP archive while an open writing handle exists"}
	}
	zinfo, err := ZipInfoFromFile(filename, arcname, z.strict)
	if err != nil {
		return err
	}
	if zinfo.IsDir() {
		zinfo.CompressSize, zinfo.CRC = 0, 0
		return z.mkdir(zinfo)
	}
	zinfo.CompressType = z.compression
	zinfo.CompressLevel = z.compresslevel
	src, err := os.Open(filename)
	if err != nil {
		return err
	}
	dest, err := z.openToWrite(zinfo)
	if err != nil {
		src.Close()
		return err
	}
	copyErr := copyChunks(dest, src)
	closeErr := dest.Close()
	srcErr := src.Close()
	switch {
	case closeErr != nil:
		return closeErr
	case copyErr != nil:
		return copyErr
	}
	return srcErr
}

// shutil.copyfileobj with an 8 KiB buffer
func copyChunks(dst io.Writer, src io.Reader) error {
	buf := make([]byte, copyChunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Handle writing a new member named name with the archive compression and the 1980 default date
func (z *ZipFile) Open(name string) (*WriteFile, error) {
	if z.closed {
		return nil, &ValueError{"Attempt to use ZIP archive that was already closed"}
	}
	zinfo, err := NewZipInfo(name, defaultDateTime)
	if err != nil {
		return nil, err
	}
	zinfo.CompressType = z.compression
	zinfo.CompressLevel = z.compresslevel
	return z.openToWrite(zinfo)
}

// Writes the provisional local header and returns the member handle
func (z *ZipFile) openToWrite(zinfo *ZipInfo) (*WriteFile, error) {
	if z.writing {
		return nil, &ValueError{"Can't write to the ZIP file while there is another write handle open on it. " +
			"Close the first handle before opening another."}
	}
	zinfo.CompressSize, zinfo.CRC = 0, 0
	zinfo.FlagBits = utfFilenameFlag
	if !z.seekable {
		zinfo.FlagBits |= maskUseDataDescriptor
	}
	if zinfo.ExternalAttr == 0 {
		zinfo.ExternalAttr = 0o600 << 16
	}
	zip64 := float64(zinfo.FileSize)*1.05 > Zip64Limit
	if z.seekable {
		if err := z.seek(z.StartDir); err != nil {
			return nil, err
		}
	}
	pos, err := z.tell()
	if err != nil {
		return nil, err
	}
	zinfo.HeaderOffset = pos
	if err := z.writecheck(zinfo); err != nil {
		return nil, err
	}
	z.didModify = true
	header, err := zinfo.FileHeader(zip64)
	if err != nil {
		return nil, err
	}
	if err := z.write(header); err != nil {
		return nil, err
	}
	z.writing = true
	w := &WriteFile{zf: z, zinfo: zinfo, zip64: zip64}
	if zinfo.CompressType == Deflated {
		level := defaultCompressLevel
		if zinfo.CompressLevel != nil {
			level = *zinfo.CompressLevel
		}
		fw, err := flate.NewWriter(&countingWriter{w: w}, level)
		if err != nil {
			return nil, &ZlibError{"Bad compression level"}
		}
		w.compressor = fw
	}
	return w, nil
}

// Passes compressor output to the archive, counting the compressed size
type countingWriter struct{ w *WriteFile }

func (c *countingWriter) Write(b []byte) (int, error) {
	c.w.compressSize += int64(len(b))
	if err := c.w.zf.write(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Open member of a ZipFile, finalized into the central directory by Close
type WriteFile struct {
	zf           *ZipFile
	zinfo        *ZipInfo
	zip64        bool
	compressor   *flate.Writer
	fileSize     int64
	compressSize int64
	crc          uint32
	closed       bool
}

// Feeds member data through the CRC and the compressor into the archive
func (w *WriteFile) Write(data []byte) (int, error) {
	if w.closed {
		return 0, &ValueError{"I/O operation on closed file."}
	}
	w.fileSize += int64(len(data))
	w.crc = crc32.Update(w.crc, crc32.IEEETable, data)
	if w.compressor != nil {
		if _, err := w.compressor.Write(data); err != nil {
			return 0, err
		}
		return len(data), nil
	}
	w.compressSize += int64(len(data))
	if err := w.zf.write(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

// Flushes the compressor, rewrites the header with the final sizes and records the member
func (w *WriteFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer func() { w.zf.writing = false }()
	if w.compressor != nil {
		if err := w.compressor.Close(); err != nil {
			return err
		}
	}
	w.zinfo.CompressSize = w.compressSize
	w.zinfo.CRC = w.crc
	w.zinfo.FileSize = w.fileSize
	if !w.zip64 {
		if w.fileSize > Zip64Limit {
			return &RuntimeError{"File size too large, try using force_zip64"}
		}
		if w.compressSize > Zip64Limit {
			return &RuntimeError{"Compressed size too large, try using force_zip64"}
		}
	}
	if w.zinfo.FlagBits&maskUseDataDescriptor != 0 {
		p := &packer{}
		p.u32(ddSignature)
		p.u32(int64(w.zinfo.CRC))
		if w.zip64 {
			p.u64(w.zinfo.CompressSize)
			p.u64(w.zinfo.FileSize)
		} else {
			p.u32(w.zinfo.CompressSize)
			p.u32(w.zinfo.FileSize)
		}
		if p.err != nil {
			return p.err
		}
		if err := w.zf.write(p.buf); err != nil {
			return err
		}
		pos, err := w.zf.tell()
		if err != nil {
			return err
		}
		w.zf.StartDir = pos
	} else {
		pos, err := w.zf.tell()
		if err != nil {
			return err
		}
		w.zf.StartDir = pos
		if err := w.zf.seek(w.zinfo.HeaderOffset); err != nil {
			return err
		}
		header, err := w.zinfo.FileHeader(w.zip64)
		if err != nil {
			return err
		}
		if err := w.zf.write(header); err != nil {
			return err
		}
		if err := w.zf.seek(w.zf.StartDir); err != nil {
			return err
		}
	}
	w.zf.Filelist = append(w.zf.Filelist, w.zinfo)
	w.zf.NameToInfo[w.zinfo.Filename] = w.zinfo
	return nil
}

// Records a directory member with its header and no data
func (z *ZipFile) mkdir(zinfo *ZipInfo) error {
	if !zinfo.IsDir() {
		return &ValueError{"The given ZipInfo does not describe a directory"}
	}
	if z.seekable {
		if err := z.seek(z.StartDir); err != nil {
			return err
		}
	}
	pos, err := z.tell()
	if err != nil {
		return err
	}
	zinfo.HeaderOffset = pos
	if err := z.writecheck(zinfo); err != nil {
		return err
	}
	z.didModify = true
	z.Filelist = append(z.Filelist, zinfo)
	z.NameToInfo[zinfo.Filename] = zinfo
	header, err := zinfo.FileHeader(false)
	if err != nil {
		return err
	}
	if err := z.write(header); err != nil {
		return err
	}
	pos, err = z.tell()
	if err != nil {
		return err
	}
	z.StartDir = pos
	return nil
}

// Writes the central directory and end records then closes the file, a close failure winning
func (z *ZipFile) Close() error {
	if z.closed {
		return nil
	}
	if z.writing {
		return &ValueError{"Can't close the ZIP file while there is an open writing handle on it. " +
			"Close the writing handle before closing the zip."}
	}
	var recErr error
	if z.didModify {
		if z.seekable {
			recErr = z.seek(z.StartDir)
		}
		if recErr == nil {
			recErr = z.writeEndRecord()
		}
	}
	z.closed = true
	if err := z.Fp.Close(); err != nil {
		return err
	}
	return recErr
}

// Central directory entries, ZIP64 end records when needed, and the end of archive record
func (z *ZipFile) writeEndRecord() error {
	for _, zinfo := range z.Filelist {
		dosdate, dostime := dosDateTime(zinfo.DateTime)
		var extra []int64
		fileSize, compressSize := zinfo.FileSize, zinfo.CompressSize
		if zinfo.FileSize > Zip64Limit || zinfo.CompressSize > Zip64Limit {
			extra = append(extra, zinfo.FileSize, zinfo.CompressSize)
			fileSize, compressSize = 0xffffffff, 0xffffffff
		}
		headerOffset := zinfo.HeaderOffset
		if zinfo.HeaderOffset > Zip64Limit {
			extra = append(extra, zinfo.HeaderOffset)
			headerOffset = 0xffffffff
		}
		extraData := zinfo.Extra
		minVersion := 0
		if len(extra) > 0 {
			x := &packer{}
			x.u16(1)
			x.u16(int64(8 * len(extra)))
			for _, v := range extra {
				x.u64(v)
			}
			if x.err != nil {
				return x.err
			}
			extraData = append(x.buf, stripExtra(extraData, map[int]bool{1: true})...)
			minVersion = zip64Version
		}
		extractVersion := max(minVersion, zinfo.ExtractVersion)
		createVersion := max(minVersion, zinfo.CreateVersion)
		filename, flagBits, err := zinfo.encodeFilenameFlags()
		if err != nil {
			return err
		}
		p := &packer{}
		p.bytes([]byte(centralDirSig))
		p.u8(createVersion)
		p.u8(zinfo.CreateSystem)
		p.u8(extractVersion)
		p.u8(zinfo.Reserved)
		p.u16(int64(flagBits))
		p.u16(int64(zinfo.CompressType))
		p.u16(int64(dostime))
		p.u16(int64(dosdate))
		p.u32(int64(zinfo.CRC))
		p.u32(compressSize)
		p.u32(fileSize)
		p.u16(int64(len(filename)))
		p.u16(int64(len(extraData)))
		p.u16(int64(len(zinfo.Comment)))
		p.u16(0)
		p.u16(int64(zinfo.InternalAttr))
		p.u32(int64(zinfo.ExternalAttr))
		p.u32(headerOffset)
		if p.err != nil {
			return p.err
		}
		for _, b := range [][]byte{p.buf, filename, extraData, zinfo.Comment} {
			if err := z.write(b); err != nil {
				return err
			}
		}
	}
	pos2, err := z.tell()
	if err != nil {
		return err
	}
	count := int64(len(z.Filelist))
	size := pos2 - z.StartDir
	offset := z.StartDir
	if count > FileCountLimit || offset > Zip64Limit || size > Zip64Limit {
		p := &packer{}
		p.bytes([]byte(endArchive64Sig))
		p.u64(endCentDir64Size - 12)
		p.u16(45)
		p.u16(45)
		p.u32(0)
		p.u32(0)
		p.u64(count)
		p.u64(count)
		p.u64(size)
		p.u64(offset)
		p.bytes([]byte(endArchive64LocSig))
		p.u32(0)
		p.u64(pos2)
		p.u32(1)
		if p.err != nil {
			return p.err
		}
		if err := z.write(p.buf); err != nil {
			return err
		}
		count, size, offset = min(count, 0xFFFF), min(size, 0xFFFFFFFF), min(offset, 0xFFFFFFFF)
	}
	p := &packer{}
	p.bytes([]byte(endArchiveSig))
	p.u16(0)
	p.u16(0)
	p.u16(count)
	p.u16(count)
	p.u32(size)
	p.u32(offset)
	p.u16(int64(len(z.comment)))
	if p.err != nil {
		return p.err
	}
	if err := z.write(append(p.buf, z.comment...)); err != nil {
		return err
	}
	return nil
}
