package transport

import (
	"cmp"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds of an AWS event stream frame
const (
	preludeLength  = 12
	maxHeaderBytes = 128 * 1024
	maxPayload     = 24 * 1024 * 1024
	minFrame       = preludeLength + 4
)

// Value types of an event stream header, as the wire codes them
const (
	headerTrue byte = iota
	headerFalse
	headerByte
	headerShort
	headerInteger
	headerLong
	headerBytes
	headerString
	headerTimestamp
	headerUUID
)

// Bytes the value of each fixed size header type takes
var headerSizes = map[byte]int{headerTrue: 0, headerFalse: 0, headerByte: 1, headerShort: 2, headerInteger: 4, headerLong: 8, headerTimestamp: 8,
	headerUUID: 16}

// One frame of an AWS event stream, as Bedrock streams its replies: its typed headers in order and its payload
type EventMessage struct {
	Headers []EventHeader
	Payload []byte
}

type EventHeader struct {
	Name  string
	Value any
}

// Text of the header, empty when the frame has none or it isn't text
func (m *EventMessage) Header(name string) string {
	for _, h := range m.Headers {
		if h.Name == name {
			if s, ok := h.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

// Frame of an event stream that can't be read: cut short, its checksum wrong or its lengths out of bounds
type FrameReadError struct {
	Kind   string
	Detail string
}

func (e *FrameReadError) Error() string {
	return "The AWS reply stream could not be read, " + e.Kind + ": " + strings.TrimSuffix(e.Detail, ".") + "."
}

// HTTP status each exception of the Bedrock runtime stands for
var exceptionStatus = map[string]int{
	"accessdeniedexception": 403, "conflictexception": 400, "internalserverexception": 500, "modelerrorexception": 424,
	"modelnotreadyexception": 429, "modelstreamerrorexception": 424, "modeltimeoutexception": 408, "resourcenotfoundexception": 404,
	"servicequotaexceededexception": 400, "serviceunavailableexception": 503, "throttlingexception": 429, "validationexception": 400,
}

// Error of an exception or error frame the service sent mid stream, nil for an event frame
func (m *EventMessage) Exception(header http.Header) *StreamError {
	kind, message := "", ""
	switch m.Header(":message-type") {
	case "exception":
		kind = m.Header(":exception-type")
		var body map[string]any
		if json.Unmarshal(m.Payload, &body) == nil {
			message = first(body["message"], body["Message"])
		}
		message = cmp.Or(message, string(m.Payload))
	case "error":
		kind, message = m.Header(":error-code"), m.Header(":error-message")
	default:
		return nil
	}
	status, ok := exceptionStatus[strings.ToLower(kind)]
	if !ok {
		status = 400
	}
	return &StreamError{Status: status, Body: ErrorBody(map[string]any{"code": kind, "type": kind, "message": message}, status), Text: string(m.Payload),
		Header: header}
}

// Reads the frames of an AWS event stream one at a time, whatever the chunks the body arrives in
type EventStream struct {
	r io.Reader
}

func NewEventStream(r io.Reader) *EventStream {
	return &EventStream{r: r}
}

// The next frame, io.EOF at a clean end between frames
func (s *EventStream) Next() (*EventMessage, error) {
	prelude := make([]byte, preludeLength)
	if n, err := io.ReadFull(s.r, prelude); err != nil {
		if err == io.EOF && n == 0 {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, &FrameReadError{Kind: "TruncatedFrame", Detail: fmt.Sprintf("stream ended after %d prelude bytes", n)}
		}
		return nil, err
	}
	total, headers := binary.BigEndian.Uint32(prelude[0:4]), binary.BigEndian.Uint32(prelude[4:8])
	if want, got := binary.BigEndian.Uint32(prelude[8:12]), crc32.ChecksumIEEE(prelude[:8]); got != want {
		return nil, &FrameReadError{Kind: "ChecksumMismatch", Detail: fmt.Sprintf("Checksum mismatch: expected 0x%08x, calculated 0x%08x", want, got)}
	}
	switch {
	case headers > maxHeaderBytes:
		return nil, &FrameReadError{Kind: "InvalidHeadersLength", Detail: fmt.Sprintf("Header length of %d exceeded the maximum of %d", headers, maxHeaderBytes)}
	case total < minFrame || uint64(headers)+minFrame > uint64(total):
		return nil, &FrameReadError{Kind: "InvalidFrameLength", Detail: fmt.Sprintf("total length %d cannot hold %d header bytes", total, headers)}
	case uint64(total)-uint64(headers)-minFrame > maxPayload:
		return nil, &FrameReadError{Kind: "InvalidPayloadLength", Detail: fmt.Sprintf("Payload length of %d exceeded the maximum of %d",
			uint64(total)-uint64(headers)-minFrame, maxPayload)}
	}
	rest := make([]byte, int(total)-preludeLength)
	if n, err := io.ReadFull(s.r, rest); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, &FrameReadError{Kind: "TruncatedFrame", Detail: fmt.Sprintf("stream ended %d bytes into a %d byte frame", n+preludeLength, total)}
		}
		return nil, err
	}
	body, sum := rest[:len(rest)-4], binary.BigEndian.Uint32(rest[len(rest)-4:])
	if got := crc32.Update(crc32.ChecksumIEEE(prelude), crc32.IEEETable, body); got != sum {
		return nil, &FrameReadError{Kind: "ChecksumMismatch", Detail: fmt.Sprintf("Checksum mismatch: expected 0x%08x, calculated 0x%08x", sum, got)}
	}
	parsed, err := eventHeaders(body[:headers])
	if err != nil {
		return nil, err
	}
	return &EventMessage{Headers: parsed, Payload: append([]byte(nil), body[headers:]...)}, nil
}

// Typed headers of a frame's header block
func eventHeaders(data []byte) ([]EventHeader, error) {
	var out []EventHeader
	short := func(what string) error {
		return &FrameReadError{Kind: "InvalidHeaders", Detail: "headers block ends inside " + what}
	}
	for i := 0; i < len(data); {
		size := int(data[i])
		i++
		if i+size+1 > len(data) {
			return nil, short("a header name")
		}
		name := string(data[i : i+size])
		i += size
		kind := data[i]
		i++
		n, fixed := headerSizes[kind]
		if !fixed {
			if kind != headerBytes && kind != headerString {
				return nil, &FrameReadError{Kind: "InvalidHeaders", Detail: fmt.Sprintf("header %q has unknown value type %d", name, kind)}
			}
			if i+2 > len(data) {
				return nil, short("a value length")
			}
			n = int(binary.BigEndian.Uint16(data[i : i+2]))
			i += 2
		}
		if i+n > len(data) {
			return nil, short("a header value")
		}
		raw := data[i : i+n]
		i += n
		var value any
		switch kind {
		case headerTrue:
			value = true
		case headerFalse:
			value = false
		case headerByte:
			value = int8(raw[0])
		case headerShort:
			value = int16(binary.BigEndian.Uint16(raw))
		case headerInteger:
			value = int32(binary.BigEndian.Uint32(raw))
		case headerLong:
			value = int64(binary.BigEndian.Uint64(raw))
		case headerTimestamp:
			value = time.UnixMilli(int64(binary.BigEndian.Uint64(raw))).UTC()
		case headerUUID, headerBytes:
			value = append([]byte(nil), raw...)
		case headerString:
			if !utf8.Valid(raw) {
				return nil, &FrameReadError{Kind: "InvalidHeaders", Detail: fmt.Sprintf("header %q is not valid utf-8", name)}
			}
			value = string(raw)
		}
		out = append(out, EventHeader{Name: name, Value: value})
	}
	return out, nil
}
