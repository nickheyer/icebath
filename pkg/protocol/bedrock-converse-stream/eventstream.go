package bedrockconversestream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"iter"
	"time"
)

const (
	preludeLen = 12
	msgCRCLen  = 4
	minFrame   = preludeLen + msgCRCLen
	// Variable length header values carry a two byte length prefix
	maxHeaderValue = 1<<15 - 1
	maxFrameLen    = 16 << 20
	maxHeadersLen  = 128 << 10
)

const (
	valueTrue uint8 = iota
	valueFalse
	valueInt8
	valueInt16
	valueInt32
	valueInt64
	valueBytes
	valueString
	valueTimestamp
	valueUUID
)

var crcTable = crc32.MakeTable(crc32.IEEE)

// One message of the application/vnd.amazon.eventstream framing
type Frame struct {
	// Typed header values keyed by name, strings for every header Bedrock emits
	Headers map[string]any
	Payload []byte
}

// Returns the string header named name, or empty when absent or not a string
func (f Frame) String(name string) string {
	s, _ := f.Headers[name].(string)
	return s
}

// Yields frames from r until EOF, failing on any length or CRC mismatch
func ReadFrames(r io.Reader) iter.Seq2[Frame, error] {
	return func(yield func(Frame, error) bool) {
		for {
			f, err := readFrame(r)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(Frame{}, err)
				return
			}
			if !yield(f, nil) {
				return
			}
		}
	}
}

func readFrame(r io.Reader) (Frame, error) {
	var prelude [preludeLen]byte
	if _, err := io.ReadFull(r, prelude[:4]); err != nil {
		return Frame{}, err
	}
	if _, err := io.ReadFull(r, prelude[4:]); err != nil {
		return Frame{}, fmt.Errorf("eventstream: prelude: %w", io.ErrUnexpectedEOF)
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.Checksum(prelude[:8], crcTable) != binary.BigEndian.Uint32(prelude[8:12]) {
		return Frame{}, errors.New("eventstream: prelude checksum mismatch")
	}
	if total > maxFrameLen {
		return Frame{}, fmt.Errorf("eventstream: frame of %d bytes exceeds the %d byte limit", total, maxFrameLen)
	}
	if headersLen > maxHeadersLen {
		return Frame{}, fmt.Errorf("eventstream: headers of %d bytes exceed the %d byte limit", headersLen, maxHeadersLen)
	}
	if total < minFrame || headersLen > total-minFrame {
		return Frame{}, fmt.Errorf("eventstream: invalid lengths total=%d headers=%d", total, headersLen)
	}
	rest := make([]byte, total-preludeLen)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Frame{}, fmt.Errorf("eventstream: body: %w", io.ErrUnexpectedEOF)
	}
	body := rest[:len(rest)-msgCRCLen]
	want := binary.BigEndian.Uint32(rest[len(rest)-msgCRCLen:])
	sum := crc32.Update(crc32.Checksum(prelude[:], crcTable), crcTable, body)
	if sum != want {
		return Frame{}, errors.New("eventstream: message checksum mismatch")
	}
	headers, err := decodeHeaders(body[:headersLen])
	if err != nil {
		return Frame{}, err
	}
	return Frame{Headers: headers, Payload: body[headersLen:]}, nil
}

func decodeHeaders(b []byte) (map[string]any, error) {
	headers := map[string]any{}
	r := bytes.NewReader(b)
	for r.Len() > 0 {
		nameLen, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(r, name); err != nil {
			return nil, fmt.Errorf("eventstream: header name: %w", err)
		}
		typ, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("eventstream: header type: %w", err)
		}
		val, err := decodeValue(r, typ)
		if err != nil {
			return nil, fmt.Errorf("eventstream: header %q: %w", name, err)
		}
		headers[string(name)] = val
	}
	return headers, nil
}

func decodeValue(r *bytes.Reader, typ uint8) (any, error) {
	switch typ {
	case valueTrue:
		return true, nil
	case valueFalse:
		return false, nil
	case valueInt8:
		var v int8
		return v, binary.Read(r, binary.BigEndian, &v)
	case valueInt16:
		var v int16
		return v, binary.Read(r, binary.BigEndian, &v)
	case valueInt32:
		var v int32
		return v, binary.Read(r, binary.BigEndian, &v)
	case valueInt64:
		var v int64
		return v, binary.Read(r, binary.BigEndian, &v)
	case valueBytes, valueString:
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		v := make([]byte, n)
		if _, err := io.ReadFull(r, v); err != nil {
			return nil, err
		}
		if typ == valueString {
			return string(v), nil
		}
		return v, nil
	case valueTimestamp:
		var ms int64
		if err := binary.Read(r, binary.BigEndian, &ms); err != nil {
			return nil, err
		}
		return time.UnixMilli(ms).UTC(), nil
	case valueUUID:
		var v [16]byte
		_, err := io.ReadFull(r, v[:])
		return v, err
	}
	return nil, fmt.Errorf("unknown value type %d", typ)
}

// Serializes f with both checksums, used to build fixtures and request frames
func (f Frame) Encode() ([]byte, error) {
	var headers bytes.Buffer
	for name, val := range f.Headers {
		if len(name) > 255 {
			return nil, fmt.Errorf("eventstream: header name %q too long", name)
		}
		headers.WriteByte(uint8(len(name)))
		headers.WriteString(name)
		if err := encodeValue(&headers, val); err != nil {
			return nil, fmt.Errorf("eventstream: header %q: %w", name, err)
		}
	}
	total := uint32(minFrame + headers.Len() + len(f.Payload))
	var out bytes.Buffer
	var prelude [preludeLen]byte
	binary.BigEndian.PutUint32(prelude[0:4], total)
	binary.BigEndian.PutUint32(prelude[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.Checksum(prelude[:8], crcTable))
	out.Write(prelude[:])
	out.Write(headers.Bytes())
	out.Write(f.Payload)
	sum := crc32.Checksum(out.Bytes(), crcTable)
	var tail [msgCRCLen]byte
	binary.BigEndian.PutUint32(tail[:], sum)
	out.Write(tail[:])
	return out.Bytes(), nil
}

func encodeValue(w *bytes.Buffer, val any) error {
	switch v := val.(type) {
	case bool:
		if v {
			return w.WriteByte(valueTrue)
		}
		return w.WriteByte(valueFalse)
	case int8:
		w.WriteByte(valueInt8)
		return binary.Write(w, binary.BigEndian, v)
	case int16:
		w.WriteByte(valueInt16)
		return binary.Write(w, binary.BigEndian, v)
	case int32:
		w.WriteByte(valueInt32)
		return binary.Write(w, binary.BigEndian, v)
	case int64:
		w.WriteByte(valueInt64)
		return binary.Write(w, binary.BigEndian, v)
	case []byte:
		return encodeVariable(w, valueBytes, v)
	case string:
		return encodeVariable(w, valueString, []byte(v))
	case time.Time:
		w.WriteByte(valueTimestamp)
		return binary.Write(w, binary.BigEndian, v.UnixMilli())
	case [16]byte:
		w.WriteByte(valueUUID)
		_, err := w.Write(v[:])
		return err
	}
	return fmt.Errorf("unsupported header value %T", val)
}

func encodeVariable(w *bytes.Buffer, typ uint8, v []byte) error {
	if len(v) > maxHeaderValue {
		return fmt.Errorf("value of %d bytes exceeds %d", len(v), maxHeaderValue)
	}
	w.WriteByte(typ)
	if err := binary.Write(w, binary.BigEndian, uint16(len(v))); err != nil {
		return err
	}
	_, err := w.Write(v)
	return err
}
