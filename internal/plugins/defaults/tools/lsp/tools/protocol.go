package tools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Error codes the client acts on
const (
	contentModified = -32801
	methodNotFound  = -32601
)

// Bounds of one framed message
const (
	maxHeaderBytes = 8 << 10
	maxBodyBytes   = 64 << 20
	streamLimit    = 16 << 20
)

// Broken framing or a broken envelope, rather than an error response
type protocolError struct{ text string }

func (e *protocolError) Error() string { return e.text }

func protocolErrorf(format string, args ...any) error {
	return &protocolError{text: fmt.Sprintf(format, args...)}
}

// Error response of a request
type requestError struct {
	code    int
	message string
}

func (e *requestError) Error() string { return fmt.Sprintf("LSP error %d: %s", e.code, e.message) }

// Wait that ran out of its budget
type timeoutError struct{ text string }

func (e *timeoutError) Error() string { return e.text }

// Whether the error is the server's or the wait's, rather than the caller giving up
func lspFailure(err error) bool {
	_, protocol := errors.AsType[*protocolError](err)
	_, request := errors.AsType[*requestError](err)
	_, timeout := errors.AsType[*timeoutError](err)
	return protocol || request || timeout
}

// Code of an error response, zero for any other error
func errorCode(err error) int {
	if re, ok := errors.AsType[*requestError](err); ok {
		return re.code
	}
	return 0
}

// One message as it travels, its parts kept raw until the kind is known
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  *string         `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    *json.Number `json:"code"`
		Message *string      `json:"message"`
	} `json:"error,omitempty"`
}

// Kinds of message a server sends
const (
	invalid = iota
	request
	response
	notification
)

func (e *envelope) kind() int {
	if e.JSONRPC != "2.0" {
		return invalid
	}
	if e.ID != nil {
		switch {
		case e.Method != nil:
			return request
		case e.Result != nil || e.Error != nil:
			return response
		}
		return invalid
	}
	if e.Method != nil {
		return notification
	}
	return invalid
}

// Id of a response as the pending requests are keyed, false for one no request of ours has
func (e *envelope) id() (int64, bool) {
	var n json.Number
	if json.Unmarshal(e.ID, &n) != nil {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	if f, err := n.Float64(); err == nil && f == float64(int64(f)) {
		return int64(f), true
	}
	return 0, false
}

// Message framed as UTF-8 JSON behind its exact Content-Length
func encode(message map[string]any) ([]byte, error) {
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return append(fmt.Appendf(nil, "Content-Length: %d\r\n\r\n", len(body)), body...), nil
}

func requestMessage(id int64, method string, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

func notificationMessage(method string, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

func resultMessage(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func errorMessage(id json.RawMessage, code int, text string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": text}}
}

// Bytes up to and including the next CRLF, the bytes read with io.EOF when the stream ends first
func crlfLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if err == nil && bytes.HasSuffix(line, []byte("\r\n")) {
			return line, nil
		}
		if len(line) > streamLimit {
			return line, protocolErrorf("LSP header line exceeded the stream limit")
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// Header fields of the next message by lower case name, false on a clean end of the stream between messages
func headers(r *bufio.Reader) (map[string]string, bool, error) {
	fields := map[string]string{}
	size := 0
	for {
		line, err := crlfLine(r)
		if errors.Is(err, io.EOF) {
			if len(line) == 0 && len(fields) == 0 {
				return nil, false, nil
			}
			return nil, false, protocolErrorf("unexpected EOF while reading LSP headers (partial=%q)", line)
		}
		if err != nil {
			return nil, false, err
		}
		if size += len(line); size > maxHeaderBytes {
			return nil, false, protocolErrorf("LSP header block exceeded 8 KiB without terminator")
		}
		line = line[:len(line)-2]
		if len(line) == 0 {
			return fields, true, nil
		}
		for _, c := range line {
			if c >= utf8.RuneSelf {
				return nil, false, protocolErrorf("non-ASCII LSP header: %q", line)
			}
		}
		key, value, _ := strings.Cut(string(line), ":")
		if key == "" {
			return nil, false, protocolErrorf("malformed LSP header line: %q", line)
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
}

// Next framed message, false on a clean end of the stream between messages; a message that is JSON but no object comes back nil
func readMessage(r *bufio.Reader) (*envelope, bool, error) {
	fields, ok, err := headers(r)
	if err != nil || !ok {
		return nil, false, err
	}
	length, ok := fields["content-length"]
	if !ok {
		return nil, false, protocolErrorf("LSP message missing Content-Length: %v", fields)
	}
	n, err := strconv.ParseInt(length, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return nil, false, protocolErrorf("unreasonable Content-Length: %s", length)
	case err != nil:
		return nil, false, protocolErrorf("non-integer Content-Length: %q", length)
	case n < 0 || n > maxBodyBytes:
		return nil, false, protocolErrorf("unreasonable Content-Length: %d", n)
	}
	body := make([]byte, n)
	if got, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, false, protocolErrorf("truncated LSP body: expected %d bytes, got %d", n, got)
		}
		return nil, false, err
	}
	if !utf8.Valid(body) {
		return nil, false, protocolErrorf("non-UTF-8 LSP body")
	}
	var e envelope
	if err := json.Unmarshal(body, &e); err != nil {
		if syntax, ok := errors.AsType[*json.SyntaxError](err); ok {
			return nil, false, protocolErrorf("invalid JSON in LSP body: %s", syntax)
		}
		return nil, true, nil
	}
	return &e, true, nil
}

// Position in a document, its character counted in UTF-16 units
type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type span struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

// Text of a field a server sends as a string, a number, or markup with a value
type text string

func (t *text) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		*t = text(s)
		return nil
	}
	var markup struct {
		Value *string `json:"value"`
	}
	if json.Unmarshal(data, &markup) == nil && markup.Value != nil {
		*t = text(*markup.Value)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		if string(bytes.TrimSpace(data)) == "null" {
			return nil
		}
		return err
	}
	*t = text(n.String())
	return nil
}

// One problem a server reports
type diagnostic struct {
	Range span `json:"range"`
	// 1 error, 2 warning, 3 information, 4 hint; none counting as an error
	Severity int  `json:"severity,omitempty"`
	Code     text `json:"code,omitempty"`
	Source   text `json:"source,omitempty"`
	Message  text `json:"message"`
}

func (d diagnostic) severity() int {
	if d.Severity == 0 {
		return 1
	}
	return d.Severity
}

// Equality of two diagnostics by their content: severity, code, source, message and range
func (d diagnostic) key() string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%d:%d-%d:%d", d.severity(), d.Code, d.Source, strings.TrimSpace(string(d.Message)),
		d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Line, d.Range.End.Character)
}

// Diagnostics of a list a server sent, entries that are no diagnostic left out
func diagnostics(raw []json.RawMessage) []diagnostic {
	out := []diagnostic{}
	for _, item := range raw {
		var d diagnostic
		if json.Unmarshal(item, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// Union of diagnostic lists, each content once
func dedupe(lists ...[]diagnostic) []diagnostic {
	seen := map[string]bool{}
	out := []diagnostic{}
	for _, list := range lists {
		for _, d := range list {
			if k := d.key(); !seen[k] {
				seen[k] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// Diagnostics not in the baseline
func without(diags, baseline []diagnostic) []diagnostic {
	seen := map[string]bool{}
	for _, d := range baseline {
		seen[d.key()] = true
	}
	out := []diagnostic{}
	for _, d := range diags {
		if !seen[d.key()] {
			out = append(out, d)
		}
	}
	return out
}

// File URI of a path, spaces, unicode and drive letters encoded
func fileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		abs = filepath.ToSlash(abs)
		if !strings.HasPrefix(abs, "/") {
			abs = "/" + abs
		}
	}
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(abs); i++ {
		c := abs[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("_.-~/:", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String(), nil
}

// Path of a file URI, anything else as it is
func uriPath(uri string) string {
	raw, ok := strings.CutPrefix(uri, "file://")
	if !ok {
		return uri
	}
	if unescaped, err := url.PathUnescape(raw); err == nil {
		raw = unescaped
	}
	if runtime.GOOS == "windows" && len(raw) > 2 && raw[0] == '/' && raw[2] == ':' {
		raw = raw[1:]
	}
	return filepath.Clean(filepath.FromSlash(raw))
}

// Text as a read of its file sees it: invalid UTF-8 replaced, the byte order mark dropped, line ends as \n
func asDiskRead(s string) string {
	s = strings.TrimPrefix(strings.ToValidUTF8(s, "\uFFFD"), "\uFEFF")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Lines of a text as a server counts them
func lines(s string) []string {
	s = asDiskRead(s)
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// Position at the end of a text, for a whole document replace range
func endOf(s string) position {
	last := s[strings.LastIndexByte(s, '\n')+1:]
	units := 0
	for _, r := range last {
		units += utf16Len(r)
	}
	return position{Line: strings.Count(s, "\n"), Character: units}
}

func utf16Len(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}
