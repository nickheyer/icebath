package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testDecode(text string) (any, error) { return decodeJSON([]byte(text)) }

func TestEncodeMessageUsesCompactSeparatorsAndUTF8(t *testing.T) {
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "x", "params": map[string]any{"k": "ä"}}
	out, err := EncodeMessage(msg)
	must(t, err)
	end := bytes.Index(out, []byte("\r\n\r\n")) + 4
	header, body := string(out[:end]), out[end:]
	if !strings.Contains(header, "Content-Length:") {
		t.Fatalf("header %q", header)
	}
	declared, err := strconv.Atoi(strings.TrimSpace(strings.Split(strings.Split(header, "Content-Length:")[1], "\r\n")[0]))
	must(t, err)
	if declared != len(body) {
		t.Fatalf("declared %d, body %d", declared, len(body))
	}
	var parsed map[string]any
	must(t, json.Unmarshal(body, &parsed))
	want := map[string]any{"jsonrpc": "2.0", "id": float64(1), "method": "x", "params": map[string]any{"k": "ä"}}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("round trip %v", parsed)
	}
	if !bytes.Contains(body, []byte(`"id":1`)) || !bytes.Contains(body, []byte("ä")) {
		t.Fatalf("body %s", body)
	}
}

func TestReadMessageRejectsRunawayHeader(t *testing.T) {
	flood := strings.Repeat("X-Junk: "+strings.Repeat("A", 200)+"\r\n", 60)
	_, _, err := ReadMessage(bufio.NewReader(strings.NewReader(flood)), testDecode)
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("want protocol error, got %v", err)
	}
}

func TestReadMessageFraming(t *testing.T) {
	frame := func(body string) string { return "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body }
	r := bufio.NewReader(strings.NewReader(frame(`{"jsonrpc":"2.0","id":3,"result":{"a":1.5}}`) + frame(`{"jsonrpc":"2.0","method":"m"}`)))
	msg, ok, err := ReadMessage(r, testDecode)
	if err != nil || !ok {
		t.Fatalf("first message %v %v", ok, err)
	}
	m := msg.(map[string]any)
	if m["id"] != int64(3) || m["result"].(map[string]any)["a"] != 1.5 {
		t.Fatalf("numbers decode like json.loads: %#v", m)
	}
	if _, ok, err := ReadMessage(r, testDecode); err != nil || !ok {
		t.Fatalf("second message %v %v", ok, err)
	}
	if _, ok, err := ReadMessage(r, testDecode); err != nil || ok {
		t.Fatalf("clean EOF between messages gives no message and no error, got %v %v", ok, err)
	}
	cases := map[string]string{
		"Content-Type: x\r\n\r\n":             "LSP message missing Content-Length: {'content-type': 'x'}",
		"Content-Length: abc\r\n\r\n":         "non-integer Content-Length: 'abc'",
		"Content-Length: 99999999999\r\n\r\n": "unreasonable Content-Length: 99999999999",
		"Content-Length: 10\r\n\r\n{}":        "truncated LSP body: expected 10 bytes, got 2",
		"Content-Length: 2\r\n":               "unexpected EOF while reading LSP headers (partial=b'')",
		"Content-Le":                          "unexpected EOF while reading LSP headers (partial=b'Content-Le')",
		": x\r\n\r\n":                         "malformed LSP header line: b': x'",
		"Content-Length: 1\r\n\r\n\xff":       "non-UTF-8 LSP body: 'utf-8' codec can't decode byte 0xff in position 0: invalid start byte",
	}
	for input, want := range cases {
		_, _, err := ReadMessage(bufio.NewReader(strings.NewReader(input)), testDecode)
		var pe *ProtocolError
		if !errors.As(err, &pe) || err.Error() != want {
			t.Errorf("%q: got %v, want %q", input, err, want)
		}
	}
	if _, _, err := ReadMessage(bufio.NewReader(strings.NewReader("Content-Length: 1_0\r\n\r\n1234567890")), testDecode); err != nil {
		t.Fatalf("int() accepts digit underscores: %v", err)
	}
}

func TestMakeNotificationOmitsID(t *testing.T) {
	msg := MakeNotification("log", map[string]any{"line": "hi"})
	if _, ok := msg["id"]; ok || msg["method"] != "log" {
		t.Fatalf("%v", msg)
	}
	if _, ok := MakeNotification("exit", nil)["params"]; ok {
		t.Fatal("nil params must be omitted")
	}
}

func TestClassifyMessage(t *testing.T) {
	if k, _ := ClassifyMessage(map[string]any{"id": 1}); k != KindInvalid {
		t.Fatal(k)
	}
	if k, _ := ClassifyMessage(map[string]any{"jsonrpc": "1.0", "method": "x"}); k != KindInvalid {
		t.Fatal(k)
	}
	if k, key := ClassifyMessage(map[string]any{"jsonrpc": "2.0", "id": int64(2), "method": "x"}); k != KindRequest || key != int64(2) {
		t.Fatal(k, key)
	}
	if k, _ := ClassifyMessage(map[string]any{"jsonrpc": "2.0", "id": int64(2), "error": nil}); k != KindResponse {
		t.Fatal(k)
	}
	if k, key := ClassifyMessage(map[string]any{"jsonrpc": "2.0", "method": "n"}); k != KindNotification || key != "n" {
		t.Fatal(k, key)
	}
}

func TestRequestErrorCarriesCodeAndDetails(t *testing.T) {
	e := NewRequestError(-32602, "bad init", nil)
	attachDetails(e, "server exited with code 0")
	if e.Code != -32602 || e.Error() != "LSP error -32602: bad init (server exited with code 0)" {
		t.Fatal(e.Error())
	}
}
