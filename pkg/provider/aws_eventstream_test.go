package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// Reader handing out the stream in the given chunk sizes, cycling through them
type chunkedReader struct {
	data  []byte
	sizes []int
	i     int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.sizes[r.i%len(r.sizes)], len(r.data), len(p))
	r.i++
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestEventStreamRoundTripEveryHeaderType(t *testing.T) {
	ts := time.UnixMilli(1700000000123).UTC()
	uuid := []byte("0123456789abcdef")
	headers := []awsEventStreamHeader{
		{Name: "t", Type: awsHeaderTrue, Value: true}, {Name: "f", Type: awsHeaderFalse, Value: false},
		{Name: "b", Type: awsHeaderByte, Value: int8(-7)}, {Name: "s", Type: awsHeaderShort, Value: int16(-300)},
		{Name: "i", Type: awsHeaderInteger, Value: int32(-70000)}, {Name: "l", Type: awsHeaderLong, Value: int64(1) << 40},
		{Name: "bytes", Type: awsHeaderBytes, Value: []byte{0, 1, 2}}, {Name: ":event-type", Type: awsHeaderString, Value: "chunk"},
		{Name: "ts", Type: awsHeaderTimestamp, Value: ts}, {Name: "id", Type: awsHeaderUUID, Value: uuid},
	}
	frame, err := awsEncodeEventStreamMessage(headers, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := newAWSEventStreamReader(bytes.NewReader(frame)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Headers, headers) || string(got.Payload) != `{"x":1}` {
		t.Fatalf("round trip mismatch: %#v %q", got.Headers, got.Payload)
	}
	if got.header(":event-type") != "chunk" || got.header("missing") != "" {
		t.Fatalf("header lookup: %q", got.header(":event-type"))
	}
}

func TestEventStreamSplitChunksAndCleanEOF(t *testing.T) {
	var stream []byte
	for _, text := range []string{"a", "bb", "ccc"} {
		f, err := bedrockInvokeChunkFrame(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		if err != nil {
			t.Fatal(err)
		}
		stream = append(stream, f...)
	}
	for _, r := range []io.Reader{iotest.OneByteReader(bytes.NewReader(stream)), &chunkedReader{data: append([]byte(nil), stream...), sizes: []int{5, 13, 1, 40, 7}}, iotest.HalfReader(bytes.NewReader(stream))} {
		src := newBedrockInvokeEvents(r, http.Header{})
		var texts []string
		for {
			ev, err := src.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			texts = append(texts, ev.Payload["delta"].(map[string]any)["text"].(string))
		}
		if strings.Join(texts, ",") != "a,bb,ccc" {
			t.Fatalf("texts across split chunks: %v", texts)
		}
	}
}

func TestEventStreamChecksumFailures(t *testing.T) {
	frame, _ := awsEncodeEventStreamMessage(awsEventHeaders("chunk", "application/json"), []byte(`{"bytes":""}`))
	for name, mutate := range map[string]func([]byte){
		"prelude": func(b []byte) { b[9] ^= 0xff },
		"message": func(b []byte) { b[len(b)-1] ^= 0xff },
		"payload": func(b []byte) { b[len(b)-6] ^= 0x01 },
	} {
		bad := append([]byte(nil), frame...)
		mutate(bad)
		_, err := newAWSEventStreamReader(bytes.NewReader(bad)).Next()
		var fe *AWSEventStreamFrameError
		if !errors.As(err, &fe) || fe.Kind != "ChecksumMismatch" {
			t.Fatalf("%s corruption: %v", name, err)
		}
	}
}

func TestEventStreamTruncatedAndOversizedFrames(t *testing.T) {
	frame, _ := awsEncodeEventStreamMessage(awsEventHeaders("chunk", "application/json"), []byte(`{"bytes":""}`))
	for _, cut := range []int{5, 20, len(frame) - 1} {
		_, err := newAWSEventStreamReader(bytes.NewReader(frame[:cut])).Next()
		var fe *AWSEventStreamFrameError
		if !errors.As(err, &fe) || fe.Kind != "TruncatedFrame" {
			t.Fatalf("cut at %d: %v", cut, err)
		}
	}
	if _, err := newAWSEventStreamReader(bytes.NewReader(nil)).Next(); err != io.EOF {
		t.Fatalf("empty stream: %v", err)
	}
	h := awsEncodePreludeForTest(16, 128*1024+1)
	var fe *AWSEventStreamFrameError
	if _, err := newAWSEventStreamReader(bytes.NewReader(h)).Next(); !errors.As(err, &fe) || fe.Kind != "InvalidHeadersLength" {
		t.Fatalf("oversized headers: %v", err)
	}
	h = awsEncodePreludeForTest(24*1024*1024+17, 0)
	if _, err := newAWSEventStreamReader(bytes.NewReader(h)).Next(); !errors.As(err, &fe) || fe.Kind != "InvalidPayloadLength" {
		t.Fatalf("oversized payload: %v", err)
	}
}

// Prelude with a valid checksum for the given lengths
func awsEncodePreludeForTest(total, headers uint32) []byte {
	p := binary.BigEndian.AppendUint32(nil, total)
	p = binary.BigEndian.AppendUint32(p, headers)
	return binary.BigEndian.AppendUint32(p, crc32.ChecksumIEEE(p))
}

func TestEventStreamExceptionFramesBecomeTypedErrors(t *testing.T) {
	start, _ := bedrockInvokeChunkFrame(map[string]any{"type": "message_start", "message": map[string]any{"id": "m"}})
	cases := []struct {
		exc    string
		status int
	}{{"throttlingException", 429}, {"modelStreamErrorException", 424}, {"internalServerException", 500},
		{"serviceUnavailableException", 503}, {"validationException", 400}, {"modelTimeoutException", 408}, {"somethingNew", 400}}
	for _, c := range cases {
		exc, err := awsEventStreamExceptionFrame(c.exc, "slow down")
		if err != nil {
			t.Fatal(err)
		}
		src := newBedrockInvokeEvents(bytes.NewReader(append(append([]byte(nil), start...), exc...)), http.Header{"X-Amzn-Requestid": {"r1"}})
		if ev, err := src.Next(); err != nil || ev.Type != "message_start" {
			t.Fatalf("%s first event: %v %v", c.exc, ev, err)
		}
		_, err = src.Next()
		var pse *ProviderStreamError
		if !errors.As(err, &pse) || pse.StatusCode != c.status {
			t.Fatalf("%s: %v", c.exc, err)
		}
		eo := pse.Body["error"].(map[string]any)
		if eo["type"] != c.exc || eo["message"] != "slow down" || pse.Headers.Get("x-amzn-requestid") != "r1" {
			t.Fatalf("%s body: %#v", c.exc, pse.Body)
		}
	}
	errFrame, _ := awsEncodeEventStreamMessage([]awsEventStreamHeader{
		{Name: ":message-type", Type: awsHeaderString, Value: "error"},
		{Name: ":error-code", Type: awsHeaderString, Value: "InternalFailure"},
		{Name: ":error-message", Type: awsHeaderString, Value: "boom"},
	}, nil)
	m, _ := newAWSEventStreamReader(bytes.NewReader(errFrame)).Next()
	exc := awsEventStreamFrameException(m)
	if exc == nil || exc.MessageType != "error" || exc.ExceptionType != "InternalFailure" || exc.Message != "boom" {
		t.Fatalf("error frame: %#v", exc)
	}
	if !strings.Contains(exc.Error(), "InternalFailure") {
		t.Fatalf("error text: %s", exc.Error())
	}
}

func TestBedrockInvokeEventsSkipPingAndNonChunkFrames(t *testing.T) {
	ping, _ := bedrockInvokeChunkFrame(map[string]any{"type": "ping"})
	other, _ := awsEncodeEventStreamMessage(awsEventHeaders("initial-response", "application/json"), []byte(`{}`))
	stop, _ := bedrockInvokeChunkFrame(map[string]any{"type": "message_stop", "amazon-bedrock-invocationMetrics": map[string]any{"inputTokenCount": 3}})
	src := newBedrockInvokeEvents(bytes.NewReader(bytes.Join([][]byte{ping, other, stop}, nil)), nil)
	ev, err := src.Next()
	if err != nil || ev.Type != "message_stop" || !strings.Contains(ev.Raw, "invocationMetrics") {
		t.Fatalf("event: %#v %v", ev, err)
	}
	if _, err := src.Next(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	notB64, _ := awsEncodeEventStreamMessage(awsEventHeaders("chunk", "application/json"), []byte(`{"bytes":"%%%"}`))
	if _, err := newBedrockInvokeEvents(bytes.NewReader(notB64), nil).Next(); err == nil {
		t.Fatal("non base64 chunk accepted")
	}
	notJSON, _ := awsEncodeEventStreamMessage(awsEventHeaders("chunk", "application/json"), []byte(`{"bytes":"`+base64.StdEncoding.EncodeToString([]byte("nope"))+`"}`))
	var pse *ProviderStreamError
	if _, err := newBedrockInvokeEvents(bytes.NewReader(notJSON), nil).Next(); !errors.As(err, &pse) {
		t.Fatalf("non JSON chunk: %v", err)
	}
}
