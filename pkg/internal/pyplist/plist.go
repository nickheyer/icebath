package pyplist

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Error is a Python exception raised by plistlib.load, Class naming the exception type
type Error struct {
	Class string
	Msg   string
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return e.Class
	}
	return e.Class + ": " + e.Msg
}

// Dict is a plist dict keeping insertion order and Python key equality
type Dict struct {
	keys  []any
	vals  []any
	index map[string]int
}

// Value stored under a string key and whether it exists
func (d *Dict) Get(key string) (any, bool) {
	if i, ok := d.index["s:"+key]; ok {
		return d.vals[i], true
	}
	return nil, false
}

// Number of entries
func (d *Dict) Len() int { return len(d.keys) }

func (d *Dict) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for i := range d.keys {
			if !yield(d.keys[i], d.vals[i]) {
				return
			}
		}
	}
}

func (d *Dict) set(key, value any) error {
	h, ok := hashKey(key)
	if !ok {
		return &Error{Class: "TypeError", Msg: "unhashable type"}
	}
	if d.index == nil {
		d.index = map[string]int{}
	}
	if i, seen := d.index[h]; seen {
		d.vals[i] = value
		return nil
	}
	d.index[h] = len(d.keys)
	d.keys = append(d.keys, key)
	d.vals = append(d.vals, value)
	return nil
}

// Canonical hash text of a hashable Python value, equal values sharing one text
func hashKey(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "n", true
	case string:
		return "s:" + x, true
	case bool:
		if x {
			return "i:1", true
		}
		return "i:0", true
	case *big.Int:
		return "i:" + x.String(), true
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			bf, _ := new(big.Float).SetFloat64(x).Int(nil)
			return "i:" + bf.String(), true
		}
		return "f:" + strconv.FormatFloat(x, 'g', -1, 64), true
	case []byte:
		return "b:" + string(x), true
	case Date:
		return "d:" + time.Time(x).Format(time.RFC3339Nano), true
	case UID:
		return "u:" + strconv.FormatUint(uint64(x), 10), true
	}
	return "", false
}

// Date is a naive datetime.datetime read from a plist
type Date time.Time

func (d Date) PyStr() string {
	t := time.Time(d)
	s := t.Format("2006-01-02 15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func (d Date) PyRepr() string {
	t := time.Time(d)
	s := fmt.Sprintf("datetime.datetime(%d, %d, %d, %d, %d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute())
	us := t.Nanosecond() / 1000
	if t.Second() != 0 || us != 0 {
		s += fmt.Sprintf(", %d", t.Second())
	}
	if us != 0 {
		s += fmt.Sprintf(", %d", us)
	}
	return s + ")"
}

// UID is a keyed archiver reference from a binary plist
type UID uint64

func (u UID) PyRepr() string { return fmt.Sprintf("UID(%d)", uint64(u)) }
func (u UID) PyStr() string  { return u.PyRepr() }

// Str is Python str() of a decoded plist value
func Str(v any) string { return pyrepr.Str(v) }

// Decodes an XML or binary plist like plistlib.load with the default dict type
func Load(data []byte) (any, error) {
	header := data
	if len(header) > 32 {
		header = header[:32]
	}
	if isXML(header) {
		return parseXML(data)
	}
	if bytes.HasPrefix(header, []byte("bplist00")) {
		return parseBinary(data)
	}
	return nil, &Error{Class: "InvalidFileException", Msg: "Invalid file"}
}

var xmlBOMs = []struct {
	bom []byte
	enc func(string) []byte
}{
	{[]byte{0xEF, 0xBB, 0xBF}, func(s string) []byte { return []byte(s) }},
	{[]byte{0xFE, 0xFF}, func(s string) []byte { return encodeUTF16(s, binary.BigEndian) }},
	{[]byte{0xFF, 0xFE}, func(s string) []byte { return encodeUTF16(s, binary.LittleEndian) }},
	{[]byte{0x00, 0x00, 0xFE, 0xFF}, func(s string) []byte { return encodeUTF32(s, binary.BigEndian) }},
	{[]byte{0xFF, 0xFE, 0x00, 0x00}, func(s string) []byte { return encodeUTF32(s, binary.LittleEndian) }},
}

func encodeUTF16(s string, order binary.AppendByteOrder) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = order.AppendUint16(out, u)
	}
	return out
}

func encodeUTF32(s string, order binary.AppendByteOrder) []byte {
	var out []byte
	for _, r := range s {
		out = order.AppendUint32(out, uint32(r))
	}
	return out
}

// True when the header starts like an XML plist in any BOM marked encoding
func isXML(header []byte) bool {
	prefixes := []string{"<?xml", "<plist"}
	for _, p := range prefixes {
		if bytes.HasPrefix(header, []byte(p)) {
			return true
		}
	}
	for _, b := range xmlBOMs {
		if !bytes.HasPrefix(header, b.bom) {
			continue
		}
		for _, p := range prefixes {
			prefix := append(append([]byte{}, b.bom...), b.enc(p)...)
			if bytes.HasPrefix(header, prefix) {
				return true
			}
		}
	}
	return false
}

func expatError(msg string) error { return &Error{Class: "ExpatError", Msg: msg} }

// Transcodes a BOM marked UTF-16 document to UTF-8, failing like expat on UTF-32
func xmlUTF8(data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0x00, 0x00, 0xFE, 0xFF}), bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0x00, 0x00}):
		return nil, expatError("not well-formed (invalid token): line 1, column 0")
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}), bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		var order binary.ByteOrder = binary.BigEndian
		if data[0] == 0xFF {
			order = binary.LittleEndian
		}
		body := data[2:]
		if len(body)%2 != 0 {
			return nil, expatError("unclosed token: line 1, column 0")
		}
		units := make([]uint16, len(body)/2)
		for i := range units {
			units[i] = order.Uint16(body[2*i:])
		}
		text := string(utf16.Decode(units))
		return []byte(xmlEncodingDecl.ReplaceAllString(text, "$1")), nil
	}
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}), nil
}

var xmlEncodingDecl = regexp.MustCompile(`^(<\?xml[^>]*?)\s+encoding\s*=\s*["'][^"']*["']`)

type xmlParser struct {
	dec        *xml.Decoder
	root       any
	stack      []any
	currentKey *string
	data       strings.Builder
}

func (p *xmlParser) line() int {
	l, _ := p.dec.InputPos()
	return l
}

// Places a parsed value under the pending key, into the open array, or as the root
func (p *xmlParser) addObject(value any) error {
	if p.currentKey != nil {
		if len(p.stack) == 0 {
			return indexError()
		}
		d, ok := p.stack[len(p.stack)-1].(*Dict)
		if !ok {
			return &Error{Class: "ValueError", Msg: fmt.Sprintf("unexpected element at line %d", p.line())}
		}
		_ = d.set(*p.currentKey, value)
		p.currentKey = nil
		return nil
	}
	if len(p.stack) == 0 {
		p.root = value
		return nil
	}
	a, ok := p.stack[len(p.stack)-1].(*[]any)
	if !ok {
		return &Error{Class: "ValueError", Msg: fmt.Sprintf("unexpected element at line %d", p.line())}
	}
	*a = append(*a, value)
	return nil
}

func (p *xmlParser) getData() string {
	s := p.data.String()
	p.data.Reset()
	return s
}

func indexError() error { return &Error{Class: "IndexError", Msg: "list index out of range"} }

func (p *xmlParser) begin(name string) error {
	p.data.Reset()
	switch name {
	case "dict":
		d := &Dict{}
		if err := p.addObject(d); err != nil {
			return err
		}
		p.stack = append(p.stack, d)
	case "array":
		a := &[]any{}
		if err := p.addObject(a); err != nil {
			return err
		}
		p.stack = append(p.stack, a)
	}
	return nil
}

func (p *xmlParser) end(name string) error {
	switch name {
	case "dict":
		if p.currentKey != nil && *p.currentKey != "" {
			return &Error{Class: "ValueError", Msg: fmt.Sprintf("missing value for key '%s' at line %d", *p.currentKey, p.line())}
		}
		return p.pop()
	case "array":
		return p.pop()
	case "key":
		if p.currentKey != nil && *p.currentKey != "" {
			return &Error{Class: "ValueError", Msg: fmt.Sprintf("unexpected key at line %d", p.line())}
		}
		if len(p.stack) == 0 {
			return indexError()
		}
		if _, ok := p.stack[len(p.stack)-1].(*Dict); !ok {
			return &Error{Class: "ValueError", Msg: fmt.Sprintf("unexpected key at line %d", p.line())}
		}
		k := p.getData()
		p.currentKey = &k
	case "true":
		return p.addObject(true)
	case "false":
		return p.addObject(false)
	case "integer":
		raw := p.getData()
		var n *big.Int
		var ok bool
		if strings.HasPrefix(raw, "0x") || strings.HasPrefix(raw, "0X") {
			n, ok = pyint.ParseBase(raw, 16)
			if !ok {
				return &Error{Class: "ValueError", Msg: fmt.Sprintf("invalid literal for int() with base 16: %s", pyrepr.StrRepr(raw))}
			}
		} else if n, ok = pyint.Parse(raw); !ok {
			return &Error{Class: "ValueError", Msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.StrRepr(raw))}
		}
		return p.addObject(n)
	case "real":
		f, err := pyint.ParseFloat(p.getData())
		if err != nil {
			return &Error{Class: "ValueError", Msg: err.Error()}
		}
		return p.addObject(f)
	case "string":
		return p.addObject(p.getData())
	case "data":
		b, err := pybase64.B64Decode(p.getData())
		if err != nil {
			return &Error{Class: "Error", Msg: err.Error()}
		}
		return p.addObject(b)
	case "date":
		d, err := dateFromString(p.getData())
		if err != nil {
			return err
		}
		return p.addObject(d)
	}
	return nil
}

func (p *xmlParser) pop() error {
	if len(p.stack) == 0 {
		return &Error{Class: "IndexError", Msg: "pop from empty list"}
	}
	p.stack = p.stack[:len(p.stack)-1]
	return nil
}

var dateRe = regexp.MustCompile(`^([0-9]{4})(?:-([0-9]{2})(?:-([0-9]{2})(?:T([0-9]{2})(?::([0-9]{2})(?::([0-9]{2}))?)?)?)?)?Z`)

// Naive datetime of a plist date string with the errors datetime() raises
func dateFromString(s string) (Date, error) {
	m := dateRe.FindStringSubmatch(s)
	if m == nil {
		return Date{}, &Error{Class: "AttributeError", Msg: "'NoneType' object has no attribute 'groupdict'"}
	}
	var parts []int
	for _, g := range m[1:] {
		if g == "" {
			break
		}
		n, _ := strconv.Atoi(g)
		parts = append(parts, n)
	}
	if len(parts) < 3 {
		missing := []string{"'month'", "'day'"}[len(parts)-1:]
		if len(missing) == 1 {
			return Date{}, &Error{Class: "TypeError", Msg: "function missing required argument " + missing[0] + " (pos " + strconv.Itoa(len(parts)+1) + ")"}
		}
		return Date{}, &Error{Class: "TypeError", Msg: "function missing required argument 'month' (pos 2)"}
	}
	for len(parts) < 6 {
		parts = append(parts, 0)
	}
	if err := validDatetime(parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]); err != nil {
		return Date{}, err
	}
	return Date(time.Date(parts[0], time.Month(parts[1]), parts[2], parts[3], parts[4], parts[5], 0, time.UTC)), nil
}

func validDatetime(y, mo, d, h, mi, s int) error {
	switch {
	case y < 1 || y > 9999:
		return &Error{Class: "ValueError", Msg: fmt.Sprintf("year %d is out of range", y)}
	case mo < 1 || mo > 12:
		return &Error{Class: "ValueError", Msg: "month must be in 1..12"}
	case d < 1 || d > time.Date(y, time.Month(mo)+1, 0, 0, 0, 0, 0, time.UTC).Day():
		return &Error{Class: "ValueError", Msg: "day is out of range for month"}
	case h > 23:
		return &Error{Class: "ValueError", Msg: "hour must be in 0..23"}
	case mi > 59:
		return &Error{Class: "ValueError", Msg: "minute must be in 0..59"}
	case s > 59:
		return &Error{Class: "ValueError", Msg: "second must be in 0..59"}
	}
	return nil
}

// Parses an XML plist with the expat driven handler semantics of plistlib
func parseXML(data []byte) (any, error) {
	text, err := xmlUTF8(data)
	if err != nil {
		return nil, err
	}
	p := &xmlParser{dec: xml.NewDecoder(bytes.NewReader(text))}
	p.dec.Strict = true
	p.dec.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(label) {
		case "iso-8859-1", "latin-1", "latin1":
			raw, err := io.ReadAll(input)
			if err != nil {
				return nil, err
			}
			runes := make([]rune, len(raw))
			for i, c := range raw {
				runes[i] = rune(c)
			}
			return strings.NewReader(string(runes)), nil
		case "us-ascii", "ascii", "utf-16", "utf-16le", "utf-16be":
			return input, nil
		}
		return nil, expatError("unknown encoding: line 1, column 30")
	}
	sawRoot := false
	for {
		tok, err := p.dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *Error
			if errors.As(err, &pe) {
				return nil, pe
			}
			return nil, expatError(err.Error())
		}
		switch t := tok.(type) {
		case xml.StartElement:
			sawRoot = true
			if err := p.begin(t.Name.Local); err != nil {
				return nil, err
			}
		case xml.EndElement:
			if err := p.end(t.Name.Local); err != nil {
				return nil, err
			}
		case xml.CharData:
			if !utf8.Valid(t) {
				return nil, expatError("not well-formed (invalid token)")
			}
			p.data.Write(t)
		case xml.Directive:
			if bytes.Contains(t, []byte("<!ENTITY")) {
				return nil, &Error{Class: "InvalidFileException", Msg: "XML entity declarations are not supported in plist files"}
			}
		}
	}
	if !sawRoot {
		return nil, expatError("no element found")
	}
	return unwrapArrays(p.root), nil
}

// Replaces the array pointers the parser grows in place with plain slices
func unwrapArrays(v any) any {
	switch x := v.(type) {
	case *[]any:
		out := *x
		for i := range out {
			out[i] = unwrapArrays(out[i])
		}
		return out
	case *Dict:
		for i := range x.vals {
			x.vals[i] = unwrapArrays(x.vals[i])
		}
	}
	return v
}

func invalidFile() error { return &Error{Class: "InvalidFileException", Msg: "Invalid file"} }

type binParser struct {
	data    []byte
	pos     int
	refSize int
	offsets []uint64
	objects []any
	done    []bool
}

func (b *binParser) read(n int) []byte {
	if b.pos >= len(b.data) || n <= 0 {
		return nil
	}
	end := b.pos + n
	if end > len(b.data) || end < b.pos {
		end = len(b.data)
	}
	out := b.data[b.pos:end]
	b.pos = end
	return out
}

func (b *binParser) seek(off uint64) error {
	if off > math.MaxInt64 {
		return invalidFile()
	}
	b.pos = int(min(off, uint64(len(b.data))+1))
	return nil
}

// Reads n big endian unsigned integers of the given byte width
func (b *binParser) readInts(n uint64, size int) ([]uint64, error) {
	if size <= 0 || n > uint64(len(b.data)) {
		return nil, invalidFile()
	}
	data := b.read(int(n) * size)
	if len(data) != int(n)*size {
		return nil, invalidFile()
	}
	out := make([]uint64, n)
	for i := range out {
		chunk := data[i*size : (i+1)*size]
		v := new(big.Int).SetBytes(chunk)
		if !v.IsUint64() {
			out[i] = math.MaxUint64
			continue
		}
		out[i] = v.Uint64()
	}
	return out, nil
}

func (b *binParser) getSize(tokenL byte) (uint64, error) {
	if tokenL != 0xF {
		return uint64(tokenL), nil
	}
	m := b.read(1)
	if len(m) != 1 {
		return 0, invalidFile()
	}
	s := 1 << (m[0] & 0x3)
	raw := b.read(s)
	if len(raw) != s {
		return 0, invalidFile()
	}
	return new(big.Int).SetBytes(raw).Uint64(), nil
}

// Parses a bplist00 document the way plistlib's binary reader does
func parseBinary(data []byte) (any, error) {
	if len(data) < 32 {
		return nil, invalidFile()
	}
	trailer := data[len(data)-32:]
	offsetSize := int(trailer[6])
	b := &binParser{data: data, refSize: int(trailer[7])}
	numObjects := binary.BigEndian.Uint64(trailer[8:])
	top := binary.BigEndian.Uint64(trailer[16:])
	if err := b.seek(binary.BigEndian.Uint64(trailer[24:])); err != nil {
		return nil, err
	}
	offsets, err := b.readInts(numObjects, offsetSize)
	if err != nil {
		return nil, err
	}
	b.offsets = offsets
	b.objects = make([]any, numObjects)
	b.done = make([]bool, numObjects)
	v, err := b.readObject(top)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) && pe.Class == "RecursionError" {
			return nil, err
		}
		return nil, invalidFile()
	}
	return unwrapArrays(v), nil
}

func (b *binParser) readObject(ref uint64) (any, error) {
	if ref >= uint64(len(b.objects)) {
		return nil, invalidFile()
	}
	if b.done[ref] {
		return b.objects[ref], nil
	}
	if err := b.seek(b.offsets[ref]); err != nil {
		return nil, err
	}
	tok := b.read(1)
	if len(tok) != 1 {
		return nil, invalidFile()
	}
	token := tok[0]
	tokenH, tokenL := token&0xF0, token&0x0F
	var result any
	switch {
	case token == 0x00:
		result = nil
	case token == 0x08:
		result = false
	case token == 0x09:
		result = true
	case token == 0x0f:
		result = []byte{}
	case tokenH == 0x10:
		raw := b.read(1 << tokenL)
		n := new(big.Int).SetBytes(raw)
		if tokenL >= 3 && len(raw) > 0 && raw[0]&0x80 != 0 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(len(raw))*8))
		}
		result = n
	case token == 0x22:
		raw := b.read(4)
		if len(raw) != 4 {
			return nil, invalidFile()
		}
		result = float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))
	case token == 0x23, token == 0x33:
		raw := b.read(8)
		if len(raw) != 8 {
			return nil, invalidFile()
		}
		f := math.Float64frombits(binary.BigEndian.Uint64(raw))
		result = f
		if token == 0x33 {
			d, err := appleEpochDate(f)
			if err != nil {
				return nil, err
			}
			result = d
		}
	case tokenH == 0x40, tokenH == 0x50, tokenH == 0x60:
		s, err := b.getSize(tokenL)
		if err != nil {
			return nil, err
		}
		if tokenH == 0x60 {
			s *= 2
		}
		if s > uint64(len(b.data)) {
			return nil, invalidFile()
		}
		raw := b.read(int(s))
		if uint64(len(raw)) != s {
			return nil, invalidFile()
		}
		switch tokenH {
		case 0x40:
			result = append([]byte{}, raw...)
		case 0x50:
			for _, c := range raw {
				if c >= 0x80 {
					return nil, invalidFile()
				}
			}
			result = string(raw)
		default:
			units := make([]uint16, len(raw)/2)
			for i := range units {
				units[i] = binary.BigEndian.Uint16(raw[2*i:])
			}
			for i := 0; i < len(units); i++ {
				if utf16.IsSurrogate(rune(units[i])) {
					if units[i] >= 0xDC00 || i+1 >= len(units) || units[i+1] < 0xDC00 || units[i+1] > 0xDFFF {
						return nil, invalidFile()
					}
					i++
				}
			}
			result = string(utf16.Decode(units))
		}
	case tokenH == 0x80:
		raw := b.read(int(tokenL) + 1)
		n := new(big.Int).SetBytes(raw)
		if !n.IsUint64() {
			return nil, invalidFile()
		}
		result = UID(n.Uint64())
	case tokenH == 0xA0:
		s, err := b.getSize(tokenL)
		if err != nil {
			return nil, err
		}
		refs, err := b.readInts(s, b.refSize)
		if err != nil {
			return nil, err
		}
		arr := &[]any{}
		b.objects[ref], b.done[ref] = arr, true
		for _, r := range refs {
			v, err := b.readObject(r)
			if err != nil {
				return nil, err
			}
			*arr = append(*arr, v)
		}
		result = arr
	case tokenH == 0xD0:
		s, err := b.getSize(tokenL)
		if err != nil {
			return nil, err
		}
		keyRefs, err := b.readInts(s, b.refSize)
		if err != nil {
			return nil, err
		}
		objRefs, err := b.readInts(s, b.refSize)
		if err != nil {
			return nil, err
		}
		d := &Dict{}
		b.objects[ref], b.done[ref] = d, true
		for i := range keyRefs {
			k, err := b.readObject(keyRefs[i])
			if err != nil {
				return nil, err
			}
			v, err := b.readObject(objRefs[i])
			if err != nil {
				return nil, err
			}
			if err := d.set(k, v); err != nil {
				return nil, invalidFile()
			}
		}
		result = d
	default:
		return nil, invalidFile()
	}
	b.objects[ref], b.done[ref] = result, true
	return result, nil
}

// datetime(2001, 1, 1) plus timedelta(seconds=f) rounded to microseconds half to even
func appleEpochDate(f float64) (Date, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return Date{}, invalidFile()
	}
	us := math.RoundToEven(f * 1e6)
	secs := math.Floor(us / 1e6)
	if math.Abs(secs) > 4e11 {
		return Date{}, invalidFile()
	}
	base := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	t := time.Unix(base+int64(secs), int64(us-secs*1e6)*1000).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return Date{}, invalidFile()
	}
	return Date(t), nil
}
