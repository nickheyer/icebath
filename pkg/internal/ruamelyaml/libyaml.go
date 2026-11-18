package ruamelyaml

import (
	"bytes"
	"math"
	"strings"
	"unicode/utf8"
)

// Derived from libyaml 0.1.7 as bundled in ruamel.yaml.clib 0.2.15, MIT license, Kirill Simonov and Ingy dot Net

const (
	libyamlInputRawBufferSize = 16384
	libyamlInputBufferSize    = libyamlInputRawBufferSize * 3
	libyamlMaxFileSize        = math.MaxInt64
	libyamlInitialStackSize   = 16
	libyamlInitialStringSize  = 16
	libyamlMaxNumberLength    = 9
	libyamlSizeofInt          = 4
	libyamlSizeofMark         = 24
	libyamlSizeofSimpleKey    = 40
	libyamlSizeofState        = 4
	libyamlSizeofTagDirective = 16
)

// Bytes a libyaml stack may reach before growth fails with MemoryError, lowered by the scaled stack limit test
var libyamlStackByteLimit = math.MaxInt32 / 2

type yamlMark struct {
	index, line, column int
}

// Collection start position kept on the marks stack, which only ever renders line and column
type yamlStackMark struct {
	line, column int
}

type yamlErrorType int8

const (
	yamlNoError yamlErrorType = iota
	yamlMemoryError
	yamlReaderError
	yamlScannerError
	yamlParserError
)

type yamlEncoding int8

const (
	yamlAnyEncoding yamlEncoding = iota
	yamlUTF8Encoding
	yamlUTF16LEEncoding
	yamlUTF16BEEncoding
)

type yamlTokenType int8

const (
	yamlNoToken yamlTokenType = iota
	yamlStreamStartToken
	yamlStreamEndToken
	yamlVersionDirectiveToken
	yamlTagDirectiveToken
	yamlDocumentStartToken
	yamlDocumentEndToken
	yamlBlockSequenceStartToken
	yamlBlockMappingStartToken
	yamlBlockEndToken
	yamlFlowSequenceStartToken
	yamlFlowSequenceEndToken
	yamlFlowMappingStartToken
	yamlFlowMappingEndToken
	yamlBlockEntryToken
	yamlFlowEntryToken
	yamlKeyToken
	yamlValueToken
	yamlAliasToken
	yamlAnchorToken
	yamlTagToken
	yamlScalarToken
)

// Scanner output unit with the scalar, alias or anchor value, tag handle and suffix, and version numbers
type yamlToken struct {
	typ       yamlTokenType
	startMark yamlMark
	style     yamlScalarStyle
	value     []byte
	handle    []byte
	suffix    []byte
	major     int32
	minor     int32
}

type yamlScalarStyle int8

const (
	yamlAnyScalarStyle yamlScalarStyle = iota
	yamlPlainScalarStyle
	yamlSingleQuotedScalarStyle
	yamlDoubleQuotedScalarStyle
	yamlLiteralScalarStyle
	yamlFoldedScalarStyle
)

type yamlEventType int8

const (
	yamlNoEvent yamlEventType = iota
	yamlStreamStartEvent
	yamlStreamEndEvent
	yamlDocumentStartEvent
	yamlDocumentEndEvent
	yamlAliasEvent
	yamlScalarEvent
	yamlSequenceStartEvent
	yamlSequenceEndEvent
	yamlMappingStartEvent
	yamlMappingEndEvent
)

// Parser output with the strings the pyx layer decodes while building its Python object
type yamlEvent struct {
	typ            yamlEventType
	startMark      yamlMark
	style          yamlScalarStyle
	plainImplicit  bool
	quotedImplicit bool
	flow           bool
	anchor        []byte
	tag           []byte
	value         []byte
	tagDirectives []yamlTagDirective
}

type yamlTagDirective struct {
	handle []byte
	prefix []byte
}

type yamlSimpleKey struct {
	possible    bool
	required    bool
	tokenNumber int
	mark        yamlMark
}

type yamlParserState int8

const (
	yamlParseStreamStartState yamlParserState = iota
	yamlParseImplicitDocumentStartState
	yamlParseDocumentStartState
	yamlParseDocumentContentState
	yamlParseDocumentEndState
	yamlParseBlockNodeState
	yamlParseBlockNodeOrIndentlessSequenceState
	yamlParseFlowNodeState
	yamlParseBlockSequenceFirstEntryState
	yamlParseBlockSequenceEntryState
	yamlParseIndentlessSequenceEntryState
	yamlParseBlockMappingFirstKeyState
	yamlParseBlockMappingKeyState
	yamlParseBlockMappingValueState
	yamlParseFlowSequenceFirstEntryState
	yamlParseFlowSequenceEntryState
	yamlParseFlowSequenceEntryMappingKeyState
	yamlParseFlowSequenceEntryMappingValueState
	yamlParseFlowSequenceEntryMappingEndState
	yamlParseFlowMappingFirstKeyState
	yamlParseFlowMappingKeyState
	yamlParseFlowMappingValueState
	yamlParseFlowMappingEmptyValueState
	yamlParseEndState
)

// Mirror of the libyaml C state with each stack capacity tracked so memory limits trip at the same depth
type yamlParser struct {
	errorType     yamlErrorType
	problem       string
	problemOffset int
	problemValue  int
	problemMark   yamlMark
	context       string
	contextMark   yamlMark

	input    string
	inputPos int
	eof      bool

	buffer     []byte
	bufferPos  int
	bufferLast int
	unread     int

	rawBuffer []byte
	rawPos    int
	rawLast   int

	encoding yamlEncoding
	offset   int
	mark     yamlMark

	streamStartProduced bool
	streamEndProduced   bool
	flowLevel           int32

	tokens         []yamlToken
	tokensHead     int
	tokensParsed   int
	tokenAvailable bool

	indents          []int32
	indentsCap       int
	indent           int32
	simpleKeyAllowed bool
	simpleKeys       []yamlSimpleKey
	simpleKeysCap    int
	states           []yamlParserState
	statesCap        int
	state            yamlParserState
	marks            []yamlStackMark
	marksCap         int
	tagDirectives    []yamlTagDirective
	tagDirectivesCap int
}


// Appends to a C stack whose capacity doubles from 16, failing like yaml_stack_extend at INT_MAX over two bytes
func libyamlPush[T any](p *yamlParser, stack *[]T, capacity *int, elemSize int, v T) bool {
	if len(*stack) == *capacity {
		if *capacity*elemSize >= libyamlStackByteLimit {
			p.errorType = yamlMemoryError
			return false
		}
		*capacity *= 2
	}
	if len(*stack) == cap(*stack) {
		grown := make([]T, len(*stack), *capacity)
		copy(grown, *stack)
		*stack = grown
	}
	*stack = append(*stack, v)
	return true
}

func yamlParserSetReaderError(p *yamlParser, problem string, offset int, value int) bool {
	p.errorType = yamlReaderError
	p.problem = problem
	p.problemOffset = offset
	p.problemValue = value
	return false
}

// Detects a UTF-16 or UTF-8 byte order mark and consumes it, defaulting to UTF-8
func yamlParserDetermineEncoding(p *yamlParser) bool {
	for !p.eof && p.rawLast-p.rawPos < 3 {
		if !yamlParserUpdateRawBuffer(p) {
			return false
		}
	}
	raw := p.rawBuffer[p.rawPos:p.rawLast]
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		p.encoding = yamlUTF16LEEncoding
		p.rawPos += 2
		p.offset += 2
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		p.encoding = yamlUTF16BEEncoding
		p.rawPos += 2
		p.offset += 2
	case len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF:
		p.encoding = yamlUTF8Encoding
		p.rawPos += 3
		p.offset += 3
	default:
		p.encoding = yamlUTF8Encoding
	}
	return true
}

// Shifts unread raw bytes to the front and copies the next input chunk like the string read handler
func yamlParserUpdateRawBuffer(p *yamlParser) bool {
	if p.rawPos == 0 && p.rawLast == len(p.rawBuffer) {
		return true
	}
	if p.eof {
		return true
	}
	if 0 < p.rawPos && p.rawPos < p.rawLast {
		copy(p.rawBuffer, p.rawBuffer[p.rawPos:p.rawLast])
	}
	p.rawLast -= p.rawPos
	p.rawPos = 0
	n := copy(p.rawBuffer[p.rawLast:], p.input[p.inputPos:])
	p.inputPos += n
	p.rawLast += n
	if n == 0 {
		p.eof = true
	}
	return true
}

// Decodes whole raw chunks into the UTF-8 working buffer until enough characters are unread
func yamlParserUpdateBuffer(p *yamlParser, length int) bool {
	first := true
	if p.eof && p.rawPos == p.rawLast {
		return true
	}
	if p.unread >= length {
		return true
	}
	if p.encoding == yamlAnyEncoding {
		if !yamlParserDetermineEncoding(p) {
			return false
		}
	}
	if 0 < p.bufferPos && p.bufferPos < p.bufferLast {
		size := p.bufferLast - p.bufferPos
		copy(p.buffer, p.buffer[p.bufferPos:p.bufferLast])
		p.bufferPos = 0
		p.bufferLast = size
	} else if p.bufferPos == p.bufferLast {
		p.bufferPos = 0
		p.bufferLast = 0
	}
	for p.unread < length {
		if !first || p.rawPos == p.rawLast {
			if !yamlParserUpdateRawBuffer(p) {
				return false
			}
		}
		first = false
		for p.rawPos != p.rawLast {
			var value, width int
			incomplete := false
			rawUnread := p.rawLast - p.rawPos
			raw := p.rawBuffer[p.rawPos:p.rawLast]
			if p.encoding == yamlUTF8Encoding {
				octet := raw[0]
				switch {
				case octet&0x80 == 0x00:
					width = 1
				case octet&0xE0 == 0xC0:
					width = 2
				case octet&0xF0 == 0xE0:
					width = 3
				case octet&0xF8 == 0xF0:
					width = 4
				}
				if width == 0 {
					return yamlParserSetReaderError(p, "invalid leading UTF-8 octet", p.offset, int(octet))
				}
				if width > rawUnread {
					if p.eof {
						return yamlParserSetReaderError(p, "incomplete UTF-8 octet sequence", p.offset, -1)
					}
					incomplete = true
				} else {
					switch width {
					case 1:
						value = int(octet & 0x7F)
					case 2:
						value = int(octet & 0x1F)
					case 3:
						value = int(octet & 0x0F)
					case 4:
						value = int(octet & 0x07)
					}
					for k := 1; k < width; k++ {
						octet = raw[k]
						if octet&0xC0 != 0x80 {
							return yamlParserSetReaderError(p, "invalid trailing UTF-8 octet", p.offset+k, int(octet))
						}
						value = (value << 6) + int(octet&0x3F)
					}
					if !(width == 1 || (width == 2 && value >= 0x80) || (width == 3 && value >= 0x800) || (width == 4 && value >= 0x10000)) {
						return yamlParserSetReaderError(p, "invalid length of a UTF-8 sequence", p.offset, -1)
					}
					if (value >= 0xD800 && value <= 0xDFFF) || value > 0x10FFFF {
						return yamlParserSetReaderError(p, "invalid Unicode character", p.offset, value)
					}
				}
			} else {
				low, high := 0, 1
				if p.encoding == yamlUTF16BEEncoding {
					low, high = 1, 0
				}
				if rawUnread < 2 {
					if p.eof {
						return yamlParserSetReaderError(p, "incomplete UTF-16 character", p.offset, -1)
					}
					incomplete = true
				} else {
					value = int(raw[low]) + int(raw[high])<<8
					if value&0xFC00 == 0xDC00 {
						return yamlParserSetReaderError(p, "unexpected low surrogate area", p.offset, value)
					}
					if value&0xFC00 == 0xD800 {
						width = 4
						if rawUnread < 4 {
							if p.eof {
								return yamlParserSetReaderError(p, "incomplete UTF-16 surrogate pair", p.offset, -1)
							}
							incomplete = true
						} else {
							value2 := int(raw[low+2]) + int(raw[high+2])<<8
							if value2&0xFC00 != 0xDC00 {
								return yamlParserSetReaderError(p, "expected low surrogate area", p.offset+2, value2)
							}
							value = 0x10000 + (value&0x3FF)<<10 + (value2 & 0x3FF)
						}
					} else {
						width = 2
					}
				}
			}
			if incomplete {
				break
			}
			if !(value == 0x09 || value == 0x0A || value == 0x0D || (value >= 0x20 && value <= 0x7E) ||
				value == 0x85 || (value >= 0xA0 && value <= 0xD7FF) || (value >= 0xE000 && value <= 0xFFFD) ||
				(value >= 0x10000 && value <= 0x10FFFF)) {
				return yamlParserSetReaderError(p, "control characters are not allowed", p.offset, value)
			}
			p.rawPos += width
			p.offset += width
			p.bufferLast += utf8.EncodeRune(p.buffer[p.bufferLast:], rune(value))
			p.unread++
		}
		if p.eof {
			p.buffer[p.bufferLast] = 0
			p.bufferLast++
			p.unread++
			return true
		}
	}
	if p.offset >= libyamlMaxFileSize {
		return yamlParserSetReaderError(p, "input is too long", p.offset, -1)
	}
	return true
}

func yamlCache(p *yamlParser, length int) bool {
	return p.unread >= length || yamlParserUpdateBuffer(p, length)
}

func yamlAt(p *yamlParser, k int) byte {
	return p.buffer[p.bufferPos+k]
}

func yamlIsAlpha(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || c == '-'
}

func yamlIsDigit(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return c >= '0' && c <= '9'
}

func yamlAsDigit(p *yamlParser, k int) int32 {
	return int32(yamlAt(p, k) - '0')
}

func yamlIsHex(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')
}

func yamlAsHex(p *yamlParser, k int) int {
	c := yamlAt(p, k)
	switch {
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return int(c - '0')
}

func yamlIsZ(p *yamlParser, k int) bool {
	return yamlAt(p, k) == 0
}

func yamlIsBOM(p *yamlParser, k int) bool {
	return yamlAt(p, k) == 0xEF && yamlAt(p, k+1) == 0xBB && yamlAt(p, k+2) == 0xBF
}

func yamlIsSpace(p *yamlParser, k int) bool {
	return yamlAt(p, k) == ' '
}

func yamlIsTab(p *yamlParser, k int) bool {
	return yamlAt(p, k) == '\t'
}

func yamlIsBlank(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return c == ' ' || c == '\t'
}

func yamlIsBreak(p *yamlParser, k int) bool {
	c := yamlAt(p, k)
	return c == '\r' || c == '\n' ||
		(c == 0xC2 && yamlAt(p, k+1) == 0x85) ||
		(c == 0xE2 && yamlAt(p, k+1) == 0x80 && yamlAt(p, k+2) == 0xA8) ||
		(c == 0xE2 && yamlAt(p, k+1) == 0x80 && yamlAt(p, k+2) == 0xA9)
}

func yamlIsCRLF(p *yamlParser, k int) bool {
	return yamlAt(p, k) == '\r' && yamlAt(p, k+1) == '\n'
}

func yamlIsBreakZ(p *yamlParser, k int) bool {
	return yamlIsBreak(p, k) || yamlIsZ(p, k)
}

func yamlIsBlankZ(p *yamlParser, k int) bool {
	return yamlIsBlank(p, k) || yamlIsBreakZ(p, k)
}

func yamlWidth(c byte) int {
	switch {
	case c&0x80 == 0x00:
		return 1
	case c&0xE0 == 0xC0:
		return 2
	case c&0xF0 == 0xE0:
		return 3
	case c&0xF8 == 0xF0:
		return 4
	}
	return 0
}

func yamlSkip(p *yamlParser) {
	p.mark.index++
	p.mark.column++
	p.unread--
	p.bufferPos += yamlWidth(p.buffer[p.bufferPos])
}

func yamlSkipLine(p *yamlParser) {
	if yamlIsCRLF(p, 0) {
		p.mark.index += 2
		p.mark.column = 0
		p.mark.line++
		p.unread -= 2
		p.bufferPos += 2
	} else if yamlIsBreak(p, 0) {
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
		p.bufferPos += yamlWidth(p.buffer[p.bufferPos])
	}
}

func yamlRead(p *yamlParser, s []byte) []byte {
	w := yamlWidth(p.buffer[p.bufferPos])
	s = append(s, p.buffer[p.bufferPos:p.bufferPos+w]...)
	p.bufferPos += w
	p.mark.index++
	p.mark.column++
	p.unread--
	return s
}

// Appends one line break normalized to LF, keeping LS and PS verbatim
func yamlReadLine(p *yamlParser, s []byte) []byte {
	c := yamlAt(p, 0)
	switch {
	case c == '\r' && yamlAt(p, 1) == '\n':
		s = append(s, '\n')
		p.bufferPos += 2
		p.mark.index += 2
		p.mark.column = 0
		p.mark.line++
		p.unread -= 2
	case c == '\r' || c == '\n':
		s = append(s, '\n')
		p.bufferPos++
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
	case c == 0xC2 && yamlAt(p, 1) == 0x85:
		s = append(s, '\n')
		p.bufferPos += 2
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
	case c == 0xE2 && yamlAt(p, 1) == 0x80 && (yamlAt(p, 2) == 0xA8 || yamlAt(p, 2) == 0xA9):
		s = append(s, p.buffer[p.bufferPos:p.bufferPos+3]...)
		p.bufferPos += 3
		p.mark.index++
		p.mark.column = 0
		p.mark.line++
		p.unread--
	}
	return s
}

// Cuts scanned bytes at the first NUL, matching what strlen based consumers in libyaml and CPython see
func yamlCString(s []byte) []byte {
	if i := bytes.IndexByte(s, 0); i >= 0 {
		return s[:i]
	}
	return s
}

func yamlNewString() []byte {
	return make([]byte, 0, libyamlInitialStringSize)
}

func yamlParserSetScannerError(p *yamlParser, context string, contextMark yamlMark, problem string) bool {
	p.errorType = yamlScannerError
	p.context = context
	p.contextMark = contextMark
	p.problem = problem
	p.problemMark = p.mark
	return false
}

func yamlEnqueueToken(p *yamlParser, t yamlToken) {
	if p.tokensHead > 0 && len(p.tokens) == cap(p.tokens) {
		n := copy(p.tokens, p.tokens[p.tokensHead:])
		p.tokens = p.tokens[:n]
		p.tokensHead = 0
	}
	p.tokens = append(p.tokens, t)
}

func yamlInsertToken(p *yamlParser, pos int, t yamlToken) {
	yamlEnqueueToken(p, yamlToken{typ: yamlNoToken})
	i := p.tokensHead + pos
	copy(p.tokens[i+1:], p.tokens[i:len(p.tokens)-1])
	p.tokens[i] = t
}

// Queues scanner output until the head entry can no longer turn into a simple key
func yamlParserFetchMoreTokens(p *yamlParser) bool {
	for {
		need := false
		if p.tokensHead == len(p.tokens) {
			need = true
		} else {
			if !yamlParserStaleSimpleKeys(p) {
				return false
			}
			for i := range p.simpleKeys {
				k := &p.simpleKeys[i]
				if k.possible && k.tokenNumber == p.tokensParsed {
					need = true
					break
				}
			}
		}
		if !need {
			break
		}
		if !yamlParserFetchNextToken(p) {
			return false
		}
	}
	p.tokenAvailable = true
	return true
}

// Dispatches on the next significant character to the matching scanner routine
func yamlParserFetchNextToken(p *yamlParser) bool {
	if !yamlCache(p, 1) {
		return false
	}
	if !p.streamStartProduced {
		return yamlParserFetchStreamStart(p)
	}
	if !yamlParserScanToNextToken(p) {
		return false
	}
	if !yamlParserStaleSimpleKeys(p) {
		return false
	}
	yamlParserUnrollIndent(p, p.mark.column)
	if !yamlCache(p, 4) {
		return false
	}
	if yamlIsZ(p, 0) {
		return yamlParserFetchStreamEnd(p)
	}
	c := yamlAt(p, 0)
	if p.mark.column == 0 && c == '%' {
		return yamlParserFetchDirective(p)
	}
	if p.mark.column == 0 && c == '-' && yamlAt(p, 1) == '-' && yamlAt(p, 2) == '-' && yamlIsBlankZ(p, 3) {
		return yamlParserFetchDocumentIndicator(p, yamlDocumentStartToken)
	}
	if p.mark.column == 0 && c == '.' && yamlAt(p, 1) == '.' && yamlAt(p, 2) == '.' && yamlIsBlankZ(p, 3) {
		return yamlParserFetchDocumentIndicator(p, yamlDocumentEndToken)
	}
	switch c {
	case '[':
		return yamlParserFetchFlowCollectionStart(p, yamlFlowSequenceStartToken)
	case '{':
		return yamlParserFetchFlowCollectionStart(p, yamlFlowMappingStartToken)
	case ']':
		return yamlParserFetchFlowCollectionEnd(p, yamlFlowSequenceEndToken)
	case '}':
		return yamlParserFetchFlowCollectionEnd(p, yamlFlowMappingEndToken)
	case ',':
		return yamlParserFetchFlowEntry(p)
	}
	if c == '-' && yamlIsBlankZ(p, 1) {
		return yamlParserFetchBlockEntry(p)
	}
	if c == '?' && (p.flowLevel != 0 || yamlIsBlankZ(p, 1)) {
		return yamlParserFetchKey(p)
	}
	if c == ':' && (p.flowLevel != 0 || yamlIsBlankZ(p, 1)) {
		return yamlParserFetchValue(p)
	}
	switch c {
	case '*':
		return yamlParserFetchAnchor(p, yamlAliasToken)
	case '&':
		return yamlParserFetchAnchor(p, yamlAnchorToken)
	case '!':
		return yamlParserFetchTag(p)
	}
	if c == '|' && p.flowLevel == 0 {
		return yamlParserFetchBlockScalar(p, true)
	}
	if c == '>' && p.flowLevel == 0 {
		return yamlParserFetchBlockScalar(p, false)
	}
	if c == '\'' {
		return yamlParserFetchFlowScalar(p, true)
	}
	if c == '"' {
		return yamlParserFetchFlowScalar(p, false)
	}
	if !(yamlIsBlankZ(p, 0) || strings.IndexByte("-?:,[]{}#&*!|>'\"%@`", c) >= 0) ||
		(c == '-' && !yamlIsBlank(p, 1)) ||
		(p.flowLevel == 0 && (c == '?' || c == ':') && !yamlIsBlankZ(p, 1)) {
		return yamlParserFetchPlainScalar(p)
	}
	return yamlParserSetScannerError(p, "while scanning for the next token", p.mark, "found character that cannot start any token")
}

// Expires pending keys left on an earlier line or over 1024 characters back, failing on required ones
func yamlParserStaleSimpleKeys(p *yamlParser) bool {
	for i := range p.simpleKeys {
		k := &p.simpleKeys[i]
		if k.possible && (k.mark.line < p.mark.line || k.mark.index+1024 < p.mark.index) {
			if k.required {
				return yamlParserSetScannerError(p, "while scanning a simple key", k.mark, "could not find expected ':'")
			}
			k.possible = false
		}
	}
	return true
}

func yamlParserSaveSimpleKey(p *yamlParser) bool {
	required := p.flowLevel == 0 && int(p.indent) == p.mark.column
	if p.simpleKeyAllowed {
		key := yamlSimpleKey{
			possible:    true,
			required:    required,
			tokenNumber: p.tokensParsed + (len(p.tokens) - p.tokensHead),
			mark:        p.mark,
		}
		if !yamlParserRemoveSimpleKey(p) {
			return false
		}
		p.simpleKeys[len(p.simpleKeys)-1] = key
	}
	return true
}

func yamlParserRemoveSimpleKey(p *yamlParser) bool {
	k := &p.simpleKeys[len(p.simpleKeys)-1]
	if k.possible && k.required {
		return yamlParserSetScannerError(p, "while scanning a simple key", k.mark, "could not find expected ':'")
	}
	k.possible = false
	return true
}

func yamlParserIncreaseFlowLevel(p *yamlParser) bool {
	if !libyamlPush(p, &p.simpleKeys, &p.simpleKeysCap, libyamlSizeofSimpleKey, yamlSimpleKey{}) {
		return false
	}
	if p.flowLevel == math.MaxInt32 {
		p.errorType = yamlMemoryError
		return false
	}
	p.flowLevel++
	return true
}

func yamlParserDecreaseFlowLevel(p *yamlParser) {
	if p.flowLevel != 0 {
		p.flowLevel--
		p.simpleKeys = p.simpleKeys[:len(p.simpleKeys)-1]
	}
}

// Opens a block collection at a deeper column by queueing its start token
func yamlParserRollIndent(p *yamlParser, column int, number int, typ yamlTokenType, mark yamlMark) bool {
	if p.flowLevel != 0 {
		return true
	}
	if int(p.indent) < column {
		if !libyamlPush(p, &p.indents, &p.indentsCap, libyamlSizeofInt, p.indent) {
			return false
		}
		if column > math.MaxInt32 {
			p.errorType = yamlMemoryError
			return false
		}
		p.indent = int32(column)
		t := yamlToken{typ: typ, startMark: mark}
		if number == -1 {
			yamlEnqueueToken(p, t)
		} else {
			yamlInsertToken(p, number-p.tokensParsed, t)
		}
	}
	return true
}

// Closes block collections deeper than the column with block end tokens
func yamlParserUnrollIndent(p *yamlParser, column int) {
	if p.flowLevel != 0 {
		return
	}
	for int(p.indent) > column {
		yamlEnqueueToken(p, yamlToken{typ: yamlBlockEndToken, startMark: p.mark})
		p.indent = p.indents[len(p.indents)-1]
		p.indents = p.indents[:len(p.indents)-1]
	}
}

func yamlParserFetchStreamStart(p *yamlParser) bool {
	p.indent = -1
	if !libyamlPush(p, &p.simpleKeys, &p.simpleKeysCap, libyamlSizeofSimpleKey, yamlSimpleKey{}) {
		return false
	}
	p.simpleKeyAllowed = true
	p.streamStartProduced = true
	yamlEnqueueToken(p, yamlToken{typ: yamlStreamStartToken, startMark: p.mark})
	return true
}

func yamlParserFetchStreamEnd(p *yamlParser) bool {
	if p.mark.column != 0 {
		p.mark.column = 0
		p.mark.line++
	}
	yamlParserUnrollIndent(p, -1)
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	yamlEnqueueToken(p, yamlToken{typ: yamlStreamEndToken, startMark: p.mark})
	return true
}

func yamlParserFetchDirective(p *yamlParser) bool {
	yamlParserUnrollIndent(p, -1)
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanDirective(p, &t) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchDocumentIndicator(p *yamlParser, typ yamlTokenType) bool {
	yamlParserUnrollIndent(p, -1)
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	startMark := p.mark
	yamlSkip(p)
	yamlSkip(p)
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: typ, startMark: startMark})
	return true
}

func yamlParserFetchFlowCollectionStart(p *yamlParser, typ yamlTokenType) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	if !yamlParserIncreaseFlowLevel(p) {
		return false
	}
	p.simpleKeyAllowed = true
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: typ, startMark: startMark})
	return true
}

func yamlParserFetchFlowCollectionEnd(p *yamlParser, typ yamlTokenType) bool {
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	yamlParserDecreaseFlowLevel(p)
	p.simpleKeyAllowed = false
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: typ, startMark: startMark})
	return true
}

func yamlParserFetchFlowEntry(p *yamlParser) bool {
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = true
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlFlowEntryToken, startMark: startMark})
	return true
}

func yamlParserFetchBlockEntry(p *yamlParser) bool {
	if p.flowLevel == 0 {
		if !p.simpleKeyAllowed {
			return yamlParserSetScannerError(p, "", p.mark, "block sequence entries are not allowed in this context")
		}
		if !yamlParserRollIndent(p, p.mark.column, -1, yamlBlockSequenceStartToken, p.mark) {
			return false
		}
	}
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = true
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlBlockEntryToken, startMark: startMark})
	return true
}

func yamlParserFetchKey(p *yamlParser) bool {
	if p.flowLevel == 0 {
		if !p.simpleKeyAllowed {
			return yamlParserSetScannerError(p, "", p.mark, "mapping keys are not allowed in this context")
		}
		if !yamlParserRollIndent(p, p.mark.column, -1, yamlBlockMappingStartToken, p.mark) {
			return false
		}
	}
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = p.flowLevel == 0
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlKeyToken, startMark: startMark})
	return true
}

// Queues the colon indicator, first inserting the key and mapping start for a pending simple key
func yamlParserFetchValue(p *yamlParser) bool {
	k := &p.simpleKeys[len(p.simpleKeys)-1]
	if k.possible {
		yamlInsertToken(p, k.tokenNumber-p.tokensParsed, yamlToken{typ: yamlKeyToken, startMark: k.mark})
		if !yamlParserRollIndent(p, k.mark.column, k.tokenNumber, yamlBlockMappingStartToken, k.mark) {
			return false
		}
		k.possible = false
		p.simpleKeyAllowed = false
	} else {
		if p.flowLevel == 0 {
			if !p.simpleKeyAllowed {
				return yamlParserSetScannerError(p, "", p.mark, "mapping values are not allowed in this context")
			}
			if !yamlParserRollIndent(p, p.mark.column, -1, yamlBlockMappingStartToken, p.mark) {
				return false
			}
		}
		p.simpleKeyAllowed = p.flowLevel == 0
	}
	startMark := p.mark
	yamlSkip(p)
	yamlEnqueueToken(p, yamlToken{typ: yamlValueToken, startMark: startMark})
	return true
}

func yamlParserFetchAnchor(p *yamlParser, typ yamlTokenType) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanAnchor(p, &t, typ) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchTag(p *yamlParser) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanTag(p, &t) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchBlockScalar(p *yamlParser, literal bool) bool {
	if !yamlParserRemoveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = true
	var t yamlToken
	if !yamlParserScanBlockScalar(p, &t, literal) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchFlowScalar(p *yamlParser, single bool) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanFlowScalar(p, &t, single) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

func yamlParserFetchPlainScalar(p *yamlParser) bool {
	if !yamlParserSaveSimpleKey(p) {
		return false
	}
	p.simpleKeyAllowed = false
	var t yamlToken
	if !yamlParserScanPlainScalar(p, &t) {
		return false
	}
	yamlEnqueueToken(p, t)
	return true
}

// Skips spaces, permitted tabs, comments, line breaks and column zero byte order marks
func yamlParserScanToNextToken(p *yamlParser) bool {
	for {
		if !yamlCache(p, 1) {
			return false
		}
		if p.mark.column == 0 && yamlIsBOM(p, 0) {
			yamlSkip(p)
		}
		if !yamlCache(p, 1) {
			return false
		}
		for yamlAt(p, 0) == ' ' || ((p.flowLevel != 0 || !p.simpleKeyAllowed) && yamlAt(p, 0) == '\t') {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
		if yamlAt(p, 0) == '#' {
			for !yamlIsBreakZ(p, 0) {
				yamlSkip(p)
				if !yamlCache(p, 1) {
					return false
				}
			}
		}
		if !yamlIsBreak(p, 0) {
			break
		}
		if !yamlCache(p, 2) {
			return false
		}
		yamlSkipLine(p)
		if p.flowLevel == 0 {
			p.simpleKeyAllowed = true
		}
	}
	return true
}

// Reads a YAML or TAG directive line including its trailing comment and line break
func yamlParserScanDirective(p *yamlParser, t *yamlToken) bool {
	startMark := p.mark
	yamlSkip(p)
	name, ok := yamlParserScanDirectiveName(p, startMark)
	if !ok {
		return false
	}
	switch string(name) {
	case "YAML":
		major, minor, ok := yamlParserScanVersionDirectiveValue(p, startMark)
		if !ok {
			return false
		}
		*t = yamlToken{typ: yamlVersionDirectiveToken, startMark: startMark, major: major, minor: minor}
	case "TAG":
		handle, prefix, ok := yamlParserScanTagDirectiveValue(p, startMark)
		if !ok {
			return false
		}
		*t = yamlToken{typ: yamlTagDirectiveToken, startMark: startMark, handle: handle, suffix: prefix}
	default:
		return yamlParserSetScannerError(p, "while scanning a directive", startMark, "found unknown directive name")
	}
	if !yamlCache(p, 1) {
		return false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
	}
	if yamlAt(p, 0) == '#' {
		for !yamlIsBreakZ(p, 0) {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
	}
	if !yamlIsBreakZ(p, 0) {
		return yamlParserSetScannerError(p, "while scanning a directive", startMark, "did not find expected comment or line break")
	}
	if yamlIsBreak(p, 0) {
		if !yamlCache(p, 2) {
			return false
		}
		yamlSkipLine(p)
	}
	return true
}

func yamlParserScanDirectiveName(p *yamlParser, startMark yamlMark) ([]byte, bool) {
	s := yamlNewString()
	if !yamlCache(p, 1) {
		return nil, false
	}
	for yamlIsAlpha(p, 0) {
		s = yamlRead(p, s)
		if !yamlCache(p, 1) {
			return nil, false
		}
	}
	if len(s) == 0 {
		return nil, yamlParserSetScannerError(p, "while scanning a directive", startMark, "could not find expected directive name")
	}
	if !yamlIsBlankZ(p, 0) {
		return nil, yamlParserSetScannerError(p, "while scanning a directive", startMark, "found unexpected non-alphabetical character")
	}
	return s, true
}

func yamlParserScanVersionDirectiveValue(p *yamlParser, startMark yamlMark) (int32, int32, bool) {
	if !yamlCache(p, 1) {
		return 0, 0, false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return 0, 0, false
		}
	}
	major, ok := yamlParserScanVersionDirectiveNumber(p, startMark)
	if !ok {
		return 0, 0, false
	}
	if yamlAt(p, 0) != '.' {
		return 0, 0, yamlParserSetScannerError(p, "while scanning a %YAML directive", startMark, "did not find expected digit or '.' character")
	}
	yamlSkip(p)
	minor, ok := yamlParserScanVersionDirectiveNumber(p, startMark)
	if !ok {
		return 0, 0, false
	}
	return major, minor, true
}

func yamlParserScanVersionDirectiveNumber(p *yamlParser, startMark yamlMark) (int32, bool) {
	var value int32
	length := 0
	if !yamlCache(p, 1) {
		return 0, false
	}
	for yamlIsDigit(p, 0) {
		length++
		if length > libyamlMaxNumberLength {
			return 0, yamlParserSetScannerError(p, "while scanning a %YAML directive", startMark, "found extremely long version number")
		}
		value = value*10 + yamlAsDigit(p, 0)
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return 0, false
		}
	}
	if length == 0 {
		return 0, yamlParserSetScannerError(p, "while scanning a %YAML directive", startMark, "did not find expected version number")
	}
	return value, true
}

func yamlParserScanTagDirectiveValue(p *yamlParser, startMark yamlMark) ([]byte, []byte, bool) {
	if !yamlCache(p, 1) {
		return nil, nil, false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return nil, nil, false
		}
	}
	handle, ok := yamlParserScanTagHandle(p, true, startMark)
	if !ok {
		return nil, nil, false
	}
	if !yamlCache(p, 1) {
		return nil, nil, false
	}
	if !yamlIsBlank(p, 0) {
		return nil, nil, yamlParserSetScannerError(p, "while scanning a %TAG directive", startMark, "did not find expected whitespace")
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return nil, nil, false
		}
	}
	prefix, ok := yamlParserScanTagURI(p, true, nil, startMark)
	if !ok {
		return nil, nil, false
	}
	if !yamlCache(p, 1) {
		return nil, nil, false
	}
	if !yamlIsBlankZ(p, 0) {
		return nil, nil, yamlParserSetScannerError(p, "while scanning a %TAG directive", startMark, "did not find expected whitespace or line break")
	}
	return handle, prefix, true
}

func yamlParserScanAnchor(p *yamlParser, t *yamlToken, typ yamlTokenType) bool {
	s := yamlNewString()
	startMark := p.mark
	yamlSkip(p)
	if !yamlCache(p, 1) {
		return false
	}
	length := 0
	for yamlIsAlpha(p, 0) {
		s = yamlRead(p, s)
		if !yamlCache(p, 1) {
			return false
		}
		length++
	}
	if length == 0 || !(yamlIsBlankZ(p, 0) || strings.IndexByte("?:,]}%@`", yamlAt(p, 0)) >= 0) {
		context := "while scanning an alias"
		if typ == yamlAnchorToken {
			context = "while scanning an anchor"
		}
		return yamlParserSetScannerError(p, context, startMark, "did not find expected alphabetic or numeric character")
	}
	*t = yamlToken{typ: typ, startMark: startMark, value: s}
	return true
}

// Reads a verbatim, shorthand or bare tag and splits it into handle and suffix
func yamlParserScanTag(p *yamlParser, t *yamlToken) bool {
	var handle, suffix []byte
	var ok bool
	startMark := p.mark
	if !yamlCache(p, 2) {
		return false
	}
	if yamlAt(p, 1) == '<' {
		handle = yamlNewString()
		yamlSkip(p)
		yamlSkip(p)
		if suffix, ok = yamlParserScanTagURI(p, false, nil, startMark); !ok {
			return false
		}
		if yamlAt(p, 0) != '>' {
			return yamlParserSetScannerError(p, "while scanning a tag", startMark, "did not find the expected '>'")
		}
		yamlSkip(p)
	} else {
		if handle, ok = yamlParserScanTagHandle(p, false, startMark); !ok {
			return false
		}
		if handle[0] == '!' && len(handle) > 1 && handle[len(handle)-1] == '!' {
			if suffix, ok = yamlParserScanTagURI(p, false, nil, startMark); !ok {
				return false
			}
		} else {
			if suffix, ok = yamlParserScanTagURI(p, false, handle, startMark); !ok {
				return false
			}
			handle = append(yamlNewString(), '!')
			if len(suffix) == 0 {
				handle, suffix = suffix, handle
			}
		}
	}
	if !yamlCache(p, 1) {
		return false
	}
	if !yamlIsBlankZ(p, 0) {
		return yamlParserSetScannerError(p, "while scanning a tag", startMark, "did not find expected whitespace or line break")
	}
	*t = yamlToken{typ: yamlTagToken, startMark: startMark, handle: handle, suffix: suffix}
	return true
}

func yamlParserScanTagHandle(p *yamlParser, directive bool, startMark yamlMark) ([]byte, bool) {
	s := yamlNewString()
	if !yamlCache(p, 1) {
		return nil, false
	}
	if yamlAt(p, 0) != '!' {
		context := "while scanning a tag"
		if directive {
			context = "while scanning a tag directive"
		}
		return nil, yamlParserSetScannerError(p, context, startMark, "did not find expected '!'")
	}
	s = yamlRead(p, s)
	if !yamlCache(p, 1) {
		return nil, false
	}
	for yamlIsAlpha(p, 0) {
		s = yamlRead(p, s)
		if !yamlCache(p, 1) {
			return nil, false
		}
	}
	if yamlAt(p, 0) == '!' {
		s = yamlRead(p, s)
	} else if directive && !(len(s) == 1 && s[0] == '!') {
		return nil, yamlParserSetScannerError(p, "while parsing a tag directive", startMark, "did not find expected '!'")
	}
	return s, true
}

// Collects URI characters and percent escapes after the head handle, cut at the first NUL
func yamlParserScanTagURI(p *yamlParser, directive bool, head []byte, startMark yamlMark) ([]byte, bool) {
	length := len(head)
	s := yamlNewString()
	if length > 1 {
		s = append(s, head[1:]...)
	}
	if !yamlCache(p, 1) {
		return nil, false
	}
	for yamlIsAlpha(p, 0) || strings.IndexByte(";/?:@&=+$,.!~*'()[]%", yamlAt(p, 0)) >= 0 {
		if yamlAt(p, 0) == '%' {
			var ok bool
			if s, ok = yamlParserScanURIEscapes(p, directive, startMark, s); !ok {
				return nil, false
			}
		} else {
			s = yamlRead(p, s)
		}
		length++
		if !yamlCache(p, 1) {
			return nil, false
		}
	}
	if length == 0 {
		context := "while parsing a tag"
		if directive {
			context = "while parsing a %TAG directive"
		}
		return nil, yamlParserSetScannerError(p, context, startMark, "did not find expected tag URI")
	}
	return yamlCString(s), true
}

// Decodes one percent escaped UTF-8 sequence, checking leading and trailing octet shapes
func yamlParserScanURIEscapes(p *yamlParser, directive bool, startMark yamlMark, s []byte) ([]byte, bool) {
	context := "while parsing a tag"
	if directive {
		context = "while parsing a %TAG directive"
	}
	width := 0
	for {
		if !yamlCache(p, 3) {
			return nil, false
		}
		if !(yamlAt(p, 0) == '%' && yamlIsHex(p, 1) && yamlIsHex(p, 2)) {
			return nil, yamlParserSetScannerError(p, context, startMark, "did not find URI escaped octet")
		}
		octet := byte(yamlAsHex(p, 1)<<4 + yamlAsHex(p, 2))
		if width == 0 {
			width = yamlWidth(octet)
			if width == 0 {
				return nil, yamlParserSetScannerError(p, context, startMark, "found an incorrect leading UTF-8 octet")
			}
		} else if octet&0xC0 != 0x80 {
			return nil, yamlParserSetScannerError(p, context, startMark, "found an incorrect trailing UTF-8 octet")
		}
		s = append(s, octet)
		yamlSkip(p)
		yamlSkip(p)
		yamlSkip(p)
		width--
		if width == 0 {
			return s, true
		}
	}
}

// Reads a literal or folded scalar with its chomping and indentation indicators
func yamlParserScanBlockScalar(p *yamlParser, t *yamlToken, literal bool) bool {
	s := yamlNewString()
	leadingBreak := yamlNewString()
	trailingBreaks := yamlNewString()
	chomping := 0
	var increment, indent int32
	leadingBlank, trailingBlank := false, false
	startMark := p.mark
	yamlSkip(p)
	if !yamlCache(p, 1) {
		return false
	}
	if yamlAt(p, 0) == '+' || yamlAt(p, 0) == '-' {
		chomping = -1
		if yamlAt(p, 0) == '+' {
			chomping = 1
		}
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
		if yamlIsDigit(p, 0) {
			if yamlAt(p, 0) == '0' {
				return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "found an indentation indicator equal to 0")
			}
			increment = yamlAsDigit(p, 0)
			yamlSkip(p)
		}
	} else if yamlIsDigit(p, 0) {
		if yamlAt(p, 0) == '0' {
			return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "found an indentation indicator equal to 0")
		}
		increment = yamlAsDigit(p, 0)
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
		if yamlAt(p, 0) == '+' || yamlAt(p, 0) == '-' {
			chomping = -1
			if yamlAt(p, 0) == '+' {
				chomping = 1
			}
			yamlSkip(p)
		}
	}
	if !yamlCache(p, 1) {
		return false
	}
	for yamlIsBlank(p, 0) {
		yamlSkip(p)
		if !yamlCache(p, 1) {
			return false
		}
	}
	if yamlAt(p, 0) == '#' {
		for !yamlIsBreakZ(p, 0) {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
	}
	if !yamlIsBreakZ(p, 0) {
		return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "did not find expected comment or line break")
	}
	if yamlIsBreak(p, 0) {
		if !yamlCache(p, 2) {
			return false
		}
		yamlSkipLine(p)
	}
	if increment != 0 {
		if p.indent >= 0 {
			indent = p.indent + increment
		} else {
			indent = increment
		}
	}
	if !yamlParserScanBlockScalarBreaks(p, &indent, &trailingBreaks, startMark) {
		return false
	}
	if !yamlCache(p, 1) {
		return false
	}
	for int32(p.mark.column) == indent && !yamlIsZ(p, 0) {
		trailingBlank = yamlIsBlank(p, 0)
		if !literal && len(leadingBreak) > 0 && leadingBreak[0] == '\n' && !leadingBlank && !trailingBlank {
			if len(trailingBreaks) == 0 {
				s = append(s, ' ')
			}
		} else {
			s = append(s, leadingBreak...)
		}
		leadingBreak = leadingBreak[:0]
		s = append(s, trailingBreaks...)
		trailingBreaks = trailingBreaks[:0]
		leadingBlank = yamlIsBlank(p, 0)
		for !yamlIsBreakZ(p, 0) {
			s = yamlRead(p, s)
			if !yamlCache(p, 1) {
				return false
			}
		}
		if !yamlCache(p, 2) {
			return false
		}
		leadingBreak = yamlReadLine(p, leadingBreak)
		if !yamlParserScanBlockScalarBreaks(p, &indent, &trailingBreaks, startMark) {
			return false
		}
	}
	if chomping != -1 {
		s = append(s, leadingBreak...)
	}
	if chomping == 1 {
		s = append(s, trailingBreaks...)
	}
	style := yamlFoldedScalarStyle
	if literal {
		style = yamlLiteralScalarStyle
	}
	*t = yamlToken{typ: yamlScalarToken, startMark: startMark, value: s, style: style}
	return true
}

// Consumes indentation and empty lines before literal or folded content and settles the auto indent
func yamlParserScanBlockScalarBreaks(p *yamlParser, indent *int32, breaks *[]byte, startMark yamlMark) bool {
	var maxIndent int32
	for {
		if !yamlCache(p, 1) {
			return false
		}
		for (*indent == 0 || int32(p.mark.column) < *indent) && yamlIsSpace(p, 0) {
			yamlSkip(p)
			if !yamlCache(p, 1) {
				return false
			}
		}
		if int32(p.mark.column) > maxIndent {
			maxIndent = int32(p.mark.column)
		}
		if (*indent == 0 || int32(p.mark.column) < *indent) && yamlIsTab(p, 0) {
			return yamlParserSetScannerError(p, "while scanning a block scalar", startMark, "found a tab character where an indentation space is expected")
		}
		if !yamlIsBreak(p, 0) {
			break
		}
		if !yamlCache(p, 2) {
			return false
		}
		*breaks = yamlReadLine(p, *breaks)
	}
	if *indent == 0 {
		*indent = maxIndent
		if *indent < p.indent+1 {
			*indent = p.indent + 1
		}
		if *indent < 1 {
			*indent = 1
		}
	}
	return true
}

// Reads a single or double quoted scalar with escapes and line folding
func yamlParserScanFlowScalar(p *yamlParser, t *yamlToken, single bool) bool {
	s := yamlNewString()
	leadingBreak := yamlNewString()
	trailingBreaks := yamlNewString()
	whitespaces := yamlNewString()
	quote := byte('"')
	if single {
		quote = '\''
	}
	startMark := p.mark
	yamlSkip(p)
	for {
		if !yamlCache(p, 4) {
			return false
		}
		if p.mark.column == 0 &&
			((yamlAt(p, 0) == '-' && yamlAt(p, 1) == '-' && yamlAt(p, 2) == '-') ||
				(yamlAt(p, 0) == '.' && yamlAt(p, 1) == '.' && yamlAt(p, 2) == '.')) &&
			yamlIsBlankZ(p, 3) {
			return yamlParserSetScannerError(p, "while scanning a quoted scalar", startMark, "found unexpected document indicator")
		}
		if yamlIsZ(p, 0) {
			return yamlParserSetScannerError(p, "while scanning a quoted scalar", startMark, "found unexpected end of stream")
		}
		if !yamlCache(p, 2) {
			return false
		}
		leadingBlanks := false
		for !yamlIsBlankZ(p, 0) {
			if single && yamlAt(p, 0) == '\'' && yamlAt(p, 1) == '\'' {
				s = append(s, '\'')
				yamlSkip(p)
				yamlSkip(p)
			} else if yamlAt(p, 0) == quote {
				break
			} else if !single && yamlAt(p, 0) == '\\' && yamlIsBreak(p, 1) {
				if !yamlCache(p, 3) {
					return false
				}
				yamlSkip(p)
				yamlSkipLine(p)
				leadingBlanks = true
				break
			} else if !single && yamlAt(p, 0) == '\\' {
				codeLength := 0
				switch yamlAt(p, 1) {
				case '0':
					s = append(s, 0)
				case 'a':
					s = append(s, '\x07')
				case 'b':
					s = append(s, '\x08')
				case 't', '\t':
					s = append(s, '\x09')
				case 'n':
					s = append(s, '\x0A')
				case 'v':
					s = append(s, '\x0B')
				case 'f':
					s = append(s, '\x0C')
				case 'r':
					s = append(s, '\x0D')
				case 'e':
					s = append(s, '\x1B')
				case ' ':
					s = append(s, '\x20')
				case '"':
					s = append(s, '"')
				case '\\':
					s = append(s, '\\')
				case 'N':
					s = append(s, 0xC2, 0x85)
				case '_':
					s = append(s, 0xC2, 0xA0)
				case 'L':
					s = append(s, 0xE2, 0x80, 0xA8)
				case 'P':
					s = append(s, 0xE2, 0x80, 0xA9)
				case 'x':
					codeLength = 2
				case 'u':
					codeLength = 4
				case 'U':
					codeLength = 8
				case '/':
					s = append(s, '/')
				default:
					return yamlParserSetScannerError(p, "while parsing a quoted scalar", startMark, "found unknown escape character")
				}
				yamlSkip(p)
				yamlSkip(p)
				if codeLength != 0 {
					if !yamlCache(p, codeLength) {
						return false
					}
					value := 0
					for k := 0; k < codeLength; k++ {
						if !yamlIsHex(p, k) {
							return yamlParserSetScannerError(p, "while parsing a quoted scalar", startMark, "did not find expected hexdecimal number")
						}
						value = (value << 4) + yamlAsHex(p, k)
					}
					if (value >= 0xD800 && value <= 0xDFFF) || value > 0x10FFFF {
						return yamlParserSetScannerError(p, "while parsing a quoted scalar", startMark, "found invalid Unicode character escape code")
					}
					s = utf8.AppendRune(s, rune(value))
					for k := 0; k < codeLength; k++ {
						yamlSkip(p)
					}
				}
			} else {
				s = yamlRead(p, s)
			}
			if !yamlCache(p, 2) {
				return false
			}
		}
		if yamlAt(p, 0) == quote {
			break
		}
		if !yamlCache(p, 1) {
			return false
		}
		for yamlIsBlank(p, 0) || yamlIsBreak(p, 0) {
			if yamlIsBlank(p, 0) {
				if !leadingBlanks {
					whitespaces = yamlRead(p, whitespaces)
				} else {
					yamlSkip(p)
				}
			} else {
				if !yamlCache(p, 2) {
					return false
				}
				if !leadingBlanks {
					whitespaces = whitespaces[:0]
					leadingBreak = yamlReadLine(p, leadingBreak)
					leadingBlanks = true
				} else {
					trailingBreaks = yamlReadLine(p, trailingBreaks)
				}
			}
			if !yamlCache(p, 1) {
				return false
			}
		}
		if leadingBlanks {
			if len(leadingBreak) > 0 && leadingBreak[0] == '\n' {
				if len(trailingBreaks) == 0 {
					s = append(s, ' ')
				} else {
					s = append(s, trailingBreaks...)
					trailingBreaks = trailingBreaks[:0]
				}
				leadingBreak = leadingBreak[:0]
			} else {
				s = append(s, leadingBreak...)
				s = append(s, trailingBreaks...)
				leadingBreak = leadingBreak[:0]
				trailingBreaks = trailingBreaks[:0]
			}
		} else {
			s = append(s, whitespaces...)
			whitespaces = whitespaces[:0]
		}
	}
	yamlSkip(p)
	style := yamlDoubleQuotedScalarStyle
	if single {
		style = yamlSingleQuotedScalarStyle
	}
	*t = yamlToken{typ: yamlScalarToken, startMark: startMark, value: s, style: style}
	return true
}

// Reads an unquoted scalar across lines, stopping at indicators, comments, document markers or dedent
func yamlParserScanPlainScalar(p *yamlParser, t *yamlToken) bool {
	s := yamlNewString()
	leadingBreak := yamlNewString()
	trailingBreaks := yamlNewString()
	whitespaces := yamlNewString()
	leadingBlanks := false
	indent := p.indent + 1
	startMark := p.mark
	for {
		if !yamlCache(p, 4) {
			return false
		}
		if p.mark.column == 0 &&
			((yamlAt(p, 0) == '-' && yamlAt(p, 1) == '-' && yamlAt(p, 2) == '-') ||
				(yamlAt(p, 0) == '.' && yamlAt(p, 1) == '.' && yamlAt(p, 2) == '.')) &&
			yamlIsBlankZ(p, 3) {
			break
		}
		if yamlAt(p, 0) == '#' {
			break
		}
		for !yamlIsBlankZ(p, 0) {
			if p.flowLevel != 0 && yamlAt(p, 0) == ':' && !yamlIsBlankZ(p, 1) {
				return yamlParserSetScannerError(p, "while scanning a plain scalar", startMark, "found unexpected ':'")
			}
			if (yamlAt(p, 0) == ':' && yamlIsBlankZ(p, 1)) ||
				(p.flowLevel != 0 && strings.IndexByte(",:?[]{}", yamlAt(p, 0)) >= 0) {
				break
			}
			if leadingBlanks || len(whitespaces) != 0 {
				if leadingBlanks {
					if len(leadingBreak) > 0 && leadingBreak[0] == '\n' {
						if len(trailingBreaks) == 0 {
							s = append(s, ' ')
						} else {
							s = append(s, trailingBreaks...)
							trailingBreaks = trailingBreaks[:0]
						}
						leadingBreak = leadingBreak[:0]
					} else {
						s = append(s, leadingBreak...)
						s = append(s, trailingBreaks...)
						leadingBreak = leadingBreak[:0]
						trailingBreaks = trailingBreaks[:0]
					}
					leadingBlanks = false
				} else {
					s = append(s, whitespaces...)
					whitespaces = whitespaces[:0]
				}
			}
			s = yamlRead(p, s)
			if !yamlCache(p, 2) {
				return false
			}
		}
		if !(yamlIsBlank(p, 0) || yamlIsBreak(p, 0)) {
			break
		}
		if !yamlCache(p, 1) {
			return false
		}
		for yamlIsBlank(p, 0) || yamlIsBreak(p, 0) {
			if yamlIsBlank(p, 0) {
				if leadingBlanks && int32(p.mark.column) < indent && yamlIsTab(p, 0) {
					return yamlParserSetScannerError(p, "while scanning a plain scalar", startMark, "found a tab character that violates indentation")
				}
				if !leadingBlanks {
					whitespaces = yamlRead(p, whitespaces)
				} else {
					yamlSkip(p)
				}
			} else {
				if !yamlCache(p, 2) {
					return false
				}
				if !leadingBlanks {
					whitespaces = whitespaces[:0]
					leadingBreak = yamlReadLine(p, leadingBreak)
					leadingBlanks = true
				} else {
					trailingBreaks = yamlReadLine(p, trailingBreaks)
				}
			}
			if !yamlCache(p, 1) {
				return false
			}
		}
		if p.flowLevel == 0 && int32(p.mark.column) < indent {
			break
		}
	}
	*t = yamlToken{typ: yamlScalarToken, startMark: startMark, value: s, style: yamlPlainScalarStyle}
	if leadingBlanks {
		p.simpleKeyAllowed = true
	}
	return true
}

func yamlPeekToken(p *yamlParser) *yamlToken {
	if p.tokenAvailable || yamlParserFetchMoreTokens(p) {
		return &p.tokens[p.tokensHead]
	}
	return nil
}

func yamlSkipToken(p *yamlParser) {
	p.tokenAvailable = false
	p.tokensParsed++
	p.streamEndProduced = p.tokens[p.tokensHead].typ == yamlStreamEndToken
	p.tokensHead++
}

func yamlPushState(p *yamlParser, s yamlParserState) bool {
	return libyamlPush(p, &p.states, &p.statesCap, libyamlSizeofState, s)
}

func yamlPopState(p *yamlParser) yamlParserState {
	s := p.states[len(p.states)-1]
	p.states = p.states[:len(p.states)-1]
	return s
}

func yamlPushMark(p *yamlParser, m yamlMark) bool {
	return libyamlPush(p, &p.marks, &p.marksCap, libyamlSizeofMark, yamlStackMark{line: m.line, column: m.column})
}

func yamlPopMark(p *yamlParser) yamlMark {
	m := p.marks[len(p.marks)-1]
	p.marks = p.marks[:len(p.marks)-1]
	return yamlMark{line: m.line, column: m.column}
}

func yamlParserSetParserError(p *yamlParser, problem string, problemMark yamlMark) bool {
	p.errorType = yamlParserError
	p.problem = problem
	p.problemMark = problemMark
	return false
}

func yamlParserSetParserErrorContext(p *yamlParser, context string, contextMark yamlMark, problem string, problemMark yamlMark) bool {
	p.errorType = yamlParserError
	p.context = context
	p.contextMark = contextMark
	p.problem = problem
	p.problemMark = problemMark
	return false
}

// Produces the next event, doing nothing once the stream ended or an error was recorded
func yamlParserParse(p *yamlParser, ev *yamlEvent) bool {
	*ev = yamlEvent{}
	if p.streamEndProduced || p.errorType != yamlNoError || p.state == yamlParseEndState {
		return true
	}
	switch p.state {
	case yamlParseStreamStartState:
		return yamlParserParseStreamStart(p, ev)
	case yamlParseImplicitDocumentStartState:
		return yamlParserParseDocumentStart(p, ev, true)
	case yamlParseDocumentStartState:
		return yamlParserParseDocumentStart(p, ev, false)
	case yamlParseDocumentContentState:
		return yamlParserParseDocumentContent(p, ev)
	case yamlParseDocumentEndState:
		return yamlParserParseDocumentEnd(p, ev)
	case yamlParseBlockNodeState:
		return yamlParserParseNode(p, ev, true, false)
	case yamlParseBlockNodeOrIndentlessSequenceState:
		return yamlParserParseNode(p, ev, true, true)
	case yamlParseFlowNodeState:
		return yamlParserParseNode(p, ev, false, false)
	case yamlParseBlockSequenceFirstEntryState:
		return yamlParserParseBlockSequenceEntry(p, ev, true)
	case yamlParseBlockSequenceEntryState:
		return yamlParserParseBlockSequenceEntry(p, ev, false)
	case yamlParseIndentlessSequenceEntryState:
		return yamlParserParseIndentlessSequenceEntry(p, ev)
	case yamlParseBlockMappingFirstKeyState:
		return yamlParserParseBlockMappingKey(p, ev, true)
	case yamlParseBlockMappingKeyState:
		return yamlParserParseBlockMappingKey(p, ev, false)
	case yamlParseBlockMappingValueState:
		return yamlParserParseBlockMappingValue(p, ev)
	case yamlParseFlowSequenceFirstEntryState:
		return yamlParserParseFlowSequenceEntry(p, ev, true)
	case yamlParseFlowSequenceEntryState:
		return yamlParserParseFlowSequenceEntry(p, ev, false)
	case yamlParseFlowSequenceEntryMappingKeyState:
		return yamlParserParseFlowSequenceEntryMappingKey(p, ev)
	case yamlParseFlowSequenceEntryMappingValueState:
		return yamlParserParseFlowSequenceEntryMappingValue(p, ev)
	case yamlParseFlowSequenceEntryMappingEndState:
		return yamlParserParseFlowSequenceEntryMappingEnd(p, ev)
	case yamlParseFlowMappingFirstKeyState:
		return yamlParserParseFlowMappingKey(p, ev, true)
	case yamlParseFlowMappingKeyState:
		return yamlParserParseFlowMappingKey(p, ev, false)
	case yamlParseFlowMappingValueState:
		return yamlParserParseFlowMappingValue(p, ev, false)
	case yamlParseFlowMappingEmptyValueState:
		return yamlParserParseFlowMappingValue(p, ev, true)
	}
	return false
}

func yamlParserParseStreamStart(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlStreamStartToken {
		return yamlParserSetParserError(p, "did not find expected <stream-start>", token.startMark)
	}
	p.state = yamlParseImplicitDocumentStartState
	ev.typ = yamlStreamStartEvent
	yamlSkipToken(p)
	return true
}

// Starts an implicit or explicit document after its directives, or ends the stream
func yamlParserParseDocumentStart(p *yamlParser, ev *yamlEvent, implicit bool) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if !implicit {
		for token.typ == yamlDocumentEndToken {
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
	}
	if implicit && token.typ != yamlVersionDirectiveToken && token.typ != yamlTagDirectiveToken &&
		token.typ != yamlDocumentStartToken && token.typ != yamlStreamEndToken {
		if _, ok := yamlParserProcessDirectives(p); !ok {
			return false
		}
		if !yamlPushState(p, yamlParseDocumentEndState) {
			return false
		}
		p.state = yamlParseBlockNodeState
		ev.typ = yamlDocumentStartEvent
		return true
	}
	if token.typ != yamlStreamEndToken {
		tagDirectives, ok := yamlParserProcessDirectives(p)
		if !ok {
			return false
		}
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlDocumentStartToken {
			return yamlParserSetParserError(p, "did not find expected <document start>", token.startMark)
		}
		if !yamlPushState(p, yamlParseDocumentEndState) {
			return false
		}
		p.state = yamlParseDocumentContentState
		ev.typ = yamlDocumentStartEvent
		ev.tagDirectives = tagDirectives
		yamlSkipToken(p)
		return true
	}
	p.state = yamlParseEndState
	ev.typ = yamlStreamEndEvent
	yamlSkipToken(p)
	return true
}

func yamlParserParseDocumentContent(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	switch token.typ {
	case yamlVersionDirectiveToken, yamlTagDirectiveToken, yamlDocumentStartToken, yamlDocumentEndToken, yamlStreamEndToken:
		p.state = yamlPopState(p)
		return yamlParserProcessEmptyScalar(ev)
	}
	return yamlParserParseNode(p, ev, true, false)
}

func yamlParserParseDocumentEnd(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlDocumentEndToken {
		yamlSkipToken(p)
	}
	p.tagDirectives = p.tagDirectives[:0]
	p.state = yamlParseDocumentStartState
	ev.typ = yamlDocumentEndEvent
	return true
}

// Handles an alias or a node with its anchor and tag properties, resolving the tag handle
func yamlParserParseNode(p *yamlParser, ev *yamlEvent, block, indentlessSequence bool) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlAliasToken {
		p.state = yamlPopState(p)
		ev.typ = yamlAliasEvent
		ev.anchor = token.value
		yamlSkipToken(p)
		return true
	}
	var anchor, tagHandle, tagSuffix, tag []byte
	var tagMark yamlMark
	startMark := token.startMark
	if token.typ == yamlAnchorToken {
		anchor = token.value
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ == yamlTagToken {
			tagHandle, tagSuffix = token.handle, token.suffix
			tagMark = token.startMark
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
	} else if token.typ == yamlTagToken {
		tagHandle, tagSuffix = token.handle, token.suffix
		tagMark = token.startMark
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ == yamlAnchorToken {
			anchor = token.value
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
	}
	if tagHandle != nil {
		if len(tagHandle) == 0 {
			tag = cString(tagSuffix)
		} else {
			for _, d := range p.tagDirectives {
				if bytes.Equal(cString(d.handle), cString(tagHandle)) {
					tag = append(append(make([]byte, 0, len(d.prefix)+len(tagSuffix)), cString(d.prefix)...), cString(tagSuffix)...)
					break
				}
			}
			if tag == nil {
				return yamlParserSetParserErrorContext(p, "while parsing a node", startMark, "found undefined tag handle", tagMark)
			}
		}
	}
	implicit := tag == nil || len(tag) == 0
	if indentlessSequence && token.typ == yamlBlockEntryToken {
		p.state = yamlParseIndentlessSequenceEntryState
		*ev = yamlEvent{typ: yamlSequenceStartEvent, anchor: anchor, tag: tag, startMark: startMark, plainImplicit: implicit}
		return true
	}
	switch {
	case token.typ == yamlScalarToken:
		p.state = yamlPopState(p)
		plainImplicit := (token.style == yamlPlainScalarStyle && tag == nil) || (tag != nil && string(tag) == "!")
		quotedImplicit := !plainImplicit && tag == nil
		*ev = yamlEvent{typ: yamlScalarEvent, anchor: anchor, tag: tag, value: token.value, startMark: startMark, style: token.style,
			plainImplicit: plainImplicit, quotedImplicit: quotedImplicit}
		yamlSkipToken(p)
		return true
	case token.typ == yamlFlowSequenceStartToken:
		p.state = yamlParseFlowSequenceFirstEntryState
		*ev = yamlEvent{typ: yamlSequenceStartEvent, anchor: anchor, tag: tag, startMark: startMark, plainImplicit: implicit, flow: true}
		return true
	case token.typ == yamlFlowMappingStartToken:
		p.state = yamlParseFlowMappingFirstKeyState
		*ev = yamlEvent{typ: yamlMappingStartEvent, anchor: anchor, tag: tag, startMark: startMark, plainImplicit: implicit, flow: true}
		return true
	case block && token.typ == yamlBlockSequenceStartToken:
		p.state = yamlParseBlockSequenceFirstEntryState
		*ev = yamlEvent{typ: yamlSequenceStartEvent, anchor: anchor, tag: tag, startMark: startMark, plainImplicit: implicit}
		return true
	case block && token.typ == yamlBlockMappingStartToken:
		p.state = yamlParseBlockMappingFirstKeyState
		*ev = yamlEvent{typ: yamlMappingStartEvent, anchor: anchor, tag: tag, startMark: startMark, plainImplicit: implicit}
		return true
	case anchor != nil || tag != nil:
		p.state = yamlPopState(p)
		*ev = yamlEvent{typ: yamlScalarEvent, anchor: anchor, tag: tag, value: []byte{}, startMark: startMark, style: yamlPlainScalarStyle,
			plainImplicit: implicit}
		return true
	}
	context := "while parsing a flow node"
	if block {
		context = "while parsing a block node"
	}
	return yamlParserSetParserErrorContext(p, context, startMark, "did not find expected node content", token.startMark)
}

func yamlParserParseBlockSequenceEntry(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlBlockEntryToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlBlockEntryToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseBlockSequenceEntryState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, false)
		}
		p.state = yamlParseBlockSequenceEntryState
		return yamlParserProcessEmptyScalar(ev)
	}
	if token.typ == yamlBlockEndToken {
		p.state = yamlPopState(p)
		yamlPopMark(p)
		ev.typ = yamlSequenceEndEvent
		yamlSkipToken(p)
		return true
	}
	return yamlParserSetParserErrorContext(p, "while parsing a block collection", yamlPopMark(p), "did not find expected '-' indicator", token.startMark)
}

func yamlParserParseIndentlessSequenceEntry(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlBlockEntryToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlBlockEntryToken && token.typ != yamlKeyToken && token.typ != yamlValueToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseIndentlessSequenceEntryState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, false)
		}
		p.state = yamlParseIndentlessSequenceEntryState
		return yamlParserProcessEmptyScalar(ev)
	}
	p.state = yamlPopState(p)
	ev.typ = yamlSequenceEndEvent
	return true
}

func yamlParserParseBlockMappingKey(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlKeyToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlKeyToken && token.typ != yamlValueToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseBlockMappingValueState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, true)
		}
		p.state = yamlParseBlockMappingValueState
		return yamlParserProcessEmptyScalar(ev)
	}
	if token.typ == yamlBlockEndToken {
		p.state = yamlPopState(p)
		yamlPopMark(p)
		ev.typ = yamlMappingEndEvent
		yamlSkipToken(p)
		return true
	}
	return yamlParserSetParserErrorContext(p, "while parsing a block mapping", yamlPopMark(p), "did not find expected key", token.startMark)
}

func yamlParserParseBlockMappingValue(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlValueToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlKeyToken && token.typ != yamlValueToken && token.typ != yamlBlockEndToken {
			if !yamlPushState(p, yamlParseBlockMappingKeyState) {
				return false
			}
			return yamlParserParseNode(p, ev, true, true)
		}
	}
	p.state = yamlParseBlockMappingKeyState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserParseFlowSequenceEntry(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlFlowSequenceEndToken {
		if !first {
			if token.typ != yamlFlowEntryToken {
				return yamlParserSetParserErrorContext(p, "while parsing a flow sequence", yamlPopMark(p), "did not find expected ',' or ']'", token.startMark)
			}
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
		if token.typ == yamlKeyToken {
			p.state = yamlParseFlowSequenceEntryMappingKeyState
			ev.typ = yamlMappingStartEvent
			yamlSkipToken(p)
			return true
		}
		if token.typ != yamlFlowSequenceEndToken {
			if !yamlPushState(p, yamlParseFlowSequenceEntryState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlPopState(p)
	yamlPopMark(p)
	ev.typ = yamlSequenceEndEvent
	yamlSkipToken(p)
	return true
}

// Handles the key of a single pair mapping inside a flow sequence, consuming the token when the key is empty
func yamlParserParseFlowSequenceEntryMappingKey(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlValueToken && token.typ != yamlFlowEntryToken && token.typ != yamlFlowSequenceEndToken {
		if !yamlPushState(p, yamlParseFlowSequenceEntryMappingValueState) {
			return false
		}
		return yamlParserParseNode(p, ev, false, false)
	}
	yamlSkipToken(p)
	p.state = yamlParseFlowSequenceEntryMappingValueState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserParseFlowSequenceEntryMappingValue(p *yamlParser, ev *yamlEvent) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ == yamlValueToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlFlowEntryToken && token.typ != yamlFlowSequenceEndToken {
			if !yamlPushState(p, yamlParseFlowSequenceEntryMappingEndState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlParseFlowSequenceEntryMappingEndState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserParseFlowSequenceEntryMappingEnd(p *yamlParser, ev *yamlEvent) bool {
	if yamlPeekToken(p) == nil {
		return false
	}
	p.state = yamlParseFlowSequenceEntryState
	ev.typ = yamlMappingEndEvent
	return true
}

func yamlParserParseFlowMappingKey(p *yamlParser, ev *yamlEvent, first bool) bool {
	if first {
		token := yamlPeekToken(p)
		if token == nil {
			return false
		}
		if !yamlPushMark(p, token.startMark) {
			return false
		}
		yamlSkipToken(p)
	}
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if token.typ != yamlFlowMappingEndToken {
		if !first {
			if token.typ != yamlFlowEntryToken {
				return yamlParserSetParserErrorContext(p, "while parsing a flow mapping", yamlPopMark(p), "did not find expected ',' or '}'", token.startMark)
			}
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
		}
		if token.typ == yamlKeyToken {
			yamlSkipToken(p)
			if token = yamlPeekToken(p); token == nil {
				return false
			}
			if token.typ != yamlValueToken && token.typ != yamlFlowEntryToken && token.typ != yamlFlowMappingEndToken {
				if !yamlPushState(p, yamlParseFlowMappingValueState) {
					return false
				}
				return yamlParserParseNode(p, ev, false, false)
			}
			p.state = yamlParseFlowMappingValueState
			return yamlParserProcessEmptyScalar(ev)
		}
		if token.typ != yamlFlowMappingEndToken {
			if !yamlPushState(p, yamlParseFlowMappingEmptyValueState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlPopState(p)
	yamlPopMark(p)
	ev.typ = yamlMappingEndEvent
	yamlSkipToken(p)
	return true
}

func yamlParserParseFlowMappingValue(p *yamlParser, ev *yamlEvent, empty bool) bool {
	token := yamlPeekToken(p)
	if token == nil {
		return false
	}
	if empty {
		p.state = yamlParseFlowMappingKeyState
		return yamlParserProcessEmptyScalar(ev)
	}
	if token.typ == yamlValueToken {
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return false
		}
		if token.typ != yamlFlowEntryToken && token.typ != yamlFlowMappingEndToken {
			if !yamlPushState(p, yamlParseFlowMappingKeyState) {
				return false
			}
			return yamlParserParseNode(p, ev, false, false)
		}
	}
	p.state = yamlParseFlowMappingKeyState
	return yamlParserProcessEmptyScalar(ev)
}

func yamlParserProcessEmptyScalar(ev *yamlEvent) bool {
	*ev = yamlEvent{typ: yamlScalarEvent, value: []byte{}, style: yamlPlainScalarStyle, plainImplicit: true}
	return true
}

// Applies the YAML and TAG directives before a document and installs the default tag handles
func yamlParserProcessDirectives(p *yamlParser) ([]yamlTagDirective, bool) {
	haveVersion := false
	var local []yamlTagDirective
	localCap := libyamlInitialStackSize
	token := yamlPeekToken(p)
	if token == nil {
		return nil, false
	}
	for token.typ == yamlVersionDirectiveToken || token.typ == yamlTagDirectiveToken {
		if token.typ == yamlVersionDirectiveToken {
			if haveVersion {
				return nil, yamlParserSetParserError(p, "found duplicate %YAML directive", token.startMark)
			}
			if token.major != 1 || (token.minor != 1 && token.minor != 2) {
				return nil, yamlParserSetParserError(p, "found incompatible YAML document", token.startMark)
			}
			haveVersion = true
		} else {
			value := yamlTagDirective{handle: token.handle, prefix: token.suffix}
			if !yamlParserAppendTagDirective(p, value, false, token.startMark) {
				return nil, false
			}
			if !libyamlPush(p, &local, &localCap, libyamlSizeofTagDirective, value) {
				return nil, false
			}
		}
		yamlSkipToken(p)
		if token = yamlPeekToken(p); token == nil {
			return nil, false
		}
	}
	defaults := [...]yamlTagDirective{
		{handle: []byte("!"), prefix: []byte("!")},
		{handle: []byte("!!"), prefix: []byte("tag:yaml.org,2002:")},
	}
	for _, d := range defaults {
		if !yamlParserAppendTagDirective(p, d, true, token.startMark) {
			return nil, false
		}
	}
	return local, true
}

func yamlParserAppendTagDirective(p *yamlParser, value yamlTagDirective, allowDuplicates bool, mark yamlMark) bool {
	for _, d := range p.tagDirectives {
		if bytes.Equal(value.handle, d.handle) {
			if allowDuplicates {
				return true
			}
			return yamlParserSetParserError(p, "found duplicate %TAG directive", mark)
		}
	}
	return libyamlPush(p, &p.tagDirectives, &p.tagDirectivesCap, libyamlSizeofTagDirective, value)
}

// Bytes up to the first NUL, the text a C string function sees
func cString(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i]
	}
	return b
}
