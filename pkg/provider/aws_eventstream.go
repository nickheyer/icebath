package provider

import (
	"bytes"
	"encoding/base64"
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

const (
	awsEventStreamPreludeLen  = 12
	awsEventStreamMaxHeaders  = 128 * 1024
	awsEventStreamMaxPayload  = 24 * 1024 * 1024
	awsEventStreamMinFrameLen = awsEventStreamPreludeLen + 4
)

// Wire type codes of event stream header values
const (
	awsHeaderTrue      byte = 0
	awsHeaderFalse     byte = 1
	awsHeaderByte      byte = 2
	awsHeaderShort     byte = 3
	awsHeaderInteger   byte = 4
	awsHeaderLong      byte = 5
	awsHeaderBytes     byte = 6
	awsHeaderString    byte = 7
	awsHeaderTimestamp byte = 8
	awsHeaderUUID      byte = 9
)

// One typed event stream header in wire order
type awsEventStreamHeader struct {
	Name  string
	Type  byte
	Value any
}

// One decoded event stream frame
type awsEventStreamMessage struct {
	Headers []awsEventStreamHeader
	Payload []byte
}

// String value of a header, empty when absent or not a string
func (m *awsEventStreamMessage) header(name string) string {
	for _, h := range m.Headers {
		if h.Name == name {
			if s, ok := h.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

// Botocore parser error class of a malformed frame
type AWSEventStreamFrameError struct {
	Kind   string
	Detail string
}

func (e *AWSEventStreamFrameError) Error() string {
	return "aws event stream " + e.Kind + ": " + e.Detail
}

// Exception or error frame Bedrock sends mid stream
type AWSEventStreamException struct {
	MessageType   string
	ExceptionType string
	Message       string
	Payload       []byte
	Headers       []awsEventStreamHeader
}

func (e *AWSEventStreamException) Error() string {
	return fmt.Sprintf("An error occurred (%s) when processing the event stream: %s", e.ExceptionType, e.Message)
}

// Typed exception of an exception or error frame, nil for an event frame
func awsEventStreamFrameException(m *awsEventStreamMessage) *AWSEventStreamException {
	switch m.header(":message-type") {
	case "exception":
		exc := &AWSEventStreamException{MessageType: "exception", ExceptionType: m.header(":exception-type"), Payload: m.Payload, Headers: m.Headers}
		var body map[string]any
		if json.Unmarshal(m.Payload, &body) == nil {
			exc.Message = stringOf(body["message"])
			if exc.Message == "" {
				exc.Message = stringOf(body["Message"])
			}
		}
		if exc.Message == "" {
			exc.Message = string(m.Payload)
		}
		return exc
	case "error":
		return &AWSEventStreamException{MessageType: "error", ExceptionType: m.header(":error-code"), Message: m.header(":error-message"), Payload: m.Payload, Headers: m.Headers}
	}
	return nil
}

// HTTP status botocore models for each bedrock-runtime exception shape
var bedrockExceptionStatus = map[string]int{
	"accessdeniedexception": 403, "conflictexception": 400, "internalserverexception": 500, "modelerrorexception": 424,
	"modelnotreadyexception": 429, "modelstreamerrorexception": 424, "modeltimeoutexception": 408, "resourcenotfoundexception": 404,
	"servicequotaexceededexception": 400, "serviceunavailableexception": 503, "throttlingexception": 429, "validationexception": 400,
}

// Provider stream error carrying the modelled HTTP status of a mid stream exception
func (e *AWSEventStreamException) streamError(headers http.Header) *ProviderStreamError {
	status, ok := bedrockExceptionStatus[strings.ToLower(e.ExceptionType)]
	if !ok {
		status = 400
	}
	return &ProviderStreamError{StatusCode: status, RawText: string(e.Payload), Headers: headers,
		Body: map[string]any{"type": "error", "error": map[string]any{"type": e.ExceptionType, "message": e.Message}}}
}

// Frame reader over a streaming body that tolerates arbitrary chunk boundaries
type awsEventStreamReader struct {
	r io.Reader
}

func newAWSEventStreamReader(r io.Reader) *awsEventStreamReader {
	return &awsEventStreamReader{r: r}
}

// Next frame, io.EOF at a clean frame boundary and a frame error on truncation or checksum mismatch
func (s *awsEventStreamReader) Next() (*awsEventStreamMessage, error) {
	prelude := make([]byte, awsEventStreamPreludeLen)
	if n, err := io.ReadFull(s.r, prelude); err != nil {
		if err == io.EOF && n == 0 {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, &AWSEventStreamFrameError{Kind: "TruncatedFrame", Detail: fmt.Sprintf("stream ended after %d prelude bytes", n)}
		}
		return nil, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if err := awsCheckPrelude(prelude, total, headersLen); err != nil {
		return nil, err
	}
	rest := make([]byte, int(total)-awsEventStreamPreludeLen)
	if n, err := io.ReadFull(s.r, rest); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, &AWSEventStreamFrameError{Kind: "TruncatedFrame", Detail: fmt.Sprintf("stream ended %d bytes into a %d byte frame", n+awsEventStreamPreludeLen, total)}
		}
		return nil, err
	}
	return awsDecodeEventStreamBody(prelude, rest, headersLen)
}

// Validates the prelude checksum and the header and payload length bounds
func awsCheckPrelude(prelude []byte, total, headersLen uint32) error {
	want := binary.BigEndian.Uint32(prelude[8:12])
	if got := crc32.ChecksumIEEE(prelude[:8]); got != want {
		return &AWSEventStreamFrameError{Kind: "ChecksumMismatch", Detail: fmt.Sprintf("Checksum mismatch: expected 0x%08x, calculated 0x%08x", want, got)}
	}
	if headersLen > awsEventStreamMaxHeaders {
		return &AWSEventStreamFrameError{Kind: "InvalidHeadersLength", Detail: fmt.Sprintf("Header length of %d exceeded the maximum of %d", headersLen, awsEventStreamMaxHeaders)}
	}
	if total < awsEventStreamMinFrameLen || uint64(headersLen)+awsEventStreamMinFrameLen > uint64(total) {
		return &AWSEventStreamFrameError{Kind: "InvalidFrameLength", Detail: fmt.Sprintf("total length %d cannot hold %d header bytes", total, headersLen)}
	}
	if payload := uint64(total) - uint64(headersLen) - awsEventStreamMinFrameLen; payload > awsEventStreamMaxPayload {
		return &AWSEventStreamFrameError{Kind: "InvalidPayloadLength", Detail: fmt.Sprintf("Payload length of %d exceeded the maximum of %d", payload, awsEventStreamMaxPayload)}
	}
	return nil
}

// Checks the message checksum then splits the headers and payload of one frame
func awsDecodeEventStreamBody(prelude, rest []byte, headersLen uint32) (*awsEventStreamMessage, error) {
	body, crcBytes := rest[:len(rest)-4], rest[len(rest)-4:]
	want := binary.BigEndian.Uint32(crcBytes)
	got := crc32.Update(crc32.ChecksumIEEE(prelude), crc32.IEEETable, body)
	if got != want {
		return nil, &AWSEventStreamFrameError{Kind: "ChecksumMismatch", Detail: fmt.Sprintf("Checksum mismatch: expected 0x%08x, calculated 0x%08x", want, got)}
	}
	headers, err := awsDecodeEventStreamHeaders(body[:headersLen])
	if err != nil {
		return nil, err
	}
	payload := make([]byte, len(body)-int(headersLen))
	copy(payload, body[headersLen:])
	return &awsEventStreamMessage{Headers: headers, Payload: payload}, nil
}

// Decodes every typed header of the headers block
func awsDecodeEventStreamHeaders(data []byte) ([]awsEventStreamHeader, error) {
	var out []awsEventStreamHeader
	short := func(what string) error {
		return &AWSEventStreamFrameError{Kind: "InvalidHeaders", Detail: "headers block ends inside " + what}
	}
	for i := 0; i < len(data); {
		nameLen := int(data[i])
		i++
		if i+nameLen+1 > len(data) {
			return nil, short("a header name")
		}
		name := string(data[i : i+nameLen])
		i += nameLen
		typ := data[i]
		i++
		need := map[byte]int{awsHeaderTrue: 0, awsHeaderFalse: 0, awsHeaderByte: 1, awsHeaderShort: 2, awsHeaderInteger: 4, awsHeaderLong: 8, awsHeaderTimestamp: 8, awsHeaderUUID: 16}
		n, fixed := need[typ]
		if !fixed {
			if typ != awsHeaderBytes && typ != awsHeaderString {
				return nil, &AWSEventStreamFrameError{Kind: "InvalidHeaders", Detail: fmt.Sprintf("header %q has unknown value type %d", name, typ)}
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
		var v any
		switch typ {
		case awsHeaderTrue:
			v = true
		case awsHeaderFalse:
			v = false
		case awsHeaderByte:
			v = int8(raw[0])
		case awsHeaderShort:
			v = int16(binary.BigEndian.Uint16(raw))
		case awsHeaderInteger:
			v = int32(binary.BigEndian.Uint32(raw))
		case awsHeaderLong:
			v = int64(binary.BigEndian.Uint64(raw))
		case awsHeaderTimestamp:
			v = time.UnixMilli(int64(binary.BigEndian.Uint64(raw))).UTC()
		case awsHeaderUUID, awsHeaderBytes:
			v = append([]byte(nil), raw...)
		case awsHeaderString:
			if !utf8.Valid(raw) {
				return nil, &AWSEventStreamFrameError{Kind: "InvalidHeaders", Detail: fmt.Sprintf("header %q is not valid utf-8", name)}
			}
			v = string(raw)
		}
		out = append(out, awsEventStreamHeader{Name: name, Type: typ, Value: v})
	}
	return out, nil
}

// Wire bytes of one frame with both checksums, the encoder fakes and tests replay through
func awsEncodeEventStreamMessage(headers []awsEventStreamHeader, payload []byte) ([]byte, error) {
	var hb bytes.Buffer
	for _, h := range headers {
		if len(h.Name) > 255 {
			return nil, fmt.Errorf("aws event stream header name %q exceeds 255 bytes", h.Name)
		}
		hb.WriteByte(byte(len(h.Name)))
		hb.WriteString(h.Name)
		hb.WriteByte(h.Type)
		var raw []byte
		switch h.Type {
		case awsHeaderTrue, awsHeaderFalse:
		case awsHeaderByte:
			raw = []byte{byte(h.Value.(int8))}
		case awsHeaderShort:
			raw = binary.BigEndian.AppendUint16(nil, uint16(h.Value.(int16)))
		case awsHeaderInteger:
			raw = binary.BigEndian.AppendUint32(nil, uint32(h.Value.(int32)))
		case awsHeaderLong:
			raw = binary.BigEndian.AppendUint64(nil, uint64(h.Value.(int64)))
		case awsHeaderTimestamp:
			raw = binary.BigEndian.AppendUint64(nil, uint64(h.Value.(time.Time).UnixMilli()))
		case awsHeaderUUID:
			raw = h.Value.([]byte)
			if len(raw) != 16 {
				return nil, fmt.Errorf("aws event stream uuid header %q must be 16 bytes", h.Name)
			}
		case awsHeaderBytes, awsHeaderString:
			if s, ok := h.Value.(string); ok {
				raw = []byte(s)
			} else {
				raw = h.Value.([]byte)
			}
			if len(raw) > 0xffff {
				return nil, fmt.Errorf("aws event stream header %q value exceeds 65535 bytes", h.Name)
			}
			hb.Write(binary.BigEndian.AppendUint16(nil, uint16(len(raw))))
		default:
			return nil, fmt.Errorf("aws event stream header %q has unknown value type %d", h.Name, h.Type)
		}
		hb.Write(raw)
	}
	total := awsEventStreamMinFrameLen + hb.Len() + len(payload)
	out := make([]byte, 0, total)
	out = binary.BigEndian.AppendUint32(out, uint32(total))
	out = binary.BigEndian.AppendUint32(out, uint32(hb.Len()))
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[:8]))
	out = append(out, hb.Bytes()...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out)), nil
}

// String header triple of an event frame with the given event type
func awsEventHeaders(eventType, contentType string) []awsEventStreamHeader {
	return []awsEventStreamHeader{
		{Name: ":event-type", Type: awsHeaderString, Value: eventType},
		{Name: ":content-type", Type: awsHeaderString, Value: contentType},
		{Name: ":message-type", Type: awsHeaderString, Value: "event"},
	}
}

// Anthropic events decoded from the chunk frames of InvokeModelWithResponseStream
type bedrockInvokeEvents struct {
	r       *awsEventStreamReader
	headers http.Header
}

func newBedrockInvokeEvents(body io.Reader, headers http.Header) *bedrockInvokeEvents {
	return &bedrockInvokeEvents{r: newAWSEventStreamReader(body), headers: headers}
}

// Next anthropic event, skipping pings and non chunk frames, a provider stream error for exception frames
func (s *bedrockInvokeEvents) Next() (*anthEvent, error) {
	for {
		m, err := s.r.Next()
		if err != nil {
			return nil, err
		}
		if exc := awsEventStreamFrameException(m); exc != nil {
			return nil, exc.streamError(s.headers)
		}
		if m.header(":event-type") != "chunk" {
			continue
		}
		var chunk struct {
			Bytes *string `json:"bytes"`
		}
		if err := json.Unmarshal(m.Payload, &chunk); err != nil || chunk.Bytes == nil {
			return nil, providerStreamErrorFromNonJSON(string(m.Payload), s.headers)
		}
		data, err := base64.StdEncoding.DecodeString(*chunk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("provider: bedrock chunk bytes are not base64: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, providerStreamErrorFromNonJSON(string(data), s.headers)
		}
		typ := asStr(payload["type"])
		if typ == "ping" {
			continue
		}
		return &anthEvent{Type: typ, Payload: payload, Raw: string(data)}, nil
	}
}

// Chunk frame wrapping one anthropic event the way InvokeModelWithResponseStream sends it
func bedrockInvokeChunkFrame(event map[string]any) ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return nil, err
	}
	return awsEncodeEventStreamMessage(awsEventHeaders("chunk", "application/json"), payload)
}

// Exception frame of the given exception type and message
func awsEventStreamExceptionFrame(exceptionType, message string) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return nil, err
	}
	return awsEncodeEventStreamMessage([]awsEventStreamHeader{
		{Name: ":exception-type", Type: awsHeaderString, Value: exceptionType},
		{Name: ":content-type", Type: awsHeaderString, Value: "application/json"},
		{Name: ":message-type", Type: awsHeaderString, Value: "exception"},
	}, payload)
}
