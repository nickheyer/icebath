package bedrockconversestream

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	stamp := time.UnixMilli(1700000000123).UTC()
	in := Frame{Headers: map[string]any{
		":message-type": "event",
		":event-type":   "contentBlockDelta",
		":content-type": "application/json",
		"flag":          true,
		"off":           false,
		"i8":            int8(-3),
		"i16":           int16(-300),
		"i32":           int32(-70000),
		"i64":           int64(-1 << 40),
		"raw":           []byte{1, 2, 3},
		"when":          stamp,
		"id":            [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	}, Payload: []byte(`{"contentBlockIndex":0,"delta":{"text":"hi"}}`)}
	enc, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := Frame{}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var frames []Frame
	for f, err := range ReadFrames(bytes.NewReader(append(enc, empty...))) {
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames", len(frames))
	}
	got := frames[0]
	if string(got.Payload) != string(in.Payload) || got.String(":event-type") != "contentBlockDelta" {
		t.Fatalf("%+v", got)
	}
	for k, v := range in.Headers {
		switch want := v.(type) {
		case []byte:
			if !bytes.Equal(got.Headers[k].([]byte), want) {
				t.Fatalf("header %s", k)
			}
		case time.Time:
			if !got.Headers[k].(time.Time).Equal(want) {
				t.Fatalf("header %s", k)
			}
		default:
			if got.Headers[k] != want {
				t.Fatalf("header %s = %v want %v", k, got.Headers[k], want)
			}
		}
	}
	if len(frames[1].Headers) != 0 || len(frames[1].Payload) != 0 {
		t.Fatalf("%+v", frames[1])
	}
	if got.String("flag") != "" || got.String("missing") != "" {
		t.Fatal("String must only return string headers")
	}
}

func TestFrameLayoutMatchesSpec(t *testing.T) {
	enc, err := Frame{Headers: map[string]any{"a": "b"}, Payload: []byte("xy")}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint32(enc[0:4]) != uint32(len(enc)) {
		t.Fatal("total length")
	}
	if binary.BigEndian.Uint32(enc[4:8]) != 6 {
		t.Fatal("headers length")
	}
	if want := []byte{1, 'a', valueString, 0, 1, 'b', 'x', 'y'}; !bytes.Equal(enc[12:len(enc)-4], want) {
		t.Fatalf("body %v", enc[12:len(enc)-4])
	}
}

func TestReadFramesRejectsCorruption(t *testing.T) {
	enc, err := Frame{Headers: map[string]any{":event-type": "messageStop"}, Payload: []byte(`{}`)}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"payload bit flip": flip(enc, len(enc)-6),
		"prelude bit flip": flip(enc, 5),
		"truncated":        enc[:len(enc)-3],
		"short prelude":    enc[:7],
	}
	for name, data := range cases {
		var failed bool
		for _, err := range ReadFrames(bytes.NewReader(data)) {
			if err != nil {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestEncodeRejectsBadHeaders(t *testing.T) {
	if _, err := (Frame{Headers: map[string]any{"x": 3.5}}).Encode(); err == nil {
		t.Fatal("float accepted")
	}
	if _, err := (Frame{Headers: map[string]any{"x": make([]byte, maxHeaderValue+1)}}).Encode(); err == nil {
		t.Fatal("oversized value accepted")
	}
}

func flip(b []byte, i int) []byte {
	out := append([]byte(nil), b...)
	out[i] ^= 0x01
	return out
}

func TestReadFramesRejectsOversizedLengths(t *testing.T) {
	cases := map[string]struct {
		total, headers uint32
	}{
		"frame too long":   {total: maxFrameLen + 1, headers: 0},
		"headers too long": {total: maxFrameLen, headers: maxHeadersLen + 1},
	}
	for name, c := range cases {
		var prelude [preludeLen]byte
		binary.BigEndian.PutUint32(prelude[0:4], c.total)
		binary.BigEndian.PutUint32(prelude[4:8], c.headers)
		binary.BigEndian.PutUint32(prelude[8:12], crc32.Checksum(prelude[:8], crcTable))
		var got error
		for _, err := range ReadFrames(bytes.NewReader(prelude[:])) {
			got = err
		}
		if got == nil || !strings.Contains(got.Error(), "limit") {
			t.Fatalf("%s: %v", name, got)
		}
	}
}
