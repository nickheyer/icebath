// Package lsp runs language servers over stdio and turns their diagnostics into post write lint verdicts
package lsp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// LSP error codes the client acts on
const (
	ErrorContentModified = -32801
	ErrorMethodNotFound  = -32601
)

const (
	maxHeaderBytes = 8192
	maxBodyBytes   = 64 * 1024 * 1024
	streamLimit    = 16 * 1024 * 1024
)

// ProtocolError reports broken framing or a broken envelope rather than a conformant error response
type ProtocolError struct{ msg string }

func (e *ProtocolError) Error() string { return e.msg }

func protocolErrorf(format string, args ...any) *ProtocolError {
	return &ProtocolError{msg: fmt.Sprintf(format, args...)}
}

// RequestError is a JSON-RPC error response carrying code, message and data
type RequestError struct {
	Code    int
	Message string
	Data    any
	text    string
}

// NewRequestError builds the error a JSON-RPC error response raises
func NewRequestError(code int, message string, data any) *RequestError {
	return &RequestError{Code: code, Message: message, Data: data, text: fmt.Sprintf("LSP error %d: %s", code, message)}
}

func (e *RequestError) Error() string { return e.text }

// JSONDecodeError is a json.loads decode failure carrying the Python message
type JSONDecodeError struct{ Msg string }

func (e *JSONDecodeError) Error() string { return e.Msg }

// PythonError builds the error a Python exception of a class and text stands for
func PythonError(class, text string) error { return &pyExc{typ: class, text: text} }

// TimeoutError is a wait that ran out of its budget
type TimeoutError struct{ msg string }

func (e *TimeoutError) Error() string { return e.msg }

// Appends failure details to an error's text in place, keeping its type
func attachDetails(err error, details string) {
	var pe *ProtocolError
	var re *RequestError
	var te *TimeoutError
	var oe *pyExc
	switch {
	case errors.As(err, &re):
		re.text = re.text + " (" + details + ")"
	case errors.As(err, &pe):
		pe.msg = pe.msg + " (" + details + ")"
	case errors.As(err, &te):
		te.msg = te.msg + " (" + details + ")"
	case errors.As(err, &oe):
		oe.text = oe.text + " (" + details + ")"
	}
}

// EncodeMessage frames an envelope as compact UTF-8 JSON behind an exact Content-Length header
func EncodeMessage(obj any) ([]byte, error) {
	body, err := pyjson.Dumps(orderedEnvelope(obj), pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+32)
	out = append(out, fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))...)
	return append(out, body...), nil
}

// Envelope and error object with their keys in the order the message builders write them
func orderedEnvelope(obj any) any {
	envelope, ok := obj.(map[string]any)
	if !ok {
		return obj
	}
	if errObj, isMap := envelope["error"].(map[string]any); isMap {
		copied := make(map[string]any, len(envelope))
		for k, v := range envelope {
			copied[k] = v
		}
		copied["error"] = keysFirst(errObj, "code", "message", "data")
		envelope = copied
	}
	return keysFirst(envelope, "jsonrpc", "id", "method", "params", "result", "error")
}

// Ordered object with the named keys first and every other key after them in sorted order
func keysFirst(m map[string]any, first ...string) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range first {
		if v, has := m[k]; has {
			out.Set(k, v)
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		if _, done := out.Get(k); !done {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out.Set(k, m[k])
	}
	return out
}

// One header field in first arrival order
type headerField struct{ key, value string }

// Header block in order, nil on a clean EOF before any header started
func readHeaders(r *bufio.Reader) ([]headerField, bool, error) {
	var headers []headerField
	headerBytes := 0
	for {
		line, err := readCRLFLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) == 0 && len(headers) == 0 {
					return nil, false, nil
				}
				return nil, false, protocolErrorf("unexpected EOF while reading LSP headers (partial=%s)", pyrepr.BytesRepr(line))
			}
			return nil, false, err
		}
		headerBytes += len(line)
		if headerBytes > maxHeaderBytes {
			return nil, false, protocolErrorf("LSP header block exceeded 8 KiB without terminator")
		}
		line = line[:len(line)-2]
		if len(line) == 0 {
			return headers, true, nil
		}
		for _, c := range line {
			if c >= 0x80 {
				return nil, false, protocolErrorf("non-ASCII LSP header: %s", pyrepr.BytesRepr(line))
			}
		}
		key, value, _ := strings.Cut(string(line), ":")
		if key == "" {
			return nil, false, protocolErrorf("malformed LSP header line: %s", pyrepr.BytesRepr(line))
		}
		name, repeated := pystr.Lower(pystr.Strip(key)), false
		for i := range headers {
			if headers[i].key == name {
				headers[i].value, repeated = pystr.Strip(value), true
				break
			}
		}
		if !repeated {
			headers = append(headers, headerField{name, pystr.Strip(value)})
		}
	}
}

// Bytes up to and including the next CRLF, the partial bytes with io.EOF when the stream ends first
func readCRLFLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if err == nil && bytes.HasSuffix(line, []byte("\r\n")) {
			return line, nil
		}
		if len(line) > streamLimit {
			return line, errors.New("Separator is not found, and chunk exceed the limit")
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// ReadMessage reads one framed message decoded by decode, ok false on a clean EOF between messages
func ReadMessage(r *bufio.Reader, decode func(text string) (any, error)) (msg any, ok bool, err error) {
	headers, ok, err := readHeaders(r)
	if err != nil || !ok {
		return nil, false, err
	}
	var cl *string
	for i := range headers {
		if headers[i].key == "content-length" {
			cl = &headers[i].value
		}
	}
	if cl == nil {
		return nil, false, protocolErrorf("LSP message missing Content-Length: %s", headersRepr(headers))
	}
	n, valid := pyint.Parse(*cl)
	if !valid {
		return nil, false, protocolErrorf("non-integer Content-Length: %s", pyrepr.StrRepr(*cl))
	}
	if n.Sign() < 0 || n.Cmp(big.NewInt(maxBodyBytes)) > 0 {
		return nil, false, protocolErrorf("unreasonable Content-Length: %s", n.String())
	}
	size := int(n.Int64())
	body := make([]byte, size)
	got, err := io.ReadFull(r, body)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, false, protocolErrorf("truncated LSP body: expected %d bytes, got %d", size, got)
		}
		return nil, false, err
	}
	if !utf8.Valid(body) {
		return nil, false, protocolErrorf("non-UTF-8 LSP body: %s", utf8DecodeErrorText(body))
	}
	v, err := decode(string(body))
	if err != nil {
		var jde *JSONDecodeError
		if errors.As(err, &jde) {
			return nil, false, protocolErrorf("invalid JSON in LSP body: %s", jde.Msg)
		}
		return nil, false, err
	}
	return v, true, nil
}

// Python dict repr of a header block
func headersRepr(headers []headerField) string {
	parts := make([]string, len(headers))
	for i, h := range headers {
		parts[i] = pyrepr.StrRepr(h.key) + ": " + pyrepr.StrRepr(h.value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// MakeNotification builds a JSON-RPC notification envelope without an id
func MakeNotification(method string, params any) map[string]any {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return msg
}

// MakeRequest builds a JSON-RPC request envelope
func MakeRequest(id int64, method string, params any) map[string]any {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	return msg
}

// MakeResponse builds a JSON-RPC success response envelope
func MakeResponse(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// MakeErrorResponse builds a JSON-RPC error response envelope
func MakeErrorResponse(id any, code int, message string, data any) map[string]any {
	errObj := map[string]any{"code": code, "message": message}
	if data != nil {
		errObj["data"] = data
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": errObj}
}

// Message kinds ClassifyMessage reports
const (
	KindRequest      = "request"
	KindResponse     = "response"
	KindNotification = "notification"
	KindInvalid      = "invalid"
)

// ClassifyMessage names a message kind with its id for requests and responses or its method for notifications
func ClassifyMessage(msg any) (string, any) {
	m, ok := msg.(map[string]any)
	if !ok {
		return KindInvalid, nil
	}
	if v, ok := m["jsonrpc"].(string); !ok || v != "2.0" {
		return KindInvalid, nil
	}
	if id, hasID := m["id"]; hasID {
		if _, isReq := m["method"]; isReq {
			return KindRequest, id
		}
		_, hasResult := m["result"]
		_, hasError := m["error"]
		if hasResult || hasError {
			return KindResponse, id
		}
		return KindInvalid, nil
	}
	if method, ok := m["method"]; ok {
		return KindNotification, method
	}
	return KindInvalid, nil
}
