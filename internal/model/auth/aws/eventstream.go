package aws

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

const (
	preludeLength   = 12
	maxHeaderLength = 128 << 10
	maxPayloadBytes = 24 << 20
)

var (
	ErrChecksum       = errors.New("aws: event stream frame failed its checksum")
	ErrFrameTruncated = errors.New("aws: event stream ended inside a frame")
	ErrFrameMalformed = errors.New("aws: event stream frame is malformed")
)

type FrameSizeError struct {
	Part  string
	Size  int
	Limit int
}

func (e *FrameSizeError) Error() string {
	return fmt.Sprintf("aws: event stream %s of %d bytes is over the limit of %d", e.Part, e.Size, e.Limit)
}

type HeaderKind uint8

const (
	HeaderTrue HeaderKind = iota
	HeaderFalse
	HeaderByte
	HeaderShort
	HeaderInt
	HeaderLong
	HeaderBytes
	HeaderString
	HeaderTimestamp
	HeaderUUID
)

type FrameHeader struct {
	Name  string
	Kind  HeaderKind
	Bool  bool
	Int   int64
	Bytes []byte
	Text  string
	Time  time.Time
}

type Frame struct {
	Headers []FrameHeader
	Payload []byte
}

func (f *Frame) Header(name string) string {
	for i := range f.Headers {
		if f.Headers[i].Name == name {
			return f.Headers[i].Text
		}
	}
	return ""
}

func (f *Frame) MessageType() string { return f.Header(":message-type") }

func (f *Frame) EventType() string { return f.Header(":event-type") }

func (f *Frame) ExceptionType() string { return f.Header(":exception-type") }

func (f *Frame) ErrorCode() string { return f.Header(":error-code") }

func (f *Frame) ErrorMessage() string { return f.Header(":error-message") }

type FrameReader struct {
	r *bufio.Reader
}

func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, 32<<10)}
}

func (d *FrameReader) Next(ctx context.Context) (*Frame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var prelude [preludeLength]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrFrameTruncated
		}
		return nil, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headerLen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return nil, ErrChecksum
	}
	if headerLen > maxHeaderLength {
		return nil, &FrameSizeError{Part: "header block", Size: int(headerLen), Limit: maxHeaderLength}
	}
	if total < preludeLength+4 || uint64(headerLen) > uint64(total)-preludeLength-4 {
		return nil, ErrFrameMalformed
	}
	payloadLen := total - preludeLength - 4 - headerLen
	if payloadLen > maxPayloadBytes {
		return nil, &FrameSizeError{Part: "payload", Size: int(payloadLen), Limit: maxPayloadBytes}
	}
	rest := make([]byte, total-preludeLength)
	if _, err := io.ReadFull(d.r, rest); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrFrameTruncated
		}
		return nil, err
	}
	body := rest[:len(rest)-4]
	sum := crc32.Update(crc32.ChecksumIEEE(prelude[:]), crc32.IEEETable, body)
	if sum != binary.BigEndian.Uint32(rest[len(rest)-4:]) {
		return nil, ErrChecksum
	}
	headers, err := parseFrameHeaders(body[:headerLen])
	if err != nil {
		return nil, err
	}
	return &Frame{Headers: headers, Payload: body[headerLen:]}, nil
}

func parseFrameHeaders(data []byte) ([]FrameHeader, error) {
	var out []FrameHeader
	take := func(n int) ([]byte, bool) {
		if n < 0 || len(data) < n {
			return nil, false
		}
		chunk := data[:n]
		data = data[n:]
		return chunk, true
	}
	for len(data) > 0 {
		nameLen, ok := take(1)
		if !ok {
			return nil, ErrFrameMalformed
		}
		name, ok := take(int(nameLen[0]))
		if !ok {
			return nil, ErrFrameMalformed
		}
		kind, ok := take(1)
		if !ok {
			return nil, ErrFrameMalformed
		}
		h := FrameHeader{Name: string(name), Kind: HeaderKind(kind[0])}
		switch h.Kind {
		case HeaderTrue:
			h.Bool = true
		case HeaderFalse:
		case HeaderByte:
			v, ok := take(1)
			if !ok {
				return nil, ErrFrameMalformed
			}
			h.Int = int64(int8(v[0]))
		case HeaderShort:
			v, ok := take(2)
			if !ok {
				return nil, ErrFrameMalformed
			}
			h.Int = int64(int16(binary.BigEndian.Uint16(v)))
		case HeaderInt:
			v, ok := take(4)
			if !ok {
				return nil, ErrFrameMalformed
			}
			h.Int = int64(int32(binary.BigEndian.Uint32(v)))
		case HeaderLong, HeaderTimestamp:
			v, ok := take(8)
			if !ok {
				return nil, ErrFrameMalformed
			}
			h.Int = int64(binary.BigEndian.Uint64(v))
			if h.Kind == HeaderTimestamp {
				h.Time = time.UnixMilli(h.Int).UTC()
			}
		case HeaderBytes, HeaderString:
			size, ok := take(2)
			if !ok {
				return nil, ErrFrameMalformed
			}
			v, ok := take(int(binary.BigEndian.Uint16(size)))
			if !ok {
				return nil, ErrFrameMalformed
			}
			if h.Kind == HeaderString {
				h.Text = string(v)
			} else {
				h.Bytes = append([]byte(nil), v...)
			}
		case HeaderUUID:
			v, ok := take(16)
			if !ok {
				return nil, ErrFrameMalformed
			}
			h.Bytes = append([]byte(nil), v...)
		default:
			return nil, ErrFrameMalformed
		}
		out = append(out, h)
	}
	return out, nil
}
