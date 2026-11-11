package pymultipart

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	cr     = '\r'
	lf     = '\n'
	space  = ' '
	hyphen = '-'
	amp    = '&'
)

const tokenChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#$%&'*+-.^_`|~"

// Defaults of a multipart part header count and line size and the longest boundary accepted
const (
	DefaultMaxHeaderCount = 8
	DefaultMaxHeaderSize  = 4096 + 128
	MaxBoundaryLength     = 256
)

const (
	flagPartBoundary = 1
	flagLastBoundary = 2
)

const (
	qsBeforeField = iota
	qsFieldName
	qsFieldData
)

const (
	mpStart = iota
	mpStartBoundary
	mpHeaderFieldStart
	mpHeaderField
	mpHeaderValueStart
	mpHeaderValue
	mpHeaderValueAlmostDone
	mpHeadersAlmostDone
	mpPartDataStart
	mpPartData
	mpPartDataEnd
	mpEndBoundary
	mpEnd
)

// Parse failure of the form parsers, Class naming the python_multipart exception
type FormParserError struct {
	Class  string
	Msg    string
	Offset int
}

func (e *FormParserError) Error() string      { return e.Msg }
func (e *FormParserError) PyTypeName() string { return e.Class }

// Value error of a parser constructed with a size below one
type ValueError struct{ Msg string }

func (e *ValueError) Error() string      { return e.Msg }
func (e *ValueError) PyTypeName() string { return "ValueError" }

type raised struct{ err error }

func parserLog() *slog.Logger {
	return slog.Default().With("logger", "python_multipart.multipart")
}

// Python slice data[start:end] with both bounds clamped to the data
func pySlice(data []byte, start, end int) []byte {
	if end > len(data) {
		end = len(data)
	}
	if start > end {
		return nil
	}
	return data[start:end]
}

// Python index normalization of a slice bound against a length
func pyBound(i, n int) int {
	if i < 0 {
		i += n
		if i < 0 {
			i = 0
		}
	}
	if i > n {
		i = n
	}
	return i
}

// Python str.find with start and end bounds, minus one when absent
func pyFind(s, sub string, start, end int) int {
	start, end = pyBound(start, len(s)), pyBound(end, len(s))
	if start > end {
		return -1
	}
	j := strings.Index(s[start:end], sub)
	if j < 0 {
		return -1
	}
	return start + j
}

// Python str.count with start and end bounds
func pyCount(s, sub string, start, end int) int {
	start, end = pyBound(start, len(s)), pyBound(end, len(s))
	if start > end {
		return 0
	}
	return strings.Count(s[start:end], sub)
}

// Semicolon separated parts of a header, a semicolon inside a quoted string kept, keys lowercased
func parseParam(s string) []string {
	s = ";" + s
	var plist []string
	start := 0
	for pyFind(s, ";", start, len(s)) == start {
		start++
		end := pyFind(s, ";", start, len(s))
		ind, diff := start, 0
		for end > 0 {
			diff += pyCount(s, `"`, ind, end) - pyCount(s, `\"`, ind, end)
			if diff%2 == 0 {
				break
			}
			end, ind = ind, pyFind(s, ";", end+1, len(s))
		}
		if end < 0 {
			end = len(s)
		}
		i := pyFind(s, "=", start, end)
		var f string
		if i == -1 {
			f = s[start:end]
		} else {
			f = pystr.Lower(pystr.Rstrip(s[start:i])) + "=" + pystr.Lstrip(s[i+1:end])
		}
		plist = append(plist, pystr.Strip(f))
		start = end
	}
	return plist
}

// Latin-1 bytes of text whose characters are all below 256
func latin1(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteByte(byte(r))
	}
	return b.String()
}

// Content type and parameters of a header, both as latin-1 bytes, extended star parameters ignored
func ParseOptionsHeader(value string) (string, map[string]string) {
	if value == "" {
		return "", map[string]string{}
	}
	if !strings.Contains(value, ";") {
		return latin1(pystr.Strip(pystr.Lower(value))), map[string]string{}
	}
	parts := parseParam(value)
	ctype, segments := parts[0], parts[1:]
	options := map[string]string{}
	for _, segment := range segments {
		key, val, _ := strings.Cut(segment, "=")
		if strings.Contains(key, "*") {
			continue
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = strings.ReplaceAll(strings.ReplaceAll(val[1:len(val)-1], `\\`, `\`), `\"`, `"`)
		}
		if key == "filename" {
			runes := []rune(val)
			if (len(runes) >= 3 && string(runes[1:3]) == `:\`) || strings.HasPrefix(val, `\\`) {
				pieces := strings.Split(val, `\`)
				val = pieces[len(pieces)-1]
			}
		}
		options[latin1(key)] = latin1(val)
	}
	return latin1(ctype), options
}

// Callbacks of the querystring parser, a nil callback skipped
type QuerystringCallbacks struct {
	OnFieldStart func() error
	OnFieldName  func(data []byte) error
	OnFieldData  func(data []byte) error
	OnFieldEnd   func() error
	OnEnd        func() error
}

// Streaming application/x-www-form-urlencoded parser
type QuerystringParser struct {
	callbacks     QuerystringCallbacks
	state         int
	foundSep      bool
	maxSize       float64
	currentSize   int
	strictParsing bool
}

// Querystring parser over callbacks, a ValueError for a max size below one
func NewQuerystringParser(callbacks QuerystringCallbacks, strictParsing bool, maxSize float64) (*QuerystringParser, error) {
	if maxSize < 1 || math.IsNaN(maxSize) {
		return nil, &ValueError{Msg: fmt.Sprintf("max_size must be a positive number, not %v", maxSize)}
	}
	return &QuerystringParser{callbacks: callbacks, maxSize: maxSize, strictParsing: strictParsing}, nil
}

// Calls the named querystring callback, a data callback skipped when start equals end
func (p *QuerystringParser) callback(name string, data []byte, start, end int) {
	var err error
	switch name {
	case "field_start":
		if p.callbacks.OnFieldStart != nil {
			err = p.callbacks.OnFieldStart()
		}
	case "field_end":
		if p.callbacks.OnFieldEnd != nil {
			err = p.callbacks.OnFieldEnd()
		}
	case "end":
		if p.callbacks.OnEnd != nil {
			err = p.callbacks.OnEnd()
		}
	case "field_name", "field_data":
		fn := p.callbacks.OnFieldName
		if name == "field_data" {
			fn = p.callbacks.OnFieldData
		}
		if fn == nil || start == end {
			return
		}
		err = fn(pySlice(data, start, end))
	}
	if err != nil {
		panic(raised{err})
	}
}

// Recovers a callback or parse error raised inside a write into err
func recoverRaised(err *error) {
	if r := recover(); r != nil {
		rv, ok := r.(raised)
		if !ok {
			panic(r)
		}
		*err = rv.err
	}
}

// Feeds data, truncated past the max size, returning the bytes consumed or the raised error
func (p *QuerystringParser) Write(data []byte) (n int, err error) {
	dataLen := len(data)
	if float64(p.currentSize+dataLen) > p.maxSize {
		newSize := int(p.maxSize - float64(p.currentSize))
		parserLog().Warn(fmt.Sprintf("Current size is %d (max %d), so truncating data length from %d to %d",
			p.currentSize, int64(p.maxSize), dataLen, newSize))
		dataLen = newSize
	}
	defer func() { p.currentSize += n }()
	defer recoverRaised(&err)
	n = p.internalWrite(data, dataLen)
	return n, nil
}

func (p *QuerystringParser) internalWrite(data []byte, length int) int {
	state := p.state
	strictParsing := p.strictParsing
	foundSep := p.foundSep
	i := 0
	for i < length {
		ch := data[i]
		switch state {
		case qsBeforeField:
			if ch == amp {
				if foundSep {
					if strictParsing {
						panic(raised{&FormParserError{Class: "QuerystringParseError", Msg: fmt.Sprintf("Skipping duplicate ampersand at %d", i), Offset: i}})
					}
					parserLog().Debug(fmt.Sprintf("Skipping duplicate ampersand at %d", i))
				} else {
					foundSep = true
				}
			} else {
				p.callback("field_start", nil, 0, 0)
				i--
				state = qsFieldName
				foundSep = false
			}
		case qsFieldName:
			sepPos := bytesFind(data, "&", i, length)
			var equalsPos int
			if sepPos != -1 {
				equalsPos = bytesFind(data, "=", i, sepPos)
			} else {
				equalsPos = bytesFind(data, "=", i, length)
			}
			if equalsPos != -1 {
				p.callback("field_name", data, i, equalsPos)
				i = equalsPos
				state = qsFieldData
			} else if !strictParsing {
				if sepPos != -1 {
					p.callback("field_name", data, i, sepPos)
					p.callback("field_end", nil, 0, 0)
					i = sepPos - 1
					state = qsBeforeField
				} else {
					p.callback("field_name", data, i, length)
					i = length
				}
			} else {
				if sepPos != -1 {
					panic(raised{&FormParserError{Class: "QuerystringParseError", Msg: fmt.Sprintf("When strict_parsing is True, we require an "+
						"equals sign in all field chunks. Did not find one in the chunk that starts at %d", i), Offset: i}})
				}
				p.callback("field_name", data, i, length)
				i = length
			}
		case qsFieldData:
			sepPos := bytesFind(data, "&", i, length)
			if sepPos != -1 {
				p.callback("field_data", data, i, sepPos)
				p.callback("field_end", nil, 0, 0)
				i = sepPos - 1
				state = qsBeforeField
			} else {
				p.callback("field_data", data, i, length)
				i = length
			}
		default:
			msg := fmt.Sprintf("Reached an unknown state %d at %d", state, i)
			parserLog().Warn(msg)
			panic(raised{&FormParserError{Class: "QuerystringParseError", Msg: msg, Offset: i}})
		}
		i++
	}
	p.state = state
	p.foundSep = foundSep
	return length
}

// Ends a field still open, then signals the end of parsing
func (p *QuerystringParser) Finalize() (err error) {
	defer recoverRaised(&err)
	if p.state == qsFieldData || p.state == qsFieldName {
		p.callback("field_end", nil, 0, 0)
	}
	p.callback("end", nil, 0, 0)
	return nil
}

// Python bytes.find of sub within data[start:end], minus one when absent
func bytesFind(data []byte, sub string, start, end int) int {
	start, end = pyBound(start, len(data)), pyBound(end, len(data))
	if start > end {
		return -1
	}
	j := bytes.Index(data[start:end], []byte(sub))
	if j < 0 {
		return -1
	}
	return start + j
}

// Python bytes.rfind of sub within data[start:end], minus one when absent
func bytesRfind(data []byte, sub []byte, start, end int) int {
	start, end = pyBound(start, len(data)), pyBound(end, len(data))
	if start > end {
		return -1
	}
	j := bytes.LastIndex(data[start:end], sub)
	if j < 0 {
		return -1
	}
	return start + j
}

// Callbacks of the multipart parser, a nil callback skipped
type MultipartCallbacks struct {
	OnPartBegin       func() error
	OnPartData        func(data []byte) error
	OnPartEnd         func() error
	OnHeaderBegin     func() error
	OnHeaderField     func(data []byte) error
	OnHeaderValue     func(data []byte) error
	OnHeaderEnd       func() error
	OnHeadersFinished func() error
	OnEnd             func() error
}

// Streaming multipart/form-data parser
type MultipartParser struct {
	callbacks          MultipartCallbacks
	state              int
	index              int
	flags              int
	maxSize            float64
	currentSize        int
	maxHeaderCount     int
	currentHeaderCount int
	maxHeaderSize      int
	currentHeaderSize  int
	marks              map[string]int
	boundary           []byte
}

// Multipart parser of a boundary, a ValueError for a max size below one and a FormParserError for a long boundary
func NewMultipartParser(boundary []byte, callbacks MultipartCallbacks, maxSize float64, maxHeaderCount, maxHeaderSize int) (*MultipartParser, error) {
	if maxSize < 1 || math.IsNaN(maxSize) {
		return nil, &ValueError{Msg: fmt.Sprintf("max_size must be a positive number, not %v", maxSize)}
	}
	if len(boundary) > MaxBoundaryLength {
		return nil, &FormParserError{Class: "FormParserError", Msg: fmt.Sprintf("Boundary length %d exceeds maximum of %d", len(boundary), MaxBoundaryLength), Offset: -1}
	}
	return &MultipartParser{callbacks: callbacks, maxSize: maxSize, maxHeaderCount: maxHeaderCount, maxHeaderSize: maxHeaderSize,
		marks: map[string]int{}, boundary: append([]byte("\r\n--"), boundary...)}, nil
}

// Calls the named multipart callback, a data callback skipped when start equals end
func (p *MultipartParser) callback(name string, data []byte, start, end int) {
	var notify func() error
	var dataFn func([]byte) error
	switch name {
	case "part_begin":
		notify = p.callbacks.OnPartBegin
	case "part_end":
		notify = p.callbacks.OnPartEnd
	case "header_begin":
		notify = p.callbacks.OnHeaderBegin
	case "header_end":
		notify = p.callbacks.OnHeaderEnd
	case "headers_finished":
		notify = p.callbacks.OnHeadersFinished
	case "end":
		notify = p.callbacks.OnEnd
	case "part_data":
		dataFn = p.callbacks.OnPartData
	case "header_field":
		dataFn = p.callbacks.OnHeaderField
	case "header_value":
		dataFn = p.callbacks.OnHeaderValue
	}
	var err error
	switch name {
	case "part_data", "header_field", "header_value":
		if dataFn == nil || start == end {
			return
		}
		err = dataFn(pySlice(data, start, end))
	default:
		if notify != nil {
			err = notify()
		}
	}
	if err != nil {
		panic(raised{err})
	}
}

func (p *MultipartParser) fail(msg string, offset int, warn bool) {
	if warn {
		parserLog().Warn(msg)
	}
	panic(raised{&FormParserError{Class: "MultipartParseError", Msg: msg, Offset: offset}})
}

// Feeds data, truncated past the max size, returning the bytes consumed or the raised error
func (p *MultipartParser) Write(data []byte) (n int, err error) {
	dataLen := len(data)
	if float64(p.currentSize+dataLen) > p.maxSize {
		newSize := int(p.maxSize - float64(p.currentSize))
		parserLog().Warn(fmt.Sprintf("Current size is %d (max %d), so truncating data length from %d to %d",
			p.currentSize, int64(p.maxSize), dataLen, newSize))
		dataLen = newSize
	}
	defer func() { p.currentSize += n }()
	defer recoverRaised(&err)
	n = p.internalWrite(data, dataLen)
	return n, nil
}

func (p *MultipartParser) internalWrite(data []byte, length int) int {
	boundary := p.boundary
	boundaryLength := len(boundary)
	state := p.state
	index := p.index
	flags := p.flags
	currentHeaderCount := p.currentHeaderCount
	currentHeaderSize := p.currentHeaderSize
	i := 0
	advanceHeaderSize := func(amount int) {
		currentHeaderSize += amount
		if currentHeaderSize > p.maxHeaderSize {
			p.fail("Maximum header size exceeded", i, false)
		}
	}
	setMark := func(name string) { p.marks[name] = i }
	deleteMark := func(name string) { delete(p.marks, name) }
	dataCallback := func(name string, endI int, remaining bool) {
		markedIndex, ok := p.marks[name]
		if !ok {
			return
		}
		if endI <= markedIndex {
		} else if markedIndex >= 0 {
			p.callback(name, data, markedIndex, endI)
		} else {
			lookbehindLen := -markedIndex
			if lookbehindLen <= boundaryLength {
				p.callback(name, boundary, 0, lookbehindLen)
			} else if p.flags&flagPartBoundary != 0 {
				p.callback(name, append(append([]byte{}, boundary...), "\r\n"...), 0, lookbehindLen)
			} else if p.flags&flagLastBoundary != 0 {
				p.callback(name, append(append([]byte{}, boundary...), "--\r\n"...), 0, lookbehindLen)
			} else {
				parserLog().Warn("Look-back buffer error")
			}
			if endI > 0 {
				p.callback(name, data, 0, endI)
			}
		}
		if remaining {
			p.marks[name] = endI - length
		} else {
			delete(p.marks, name)
		}
	}
loop:
	for i < length {
		c := data[i]
		switch state {
		case mpStart:
			if c == cr || c == lf {
				i = bytesFind(data, "-", i, len(data))
				if i == -1 {
					i = length
					break loop
				}
				continue
			}
			index = 0
			state = mpStartBoundary
			i--
		case mpStartBoundary:
			if index == boundaryLength-2 {
				if c == hyphen {
					state = mpEndBoundary
				} else if c != cr {
					p.fail(fmt.Sprintf("Did not find CR at end of boundary (%d)", i), i, true)
				}
				index++
			} else if index == boundaryLength-1 {
				if c != lf {
					p.fail(fmt.Sprintf("Did not find LF at end of boundary (%d)", i), i, true)
				}
				index = 0
				p.callback("part_begin", nil, 0, 0)
				currentHeaderCount = 0
				currentHeaderSize = 0
				state = mpHeaderFieldStart
			} else {
				if c != boundary[index+2] {
					p.fail(fmt.Sprintf("Expected boundary character %d, got %d at index %d", boundary[index+2], c, index+2), i, true)
				}
				index++
			}
		case mpHeaderFieldStart:
			index = 0
			if c != cr {
				currentHeaderCount++
				if currentHeaderCount > p.maxHeaderCount {
					p.fail("Maximum header count exceeded", i, false)
				}
				currentHeaderSize = 0
			}
			setMark("header_field")
			if c != cr {
				p.callback("header_begin", nil, 0, 0)
			}
			state = mpHeaderField
			i--
		case mpHeaderField:
			if c == cr && index == 0 {
				deleteMark("header_field")
				state = mpHeadersAlmostDone
				i++
				continue
			}
			colon := bytesFind(data, ":", i, length)
			end := length
			if colon != -1 {
				end = colon
			}
			if colon == -1 {
				advanceHeaderSize(end - i)
			} else {
				advanceHeaderSize(end - i + 1)
			}
			field := data[i:end]
			for k, b := range field {
				if strings.IndexByte(tokenChars, b) < 0 {
					badI := i + k
					p.fail(fmt.Sprintf("Found invalid character %d in header at %d", b, badI), badI, true)
				}
			}
			index += end - i
			if colon == -1 {
				i = length
			} else {
				if index == 0 {
					p.fail(fmt.Sprintf("Found 0-length header at %d", i), i, true)
				}
				i = colon
				dataCallback("header_field", i, false)
				state = mpHeaderValueStart
			}
		case mpHeaderValueStart:
			if c == space {
				advanceHeaderSize(1)
				i++
				continue
			}
			setMark("header_value")
			state = mpHeaderValue
			i--
		case mpHeaderValue:
			crPos := bytesFind(data, "\r", i, length)
			end := length
			if crPos != -1 {
				end = crPos
			}
			advanceHeaderSize(end - i)
			if crPos != -1 {
				i = crPos
				dataCallback("header_value", i, false)
				p.callback("header_end", nil, 0, 0)
				currentHeaderSize = 0
				state = mpHeaderValueAlmostDone
			} else {
				i = length
			}
		case mpHeaderValueAlmostDone:
			if c != lf {
				p.fail(fmt.Sprintf("Did not find LF character at end of header (found %d)", c), i, true)
			}
			state = mpHeaderFieldStart
		case mpHeadersAlmostDone:
			if c != lf {
				p.fail(fmt.Sprintf("Did not find LF at end of headers (found %d)", c), i, true)
			}
			p.callback("headers_finished", nil, 0, 0)
			state = mpPartDataStart
		case mpPartDataStart:
			setMark("part_data")
			state = mpPartData
			i--
		case mpPartData:
			prevIndex := index
			if index == 0 {
				i0 := bytesFind(data, string(boundary), i, length)
				if i0 >= 0 {
					index = boundaryLength - 1
					i = i0 + boundaryLength - 1
					c = data[i]
				} else {
					k := bytesRfind(data, boundary[:1], max(i, length-boundaryLength+1), length)
					if k != -1 && bytes.HasPrefix(boundary, data[k:length]) {
						index = length - k
					}
					i = length
					continue
				}
			}
			if index < boundaryLength {
				if boundary[index] == c {
					index++
				} else {
					index = 0
				}
			} else if index == boundaryLength {
				index++
				if c == cr {
					flags |= flagPartBoundary
				} else if c == hyphen {
					flags |= flagLastBoundary
				} else {
					index = 0
				}
			} else if index == boundaryLength+1 {
				if flags&flagPartBoundary != 0 {
					if c == lf {
						flags &^= flagPartBoundary
						dataCallback("part_data", i-index, false)
						p.callback("part_end", nil, 0, 0)
						p.callback("part_begin", nil, 0, 0)
						currentHeaderCount = 0
						currentHeaderSize = 0
						index = 0
						state = mpHeaderFieldStart
						i++
						continue
					}
					index = 0
					flags &^= flagPartBoundary
				} else if flags&flagLastBoundary != 0 {
					if c == hyphen {
						dataCallback("part_data", i-index, false)
						p.callback("part_end", nil, 0, 0)
						p.callback("end", nil, 0, 0)
						state = mpEnd
					} else {
						index = 0
					}
				}
			}
			if index == 0 && prevIndex > 0 {
				i--
			}
		case mpEndBoundary:
			if index == boundaryLength-1 {
				if c != hyphen {
					p.fail(fmt.Sprintf("Did not find - at end of boundary (%d)", i), i, true)
				}
				index++
				p.callback("end", nil, 0, 0)
				state = mpEnd
			}
		case mpEnd:
			i = length
			break loop
		default:
			p.fail(fmt.Sprintf("Reached an unknown state %d at %d", state, i), i, true)
		}
		i++
	}
	dataCallback("header_field", length, true)
	dataCallback("header_value", length, true)
	dataCallback("part_data", length-index, true)
	p.state = state
	p.index = index
	p.flags = flags
	p.currentHeaderCount = currentHeaderCount
	p.currentHeaderSize = currentHeaderSize
	return length
}

// Signals the end of the input, the multipart parser checking nothing further
func (p *MultipartParser) Finalize() error { return nil }
