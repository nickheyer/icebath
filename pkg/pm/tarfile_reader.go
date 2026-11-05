package pm

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// Block size and member type flags as tarfile names them
const (
	tarBlockSize      = 512
	tarRegType        = '0'
	tarARegType       = 0
	tarLnkType        = '1'
	tarSymType        = '2'
	tarChrType        = '3'
	tarBlkType        = '4'
	tarDirType        = '5'
	tarFifoType       = '6'
	tarContType       = '7'
	tarGNULongName    = 'L'
	tarGNULongLink    = 'K'
	tarGNUSparse      = 'S'
	tarXHDType        = 'x'
	tarXGLType        = 'g'
	tarSolarisXHDType = 'X'
)

// Chunk tarfile reads an extended header in and the buffer copyfileobj copies member data with
const (
	tarExtHeaderChunk = 1 << 20
	tarCopyBufSize    = 16 * 1024
)

// Python exception tarfile raises while reading or extracting, its class name and message
type TarReadError struct {
	Class   string
	Message string
}

func (e *TarReadError) Error() string { return e.Message }

// Header failure one member parse raises, its HeaderError subclass and message
type tarHeaderError struct {
	class string
	msg   string
}

func (e *tarHeaderError) Error() string { return e.msg }

// InvalidHeaderError with the message tarfile gives most header faults
func tarInvalidHeader() error {
	return &tarHeaderError{class: "InvalidHeaderError", msg: "invalid header"}
}

// ReadError tarfile raises with the given message
func tarReadError(msg string) error { return &TarReadError{Class: "ReadError", Message: msg} }

// Message an EOFError carries when a compressed stream stops before its end marker
const tarCompressedEOF = "Compressed file ended before the end-of-stream marker was reached"

// Reports a member type tarfile reads as a regular file
func tarIsRegular(flag byte) bool {
	return flag == tarRegType || flag == tarARegType || flag == tarContType || flag == tarGNUSparse
}

// Reports a member type listed in tarfile SUPPORTED_TYPES
func tarSupported(flag byte) bool {
	switch flag {
	case tarRegType, tarARegType, tarLnkType, tarSymType, tarDirType, tarFifoType, tarContType,
		tarChrType, tarBlkType, tarGNULongName, tarGNULongLink, tarGNUSparse:
		return true
	}
	return false
}

// Code points of raw name bytes as surrogateescape decodes them, undecodable bytes as lone surrogates
func tarCodePoints(s string) []rune {
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = 0xDC00 + rune(s[i])
		}
		out = append(out, r)
		i += size
	}
	return out
}

// Reports a code point Python str isprintable accepts
func tarPrintable(r rune) bool {
	if r == ' ' {
		return true
	}
	if r >= 0xD800 && r <= 0xDFFF {
		return false
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Zl, unicode.Zp, unicode.Zs) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}

// Python repr of a str whose raw bytes surrogateescape decoded
func tarRepr(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range tarCodePoints(s) {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f || tarPrintable(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// Python repr of a bytes object
func tarBytesRepr(s []byte) string {
	quote := byte('\'')
	if bytes.IndexByte(s, '\'') >= 0 && bytes.IndexByte(s, '"') < 0 {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte('b')
	b.WriteByte(quote)
	for _, c := range s {
		switch {
		case c == quote || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < ' ' || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// First n code points of a repr, the truncation a %.200R format applies
func tarTruncate(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// Decimal value of a Unicode Nd digit, minus one for any other code point
func tarDecimal(r rune) int {
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	return -1
}

// ASCII text int and float parse, Unicode digits and spaces mapped and anything else cut at a question mark
func tarASCIIDigits(s string) string {
	var b strings.Builder
	for _, r := range tarCodePoints(s) {
		switch {
		case r < 127:
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case tarDecimal(r) >= 0:
			b.WriteByte(byte('0' + tarDecimal(r)))
		default:
			b.WriteByte('?')
			return b.String()
		}
	}
	return b.String()
}

// Reports a byte C Py_ISSPACE accepts
func tarCSpace(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// Integer the way PyLong_FromString parses ASCII text in base 8 or 10, with its digit count
func tarParseInt(s string, base int) (*big.Int, int, bool) {
	i := 0
	for i < len(s) && tarCSpace(s[i]) {
		i++
	}
	negative := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		negative = s[i] == '-'
		i++
	}
	if base == 8 && i+1 < len(s) && s[i] == '0' && (s[i+1] == 'o' || s[i+1] == 'O') {
		i += 2
		if i < len(s) && s[i] == '_' {
			i++
		}
	}
	isDigit := func(c byte) bool { return c >= '0' && c < byte('0'+base) }
	var digits strings.Builder
	if i >= len(s) || !isDigit(s[i]) {
		return nil, 0, false
	}
	for i < len(s) {
		if isDigit(s[i]) {
			digits.WriteByte(s[i])
			i++
			continue
		}
		if s[i] == '_' && i+1 < len(s) && isDigit(s[i+1]) {
			i++
			continue
		}
		break
	}
	for i < len(s) && tarCSpace(s[i]) {
		i++
	}
	if i != len(s) {
		return nil, 0, false
	}
	n, _ := new(big.Int).SetString(digits.String(), base)
	if negative {
		n.Neg(n)
	}
	return n, digits.Len(), true
}

// Value of int applied to a str or bytes in base 10, a ValueError carrying Python's message on refusal
func tarPyInt(s string, isBytes bool) (*big.Int, error) {
	text, shown := s, ""
	if isBytes {
		shown = tarBytesRepr([]byte(s))
	} else {
		text, shown = tarASCIIDigits(s), tarRepr(s)
	}
	n, digits, ok := tarParseInt(text, 10)
	if !ok {
		return nil, &TarReadError{Class: "ValueError", Message: "invalid literal for int() with base 10: " + tarTruncate(shown, 200)}
	}
	if digits > 4300 {
		return nil, &TarReadError{Class: "ValueError", Message: fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit", digits)}
	}
	return n, nil
}

// Value of float applied to a str, false where Python raises ValueError
func tarPyFloat(s string) (float64, bool) {
	text := tarASCIIDigits(s)
	lo, hi := 0, len(text)
	for lo < hi && tarCSpace(text[lo]) {
		lo++
	}
	for hi > lo+1 && tarCSpace(text[hi-1]) {
		hi--
	}
	text = text[lo:hi]
	if text == "" {
		return 0, false
	}
	if strings.Contains(text, "_") {
		var b strings.Builder
		prev := byte(0)
		for i := 0; i < len(text); i++ {
			c := text[i]
			isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
			if c == '_' {
				if !isDigit(prev) {
					return 0, false
				}
			} else {
				if prev == '_' && !isDigit(c) {
					return 0, false
				}
				b.WriteByte(c)
			}
			prev = c
		}
		if prev == '_' {
			return 0, false
		}
		text = b.String()
	}
	body := strings.TrimLeft(text, "+-")
	if len(text)-len(body) > 1 {
		return 0, false
	}
	switch strings.ToLower(body) {
	case "inf", "infinity":
		if text[0] == '-' {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}
	mantissa, exponent, hasExp := strings.Cut(strings.ToLower(body), "e")
	whole, frac, _ := strings.Cut(mantissa, ".")
	allDigits := func(t string) bool {
		for i := 0; i < len(t); i++ {
			if t[i] < '0' || t[i] > '9' {
				return false
			}
		}
		return true
	}
	if whole+frac == "" || !allDigits(whole) || !allDigits(frac) {
		return 0, false
	}
	if hasExp {
		digits := strings.TrimLeft(exponent, "+-")
		if digits == "" || len(exponent)-len(digits) > 1 || !allDigits(digits) {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// Bytes before the first NUL, the text nts decodes with surrogateescape
func tarNts(s []byte) string {
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

// Numeric field the way nti reads it, base 256 or NUL terminated octal text
func tarNti(s []byte) (*big.Int, error) {
	if len(s) == 0 {
		return nil, &TarReadError{Class: "IndexError", Message: "index out of range"}
	}
	if s[0] == 0o200 || s[0] == 0o377 {
		n := new(big.Int).SetBytes(s[1:])
		if s[0] == 0o377 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(8*(len(s)-1))))
		}
		return n, nil
	}
	text := tarNts(s)
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return nil, tarInvalidHeader()
		}
	}
	text = strings.Trim(text, " \t\n\r\x0b\x0c\x1c\x1d\x1e\x1f")
	if text == "" {
		text = "0"
	}
	n, _, ok := tarParseInt(text, 8)
	if !ok {
		return nil, tarInvalidHeader()
	}
	return n, nil
}

// Unsigned and signed header checksums with the checksum field read as spaces
func tarChecksums(buf []byte) (int64, int64) {
	unsigned, signed := int64(256), int64(256)
	for i, c := range buf[:tarBlockSize] {
		if i < 148 || i >= 156 {
			unsigned += int64(c)
			signed += int64(int8(c))
		}
	}
	return unsigned, signed
}

// Count rounded up to whole blocks, a negative count an InvalidHeaderError
func tarBlock(count *big.Int) (*big.Int, error) {
	if count.Sign() < 0 {
		return nil, &tarHeaderError{class: "InvalidHeaderError", msg: "invalid offset"}
	}
	n := new(big.Int).Add(count, big.NewInt(tarBlockSize-1))
	n.Div(n, big.NewInt(tarBlockSize))
	return n.Mul(n, big.NewInt(tarBlockSize)), nil
}

// Insertion ordered pax records, keys keeping their first position when replaced
type tarPaxRecords struct {
	keys   []string
	values map[string]string
}

// Empty record set
func newTarPaxRecords() *tarPaxRecords { return &tarPaxRecords{values: map[string]string{}} }

// Sets a record, a new key appended after the existing ones
func (p *tarPaxRecords) set(key, value string) {
	if _, ok := p.values[key]; !ok {
		p.keys = append(p.keys, key)
	}
	p.values[key] = value
}

// Value of a record and whether it exists
func (p *tarPaxRecords) get(key string) (string, bool) {
	v, ok := p.values[key]
	return v, ok
}

// Independent copy keeping the key order
func (p *tarPaxRecords) clone() *tarPaxRecords {
	c := &tarPaxRecords{keys: append([]string(nil), p.keys...), values: make(map[string]string, len(p.values))}
	for k, v := range p.values {
		c.values[k] = v
	}
	return c
}

// Modification time a member carries, an int from a header or a float from a pax record
type tarMtime struct {
	isFloat bool
	i       *big.Int
	f       float64
}

// One sparse data run, its offset in the restored file and its length
type tarSparseBlock struct {
	offset   *big.Int
	numbytes *big.Int
}

// Old GNU sparse structures a header holds until the extended blocks are read
type tarGNUSparseHeader struct {
	structs    []tarSparseBlock
	isextended bool
	origsize   *big.Int
}

// One archive member as tarfile TarInfo describes it
type tarInfo struct {
	name       string
	linkname   string
	mode       int64
	size       *big.Int
	mtime      tarMtime
	typ        byte
	offset     *big.Int
	offsetData *big.Int
	sparse     []tarSparseBlock
	gnuSparse  *tarGNUSparseHeader
}

// Reports a member tarfile extracts as a regular file
func (t *tarInfo) isreg() bool { return tarIsRegular(t.typ) }

// Reports a directory member
func (t *tarInfo) isdir() bool { return t.typ == tarDirType }

// Reports a symbolic link member
func (t *tarInfo) issym() bool { return t.typ == tarSymType }

// Reports a hard link member
func (t *tarInfo) islnk() bool { return t.typ == tarLnkType }

// Member from one header block, old v7 directories recognised by their trailing slash when dircheck is set
func tarFrombuf(buf []byte, dircheck bool) (*tarInfo, error) {
	switch {
	case len(buf) == 0:
		return nil, &tarHeaderError{class: "EmptyHeaderError", msg: "empty header"}
	case len(buf) != tarBlockSize:
		return nil, &tarHeaderError{class: "TruncatedHeaderError", msg: "truncated header"}
	case bytes.Count(buf, []byte{0}) == tarBlockSize:
		return nil, &tarHeaderError{class: "EOFHeaderError", msg: "end of file header"}
	}
	chksum, err := tarNti(buf[148:156])
	if err != nil {
		return nil, err
	}
	unsigned, signed := tarChecksums(buf)
	if !chksum.IsInt64() || (chksum.Int64() != unsigned && chksum.Int64() != signed) {
		return nil, &tarHeaderError{class: "InvalidHeaderError", msg: "bad checksum"}
	}
	t := &tarInfo{name: tarNts(buf[0:100])}
	mode, err := tarNti(buf[100:108])
	if err != nil {
		return nil, err
	}
	t.mode = mode.Int64()
	for _, field := range [][]byte{buf[108:116], buf[116:124]} {
		if _, err := tarNti(field); err != nil {
			return nil, err
		}
	}
	if t.size, err = tarNti(buf[124:136]); err != nil {
		return nil, err
	}
	mtime, err := tarNti(buf[136:148])
	if err != nil {
		return nil, err
	}
	t.mtime = tarMtime{i: mtime}
	t.typ = buf[156]
	t.linkname = tarNts(buf[157:257])
	for _, field := range [][]byte{buf[329:337], buf[337:345]} {
		if _, err := tarNti(field); err != nil {
			return nil, err
		}
	}
	prefix := tarNts(buf[345:500])
	if dircheck && t.typ == tarARegType && strings.HasSuffix(t.name, "/") {
		t.typ = tarDirType
	}
	if t.typ == tarGNUSparse {
		sp := &tarGNUSparseHeader{}
		for pos := 386; pos < 386+4*24; pos += 24 {
			offset, err := tarNti(buf[pos : pos+12])
			if err != nil {
				return nil, err
			}
			numbytes, err := tarNti(buf[pos+12 : pos+24])
			if err != nil {
				return nil, err
			}
			sp.structs = append(sp.structs, tarSparseBlock{offset: offset, numbytes: numbytes})
		}
		sp.isextended = buf[482] != 0
		if sp.origsize, err = tarNti(buf[483:495]); err != nil {
			return nil, err
		}
		t.gnuSparse = sp
	}
	if t.isdir() {
		t.name = strings.TrimRight(t.name, "/")
	}
	if prefix != "" && t.typ != tarGNULongName && t.typ != tarGNULongLink && t.typ != tarGNUSparse {
		t.name = prefix + "/" + t.name
	}
	return t, nil
}

// Archive bytes as the file object tarfile reads, with its read, tell and seek behaviour
type tarStream interface {
	read(n int) ([]byte, error)
	tell() int64
	seek(pos *big.Int) error
	close()
}

// Uncompressed archive read from its source, an open file or an in memory buffer
type tarRawStream struct {
	src  io.ReadSeeker
	pos  int64
	file bool
}

// Up to n bytes, fewer only at the end of the source
func (s *tarRawStream) read(n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(s.src, buf)
	s.pos += int64(got)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:got], nil
}

func (s *tarRawStream) tell() int64 { return s.pos }

// Moves to an absolute position, past the end allowed, an offset beyond the C range refused as Python does
func (s *tarRawStream) seek(pos *big.Int) error {
	if !pos.IsInt64() {
		if s.file {
			return &TarReadError{Class: "ValueError", Message: "cannot fit 'int' into an offset-sized integer"}
		}
		return &TarReadError{Class: "OverflowError", Message: "Python int too large to convert to C ssize_t"}
	}
	p, err := s.src.Seek(pos.Int64(), io.SeekStart)
	if err != nil {
		return err
	}
	s.pos = p
	return nil
}

func (s *tarRawStream) close() {}

// Source reads that remember their own failure so codec errors can be told apart from file errors
type tarSourceReader struct {
	r   io.Reader
	err error
}

func (s *tarSourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// Classifies a codec failure as the Python exception its module raises, file errors passed through
func tarCodecFailure(src *tarSourceReader, kind string, err error) error {
	switch {
	case err == nil || err == io.EOF:
		return err
	case src.err != nil && errors.Is(err, src.err):
		return src.err
	case errors.Is(err, io.ErrUnexpectedEOF):
		return &TarReadError{Class: "EOFError", Message: tarCompressedEOF}
	}
	var already *TarReadError
	if errors.As(err, &already) {
		return err
	}
	switch kind {
	case "gz":
		return &TarReadError{Class: "zlib.error", Message: err.Error()}
	case "bz2":
		return &TarReadError{Class: "OSError", Message: "Invalid data stream"}
	case "xz":
		return &TarReadError{Class: "LZMAError", Message: "Corrupt input data"}
	}
	return &TarReadError{Class: "ZstdError", Message: "Unable to decompress Zstandard data: " + err.Error()}
}

// Gzip members read the way gzip _GzipReader does, header CRC unchecked, zero padding skipped, members concatenated
type tarGzipReader struct {
	src     *tarSourceReader
	br      *bufio.Reader
	inflate io.ReadCloser
	crc     uint32
	size    uint32
	done    bool
}

// Exactly n bytes of the compressed stream, an EOFError when it stops early
func (g *tarGzipReader) readExact(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(g.br, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if g.src.err != nil {
				return nil, g.src.err
			}
			return nil, &TarReadError{Class: "EOFError", Message: tarCompressedEOF}
		}
		return nil, err
	}
	return buf, nil
}

// Skips a NUL terminated header string, stopping quietly at the end of the stream
func (g *tarGzipReader) skipString() error {
	for {
		c, err := g.br.ReadByte()
		if err == io.EOF || (err == nil && c == 0) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Reads one member header, false at a clean end of stream
func (g *tarGzipReader) header() (bool, error) {
	magic := make([]byte, 2)
	n, err := io.ReadFull(g.br, magic)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if n != 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return false, &TarReadError{Class: "BadGzipFile", Message: fmt.Sprintf("Not a gzipped file (%s)", tarBytesRepr(magic[:n]))}
	}
	hdr, err := g.readExact(8)
	if err != nil {
		return false, err
	}
	if hdr[0] != 8 {
		return false, &TarReadError{Class: "BadGzipFile", Message: "Unknown compression method"}
	}
	flag := hdr[1]
	if flag&4 != 0 {
		extra, err := g.readExact(2)
		if err != nil {
			return false, err
		}
		if _, err := g.readExact(int(binary.LittleEndian.Uint16(extra))); err != nil {
			return false, err
		}
	}
	for _, bit := range []byte{8, 16} {
		if flag&bit != 0 {
			if err := g.skipString(); err != nil {
				return false, err
			}
		}
	}
	if flag&2 != 0 {
		if _, err := g.readExact(2); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Verifies a member trailer then skips the zero padding before the next member
func (g *tarGzipReader) trailer() error {
	tail, err := g.readExact(8)
	if err != nil {
		return err
	}
	crc, isize := binary.LittleEndian.Uint32(tail[0:4]), binary.LittleEndian.Uint32(tail[4:8])
	if crc != g.crc {
		return &TarReadError{Class: "BadGzipFile", Message: fmt.Sprintf("CRC check failed 0x%x != 0x%x", crc, g.crc)}
	}
	if isize != g.size {
		return &TarReadError{Class: "BadGzipFile", Message: "Incorrect length of data produced"}
	}
	for {
		c, err := g.br.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if c != 0 {
			return g.br.UnreadByte()
		}
	}
}

func (g *tarGzipReader) Read(p []byte) (int, error) {
	for {
		if g.done {
			return 0, io.EOF
		}
		if g.inflate == nil {
			ok, err := g.header()
			if err != nil {
				return 0, err
			}
			if !ok {
				g.done = true
				return 0, io.EOF
			}
			g.inflate, g.crc, g.size = flate.NewReader(g.br), 0, 0
		}
		n, err := g.inflate.Read(p)
		g.crc = crc32.Update(g.crc, crc32.IEEETable, p[:n])
		g.size += uint32(n)
		if err == io.EOF {
			g.inflate = nil
			if terr := g.trailer(); terr != nil {
				return n, terr
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			return n, tarCodecFailure(g.src, "gz", err)
		}
		if n > 0 || len(p) == 0 {
			return n, nil
		}
	}
}

// LZMA error liblzma reports for a header its auto decoder does not recognise
func tarLZMAFormatError() error {
	return &TarReadError{Class: "LZMAError", Message: "Input format not supported by decoder"}
}

// Streams liblzma auto detection decodes, xz by its magic, lzip on an L and LZMA_alone behind the picky header check
type tarLZMAReader struct {
	br      *bufio.Reader
	src     *tarSourceReader
	cur     io.Reader
	started bool
}

// Byte source an LZMA decoder reads exactly, without buffering past what it takes
type tarByteSource interface {
	io.Reader
	io.ByteReader
}

// Byte source serving a synthetic LZMA_alone header before the real stream
type tarLZMAHeaded struct {
	head []byte
	br   tarByteSource
}

// Largest dictionary the LZMA decoder accepts on this platform, liblzma taking any 32 bit size
var tarLZMADictCap = int(min(int64(lzma.MaxDictCap), int64(math.MaxInt)))

func (h *tarLZMAHeaded) Read(p []byte) (int, error) {
	if len(h.head) > 0 {
		n := copy(p, h.head)
		h.head = h.head[n:]
		return n, nil
	}
	return h.br.Read(p)
}

func (h *tarLZMAHeaded) ReadByte() (byte, error) {
	if len(h.head) > 0 {
		c := h.head[0]
		h.head = h.head[1:]
		return c, nil
	}
	return h.br.ReadByte()
}

// Decoder for the stream at the current position, an EOFError when too few bytes remain to recognise it
func (z *tarLZMAReader) open(first bool) (io.Reader, error) {
	head, _ := z.br.Peek(13)
	if len(head) == 0 && !first {
		return nil, io.EOF
	}
	if len(head) == 0 {
		return nil, &TarReadError{Class: "EOFError", Message: tarCompressedEOF}
	}
	switch head[0] {
	case 0xFD:
		return xz.NewReader(z.br)
	case 'L':
		return z.lzip()
	}
	if len(head) < 13 {
		return nil, &TarReadError{Class: "EOFError", Message: tarCompressedEOF}
	}
	props := head[0]
	if props > (4*5+4)*9+8 {
		return nil, tarLZMAFormatError()
	}
	pb := props / 45
	lp := (props - pb*45) / 9
	if lc := props - pb*45 - lp*9; lc+lp > 4 {
		return nil, tarLZMAFormatError()
	}
	if dict := binary.LittleEndian.Uint32(head[1:5]); dict != math.MaxUint32 {
		d := dict - 1
		for _, shift := range []uint{2, 3, 4, 8, 16} {
			d |= d >> shift
		}
		if d+1 != dict {
			return nil, tarLZMAFormatError()
		}
	}
	if size := binary.LittleEndian.Uint64(head[5:13]); size != math.MaxUint64 && size >= 1<<38 {
		return nil, tarLZMAFormatError()
	}
	dec, err := lzma.ReaderConfig{DictCap: tarLZMADictCap}.NewReader(&tarLZMAHeaded{br: z.br})
	if err != nil {
		return nil, err
	}
	return &tarLZMADrain{r: dec}, nil
}

// LZMA decoder whose truncation error waits until every byte it decoded before the cut has been read
type tarLZMADrain struct {
	r   io.Reader
	err error
}

func (d *tarLZMADrain) Read(p []byte) (int, error) {
	for {
		n, err := d.r.Read(p)
		if errors.Is(err, io.ErrUnexpectedEOF) && d.err == nil {
			d.err = err
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err == io.EOF && d.err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, d.err
		}
		return n, err
	}
}

// One lzip member decoded as an LZMA stream then checked against its CRC, data size and member size
func (z *tarLZMAReader) lzip() (io.Reader, error) {
	head := make([]byte, 6)
	if _, err := io.ReadFull(z.br, head); err != nil {
		return nil, &TarReadError{Class: "EOFError", Message: tarCompressedEOF}
	}
	if string(head[:4]) != "LZIP" || head[4] > 1 {
		return nil, tarLZMAFormatError()
	}
	dict := uint32(1) << (head[5] & 0x1F)
	dict -= (dict >> 4) * uint32((head[5]>>5)&7)
	if dict < 1<<12 || dict > 1<<29 {
		return nil, tarLZMAFormatError()
	}
	alone := []byte{0x5D, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	binary.LittleEndian.PutUint32(alone[1:5], dict)
	counter := &tarLzipCounter{br: z.br}
	dec, err := lzma.ReaderConfig{DictCap: tarLZMADictCap}.NewReader(&tarLZMAHeaded{head: alone, br: counter})
	if err != nil {
		return nil, err
	}
	return &tarLzipMember{dec: &tarLZMADrain{r: dec}, br: z.br, counter: counter, version: head[4]}, nil
}

// Counts compressed bytes an lzip member's LZMA data takes, for its member size check
type tarLzipCounter struct {
	br *bufio.Reader
	n  uint64
}

func (c *tarLzipCounter) Read(p []byte) (int, error) {
	n, err := c.br.Read(p)
	c.n += uint64(n)
	return n, err
}

func (c *tarLzipCounter) ReadByte() (byte, error) {
	b, err := c.br.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// Decoded data of one lzip member and the trailer verification at its end
type tarLzipMember struct {
	dec     io.Reader
	br      *bufio.Reader
	counter *tarLzipCounter
	version byte
	crc     uint32
	size    uint64
}

func (m *tarLzipMember) Read(p []byte) (int, error) {
	n, err := m.dec.Read(p)
	m.crc = crc32.Update(m.crc, crc32.IEEETable, p[:n])
	m.size += uint64(n)
	if err != io.EOF {
		return n, err
	}
	trailer := 12
	if m.version == 1 {
		trailer = 20
	}
	tail := make([]byte, trailer)
	if _, terr := io.ReadFull(m.br, tail); terr != nil {
		return n, &TarReadError{Class: "EOFError", Message: tarCompressedEOF}
	}
	corrupt := &TarReadError{Class: "LZMAError", Message: "Corrupt input data"}
	if binary.LittleEndian.Uint32(tail[0:4]) != m.crc || binary.LittleEndian.Uint64(tail[4:12]) != m.size {
		return n, corrupt
	}
	if m.version == 1 && binary.LittleEndian.Uint64(tail[12:20]) != 6+m.counter.n+20 {
		return n, corrupt
	}
	return n, io.EOF
}

func (z *tarLZMAReader) Read(p []byte) (int, error) {
	for {
		if z.cur == nil {
			first := !z.started
			z.started = true
			cur, err := z.open(first)
			if err != nil {
				var read *TarReadError
				truncated := errors.As(err, &read) && read.Class == "EOFError"
				if !first && err != io.EOF && !truncated && (z.src.err == nil || !errors.Is(err, z.src.err)) {
					return 0, io.EOF
				}
				return 0, err
			}
			z.cur = cur
		}
		n, err := z.cur.Read(p)
		if err == io.EOF {
			z.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// Reader whose construction failure surfaces as its first read, the way Python codecs fail on reading
type tarFailedReader struct{ err error }

func (f tarFailedReader) Read([]byte) (int, error) { return 0, f.err }

// Codec reader that classifies every failure as its Python exception
type tarClassifiedReader struct {
	r    io.Reader
	src  *tarSourceReader
	kind string
}

func (c *tarClassifiedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	return n, tarCodecFailure(c.src, c.kind, err)
}

// Decompressor of one kind over the source at its current position and the function releasing it
func tarCodec(kind string, src io.Reader) (io.Reader, func()) {
	tagged := &tarSourceReader{r: src}
	switch kind {
	case "gz":
		return &tarGzipReader{src: tagged, br: bufio.NewReaderSize(tagged, 1<<16)}, func() {}
	case "bz2":
		return &tarClassifiedReader{r: bzip2.NewReader(bufio.NewReaderSize(tagged, 1<<16)), src: tagged, kind: kind}, func() {}
	case "xz":
		return &tarClassifiedReader{r: &tarLZMAReader{br: bufio.NewReaderSize(tagged, 1<<16), src: tagged}, src: tagged, kind: kind}, func() {}
	}
	z, err := zstd.NewReader(bufio.NewReaderSize(tagged, 1<<16), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(1<<27))
	if err != nil {
		return tarFailedReader{err: tarCodecFailure(tagged, kind, err)}, func() {}
	}
	return &tarClassifiedReader{r: z, src: tagged, kind: kind}, z.Close
}

// Decompressed archive that rewinds its source and decompresses again for a backward seek as GzipFile does
type tarCodecStream struct {
	src     io.ReadSeeker
	kind    string
	dec     io.Reader
	release func()
	pos     int64
	pending []byte
	err     error
	chunk   []byte
}

// Decompressed stream of one kind over the source from its current position
func newTarCodecStream(src io.ReadSeeker, kind string) *tarCodecStream {
	s := &tarCodecStream{src: src, kind: kind, chunk: make([]byte, 1<<16)}
	s.dec, s.release = tarCodec(kind, src)
	return s
}

// Up to n decompressed bytes, fewer only at the end, a codec failure raised once its bytes are needed
func (s *tarCodecStream) read(n int) ([]byte, error) {
	out := make([]byte, 0, min(n, 1<<20))
	for len(out) < n {
		if len(s.pending) > 0 {
			k := min(n-len(out), len(s.pending))
			out = append(out, s.pending[:k]...)
			s.pending = s.pending[k:]
			continue
		}
		if s.err == io.EOF {
			break
		}
		if s.err != nil {
			return nil, s.err
		}
		m, err := s.dec.Read(s.chunk)
		s.pending, s.err = s.chunk[:m], err
	}
	s.pos += int64(len(out))
	return out, nil
}

func (s *tarCodecStream) tell() int64 { return s.pos }

// Moves to an absolute decompressed position, rewinding for a backward move, stopping at the end, refusing a non C offset
func (s *tarCodecStream) seek(pos *big.Int) error {
	if !pos.IsInt64() {
		return &TarReadError{Class: "ValueError", Message: "cannot fit 'int' into an offset-sized integer"}
	}
	if pos.Cmp(big.NewInt(s.pos)) < 0 {
		if _, err := s.src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		s.release()
		s.dec, s.release = tarCodec(s.kind, s.src)
		s.pos, s.pending, s.err = 0, nil, nil
	}
	remaining := new(big.Int).Sub(pos, big.NewInt(s.pos))
	for remaining.Sign() > 0 {
		step := int64(1 << 16)
		if remaining.IsInt64() && remaining.Int64() < step {
			step = remaining.Int64()
		}
		data, err := s.read(int(step))
		if err != nil {
			return err
		}
		if len(data) == 0 {
			break
		}
		remaining.Sub(remaining, big.NewInt(int64(len(data))))
	}
	return nil
}

func (s *tarCodecStream) close() { s.release() }

// Reads up to n bytes in bounded chunks so a forged extended header size cannot force one huge allocation
func tarSafeRead(s tarStream, n *big.Int) ([]byte, error) {
	if n.IsInt64() && n.Int64() <= tarExtHeaderChunk {
		return s.read(int(n.Int64()))
	}
	var out []byte
	left := new(big.Int).Set(n)
	for left.Sign() > 0 {
		step := int64(tarExtHeaderChunk)
		if left.IsInt64() && left.Int64() < step {
			step = left.Int64()
		}
		chunk, err := s.read(int(step))
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			break
		}
		out = append(out, chunk...)
		left.Sub(left, big.NewInt(int64(len(chunk))))
	}
	return out, nil
}

// Open archive as tarfile TarFile reads it, members cached in order and global pax records accumulated
type tarFile struct {
	stream      tarStream
	offset      *big.Int
	paxHeaders  *tarPaxRecords
	members     []*tarInfo
	loaded      bool
	firstmember *tarInfo
}

// Next member parsed from the stream, dispatched on its type the way fromtarfile does
func (tf *tarFile) fromtarfile(dircheck bool) (*tarInfo, error) {
	buf, err := tf.stream.read(tarBlockSize)
	if err != nil {
		return nil, err
	}
	t, err := tarFrombuf(buf, dircheck)
	if err != nil {
		return nil, err
	}
	t.offset = big.NewInt(tf.stream.tell() - tarBlockSize)
	switch t.typ {
	case tarGNULongName, tarGNULongLink:
		return tf.procGNULong(t)
	case tarGNUSparse:
		return tf.procSparse(t)
	case tarXHDType, tarXGLType, tarSolarisXHDType:
		return tf.procPax(t)
	}
	return tf.procBuiltin(t)
}

// A builtin or unknown member, its data skipped for regular and unknown types and global records applied
func (tf *tarFile) procBuiltin(t *tarInfo) (*tarInfo, error) {
	t.offsetData = big.NewInt(tf.stream.tell())
	offset := new(big.Int).Set(t.offsetData)
	if t.isreg() || !tarSupported(t.typ) {
		blocks, err := tarBlock(t.size)
		if err != nil {
			return nil, err
		}
		offset.Add(offset, blocks)
	}
	tf.offset = offset
	if err := t.applyPaxInfo(tf.paxHeaders); err != nil {
		return nil, err
	}
	if t.isdir() {
		t.name = strings.TrimRight(t.name, "/")
	}
	return t, nil
}

// The member after a GNU longname or longlink block, renamed or relinked from the block data
func (tf *tarFile) procGNULong(t *tarInfo) (*tarInfo, error) {
	blocks, err := tarBlock(t.size)
	if err != nil {
		return nil, err
	}
	buf, err := tarSafeRead(tf.stream, blocks)
	if err != nil {
		return nil, err
	}
	next, err := tf.fromtarfile(false)
	if err != nil {
		return nil, tarSubsequent(err)
	}
	next.offset = t.offset
	if t.typ == tarGNULongName {
		next.name = tarNts(buf)
	} else {
		next.linkname = tarNts(buf)
	}
	if next.isdir() {
		next.name = strings.TrimSuffix(next.name, "/")
	}
	return next, nil
}

// HeaderError from a follow up header turned into the SubsequentHeaderError tarfile raises
func tarSubsequent(err error) error {
	var h *tarHeaderError
	if errors.As(err, &h) {
		return &tarHeaderError{class: "SubsequentHeaderError", msg: h.msg}
	}
	return err
}

// An old GNU sparse member with its extended sparse blocks collected
func (tf *tarFile) procSparse(t *tarInfo) (*tarInfo, error) {
	sp := t.gnuSparse
	t.gnuSparse = nil
	structs, isextended := sp.structs, sp.isextended
	for isextended {
		buf, err := tf.stream.read(tarBlockSize)
		if err != nil {
			return nil, err
		}
		window := func(lo, hi int) []byte { return buf[min(lo, len(buf)):min(hi, len(buf))] }
		for pos := 0; pos < 21*24; pos += 24 {
			offset, err := tarNti(window(pos, pos+12))
			if err != nil {
				return nil, err
			}
			numbytes, err := tarNti(window(pos+12, pos+24))
			if err != nil {
				return nil, err
			}
			if offset.Sign() != 0 && numbytes.Sign() != 0 {
				structs = append(structs, tarSparseBlock{offset: offset, numbytes: numbytes})
			}
		}
		if len(buf) <= 504 {
			return nil, &TarReadError{Class: "IndexError", Message: "index out of range"}
		}
		isextended = buf[504] != 0
	}
	t.sparse = append(make([]tarSparseBlock, 0, len(structs)), structs...)
	t.offsetData = big.NewInt(tf.stream.tell())
	blocks, err := tarBlock(t.size)
	if err != nil {
		return nil, err
	}
	tf.offset = new(big.Int).Add(t.offsetData, blocks)
	t.size = sp.origsize
	return t, nil
}

// One raw pax record, keyword and value bytes before decoding
type tarRawRecord struct {
	keyword []byte
	value   []byte
}

// Parses a pax, Solaris or global header and its member, records kept raw since every hdrcharset round trips them
func (tf *tarFile) procPax(t *tarInfo) (*tarInfo, error) {
	blocks, err := tarBlock(t.size)
	if err != nil {
		return nil, err
	}
	buf, err := tarSafeRead(tf.stream, blocks)
	if err != nil {
		return nil, err
	}
	pax := tf.paxHeaders
	if t.typ != tarXGLType {
		pax = tf.paxHeaders.clone()
	}
	var raws []tarRawRecord
	for pos := 0; len(buf) > pos && buf[pos] != 0; {
		digits := 0
		for digits < 20 && pos+digits < len(buf) && buf[pos+digits] >= '0' && buf[pos+digits] <= '9' {
			digits++
		}
		if digits == 0 || pos+digits >= len(buf) || buf[pos+digits] != ' ' {
			return nil, tarInvalidHeader()
		}
		length, err := strconv.ParseUint(string(buf[pos:pos+digits]), 10, 64)
		if err != nil || length > uint64(len(buf)) {
			return nil, tarInvalidHeader()
		}
		if length < 5 || pos+int(length) > len(buf) {
			return nil, tarInvalidHeader()
		}
		end := pos + int(length) - 1
		var record []byte
		if start := pos + digits + 1; start < end {
			record = buf[start:end]
		}
		keyword, value, found := bytes.Cut(record, []byte("="))
		if len(keyword) == 0 || !found || buf[end] != '\n' {
			return nil, tarInvalidHeader()
		}
		raws = append(raws, tarRawRecord{keyword: keyword, value: value})
		pos += int(length)
	}
	for _, raw := range raws {
		pax.set(string(raw.keyword), string(raw.value))
	}
	next, err := tf.fromtarfile(false)
	if err != nil {
		return nil, tarSubsequent(err)
	}
	if _, ok := pax.get("GNU.sparse.map"); ok {
		if err := tarGNUSparse01(next, pax); err != nil {
			return nil, err
		}
	} else if _, ok := pax.get("GNU.sparse.size"); ok {
		if err := tarGNUSparse00(next, raws); err != nil {
			return nil, err
		}
	} else if major, _ := pax.get("GNU.sparse.major"); major == "1" {
		if minor, _ := pax.get("GNU.sparse.minor"); minor == "0" {
			if err := tf.gnuSparse10(next); err != nil {
				return nil, err
			}
		}
	}
	if t.typ == tarXHDType || t.typ == tarSolarisXHDType {
		if err := next.applyPaxInfo(pax); err != nil {
			return nil, err
		}
		if value, ok := pax.get("size"); ok {
			offset := new(big.Int).Add(next.offset, big.NewInt(tarBlockSize))
			if next.isreg() || !tarSupported(next.typ) {
				size, err := tarPyInt(value, false)
				if err != nil {
					size = new(big.Int)
				}
				blocks, err := tarBlock(size)
				if err != nil {
					return nil, err
				}
				offset.Add(offset, blocks)
			}
			tf.offset = offset
		}
		next.offset = t.offset
	}
	return next, nil
}

// GNU pax sparse format 0.0, repeated offset and numbytes records paired in order
func tarGNUSparse00(next *tarInfo, raws []tarRawRecord) error {
	var offsets, numbytes []*big.Int
	for _, raw := range raws {
		keyword := string(raw.keyword)
		if keyword != "GNU.sparse.offset" && keyword != "GNU.sparse.numbytes" {
			continue
		}
		if !utf8.Valid(raw.value) {
			return tarInvalidHeader()
		}
		n, err := tarPyInt(string(raw.value), false)
		if err != nil {
			return tarInvalidHeader()
		}
		if keyword == "GNU.sparse.offset" {
			offsets = append(offsets, n)
		} else {
			numbytes = append(numbytes, n)
		}
	}
	next.sparse = tarPairs(offsets, numbytes)
	return nil
}

// GNU pax sparse format 0.1, one comma separated map of alternating offsets and sizes
func tarGNUSparse01(next *tarInfo, pax *tarPaxRecords) error {
	spec, _ := pax.get("GNU.sparse.map")
	var numbers []*big.Int
	for _, field := range strings.Split(spec, ",") {
		n, err := tarPyInt(field, false)
		if err != nil {
			return err
		}
		numbers = append(numbers, n)
	}
	var offsets, sizes []*big.Int
	for i, n := range numbers {
		if i%2 == 0 {
			offsets = append(offsets, n)
		} else {
			sizes = append(sizes, n)
		}
	}
	next.sparse = tarPairs(offsets, sizes)
	return nil
}

// Sparse runs zipped from offsets and sizes, the longer list cut to the shorter
func tarPairs(offsets, sizes []*big.Int) []tarSparseBlock {
	out := make([]tarSparseBlock, 0, min(len(offsets), len(sizes)))
	for i := range min(len(offsets), len(sizes)) {
		out = append(out, tarSparseBlock{offset: offsets[i], numbytes: sizes[i]})
	}
	return out
}

// GNU pax sparse format 1.0, a newline separated map at the start of the member data
func (tf *tarFile) gnuSparse10(next *tarInfo) error {
	unpack := &TarReadError{Class: "ValueError", Message: "not enough values to unpack (expected 2, got 1)"}
	buf, err := tf.stream.read(tarBlockSize)
	if err != nil {
		return err
	}
	head, rest, found := bytes.Cut(buf, []byte("\n"))
	if !found {
		return unpack
	}
	fields, err := tarPyInt(string(head), true)
	if err != nil {
		return err
	}
	want := new(big.Int).Mul(fields, big.NewInt(2))
	buf = rest
	var numbers []*big.Int
	for big.NewInt(int64(len(numbers))).Cmp(want) < 0 {
		if bytes.IndexByte(buf, '\n') < 0 {
			more, err := tf.stream.read(tarBlockSize)
			if err != nil {
				return err
			}
			buf = append(append([]byte(nil), buf...), more...)
		}
		number, rest, found := bytes.Cut(buf, []byte("\n"))
		if !found {
			return unpack
		}
		n, err := tarPyInt(string(number), true)
		if err != nil {
			return err
		}
		numbers = append(numbers, n)
		buf = rest
	}
	next.offsetData = big.NewInt(tf.stream.tell())
	var offsets, sizes []*big.Int
	for i, n := range numbers {
		if i%2 == 0 {
			offsets = append(offsets, n)
		} else {
			sizes = append(sizes, n)
		}
	}
	next.sparse = tarPairs(offsets, sizes)
	return nil
}

// Overrides fields from pax records in record order, a bad number reading as zero except the GNU sparse sizes
func (t *tarInfo) applyPaxInfo(pax *tarPaxRecords) error {
	for _, keyword := range pax.keys {
		value := pax.values[keyword]
		switch keyword {
		case "GNU.sparse.name":
			t.name = value
		case "GNU.sparse.size", "GNU.sparse.realsize":
			n, err := tarPyInt(value, false)
			if err != nil {
				return err
			}
			t.size = n
		case "size":
			n, err := tarPyInt(value, false)
			if err != nil {
				n = new(big.Int)
			}
			t.size = n
		case "mtime":
			if f, ok := tarPyFloat(value); ok {
				t.mtime = tarMtime{isFloat: true, f: f}
			} else {
				t.mtime = tarMtime{i: new(big.Int)}
			}
		case "path":
			t.name = strings.TrimRight(value, "/")
		case "linkpath":
			t.linkname = value
		}
	}
	return nil
}

// Next member the way TarFile.next reads it, an unreadable header after the first ending the archive quietly
func (tf *tarFile) next() (*tarInfo, error) {
	if tf.firstmember != nil {
		m := tf.firstmember
		tf.firstmember = nil
		return m, nil
	}
	if tf.offset.Cmp(big.NewInt(tf.stream.tell())) != 0 {
		if tf.offset.Sign() == 0 {
			return nil, nil
		}
		if err := tf.stream.seek(new(big.Int).Sub(tf.offset, big.NewInt(1))); err != nil {
			return nil, err
		}
		probe, err := tf.stream.read(1)
		if err != nil {
			return nil, err
		}
		if len(probe) == 0 {
			return nil, tarReadError("unexpected end of data")
		}
	}
	t, err := tf.fromtarfile(true)
	if err != nil {
		var h *tarHeaderError
		var codec *TarReadError
		switch {
		case errors.As(err, &h):
			first := tf.offset.Sign() == 0
			switch h.class {
			case "EOFHeaderError":
			case "EmptyHeaderError":
				if first {
					return nil, tarReadError("empty file")
				}
			case "SubsequentHeaderError":
				return nil, tarReadError(h.msg)
			default:
				if first {
					return nil, tarReadError(h.msg)
				}
			}
			t = nil
		case errors.As(err, &codec) && codec.Class == "zlib.error":
			return nil, tarReadError("zlib error: " + codec.Message)
		default:
			return nil, err
		}
	}
	if t == nil {
		tf.loaded = true
		return nil, nil
	}
	tf.members = append(tf.members, t)
	return t, nil
}

// Visits members in archive order the way iterating a TarFile does, cached members first
func (tf *tarFile) each(visit func(*tarInfo) error) error {
	if tf.loaded {
		for i := 0; i < len(tf.members); i++ {
			if err := visit(tf.members[i]); err != nil {
				return err
			}
		}
		return nil
	}
	index := 0
	if tf.firstmember != nil {
		m, err := tf.next()
		if err != nil {
			return err
		}
		index++
		if err := visit(m); err != nil {
			return err
		}
	}
	for {
		var m *tarInfo
		switch {
		case index < len(tf.members):
			m = tf.members[index]
		case !tf.loaded:
			next, err := tf.next()
			if err != nil {
				return err
			}
			if next == nil {
				tf.loaded = true
				return nil
			}
			m = next
		default:
			return nil
		}
		index++
		if err := visit(m); err != nil {
			return err
		}
	}
}

// Every member, reading the rest of the archive first when it has not all been read
func (tf *tarFile) getmembers() ([]*tarInfo, error) {
	if !tf.loaded {
		for {
			m, err := tf.next()
			if err != nil {
				return nil, err
			}
			if m == nil {
				break
			}
		}
		tf.loaded = true
	}
	return tf.members, nil
}

// Python exception classes each open method turns into its not a compressed file ReadError
var tarMethodCatches = map[string]map[string]bool{
	"gz":  {"BadGzipFile": true, "OSError": true},
	"bz2": {"OSError": true, "EOFError": true},
	"xz":  {"LZMAError": true, "EOFError": true},
	"zst": {"ZstdError": true, "EOFError": true},
}

// ReadError text each open method gives an archive its codec refuses
var tarMethodRefusals = map[string]string{"gz": "not a gzip file", "bz2": "not a bzip2 file", "xz": "not an lzma file", "zst": "not a zstd file"}

// Reports the gzip, bzip2 or zstd frame magic, skippable zstd frames included, leading an archive
func tarMagicMatches(method string, head []byte) bool {
	switch method {
	case "gz":
		return bytes.HasPrefix(head, []byte{0x1f, 0x8b})
	case "bz2":
		return bytes.HasPrefix(head, []byte("BZh"))
	}
	skippable := len(head) >= 4 && head[0]&0xF0 == 0x50 && bytes.Equal(head[1:4], []byte{0x2A, 0x4D, 0x18})
	return skippable || bytes.HasPrefix(head, []byte{0x28, 0xB5, 0x2F, 0xFD})
}

// Opens an archive the way tarfile.open reads mode r, each compression tried in turn then the plain tar
func openTarFile(src io.ReadSeeker) (*tarFile, error) {
	start, err := src.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 6)
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	head = head[:n]
	_, isFile := src.(*os.File)
	var failures []string
	for _, method := range []string{"gz", "bz2", "xz", "zst", "tar"} {
		if method != "tar" && method != "xz" && !tarMagicMatches(method, head) {
			refusal := tarMethodRefusals[method]
			if method == "gz" && n == 0 {
				refusal = "empty file"
			}
			failures = append(failures, fmt.Sprintf("- method %s: ReadError(%s)", method, tarRepr(refusal)))
			continue
		}
		if _, err := src.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
		var stream tarStream = &tarRawStream{src: src, pos: start, file: isFile}
		if method != "tar" {
			stream = newTarCodecStream(src, method)
		}
		tf := &tarFile{stream: stream, offset: big.NewInt(stream.tell()), paxHeaders: newTarPaxRecords()}
		first, err := tf.next()
		if err == nil {
			tf.firstmember = first
			return tf, nil
		}
		stream.close()
		var read *TarReadError
		isRead := errors.As(err, &read)
		switch {
		case isRead && read.Class == "ReadError":
			failures = append(failures, fmt.Sprintf("- method %s: ReadError(%s)", method, tarRepr(read.Message)))
		case method != "tar" && ((isRead && tarMethodCatches[method][read.Class]) || (!isRead && (method == "gz" || method == "bz2"))):
			failures = append(failures, fmt.Sprintf("- method %s: ReadError(%s)", method, tarRepr(tarMethodRefusals[method])))
		default:
			return nil, err
		}
	}
	return nil, tarReadError("file could not be opened successfully:\n" + strings.Join(failures, "\n"))
}

// Releases the decompressor the archive was read through
func (tf *tarFile) close() { tf.stream.close() }
