package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/httpclient"
)

// Codes of stream frames a decoder could not read
const (
	StreamNonJSON    = "provider_stream_non_json_data"
	StreamEmptyFrame = "provider_stream_empty_frame"
)

// Returned when a provider closes a stream without yielding a response
var ErrEmptyStream = errors.New("provider closed the stream without a response")

// Returned with the partial response when a stream ends cleanly before the provider finished the turn
var ErrStreamDropped = errors.New("provider ended the stream before finishing the turn")

// Provider-reported failure whose code, message and body hold provider text only in masked form
type APIError struct {
	Status   int
	Header   http.Header
	InStream bool

	code    string
	message string
	body    json.RawMessage
}

// Structured code or type the provider named, credentials masked
func (e *APIError) Code() string { return e.code }

// Text the provider gave, credentials masked
func (e *APIError) Message() string { return e.message }

// Error payload as JSON, every string in it with credentials masked
func (e *APIError) Body() json.RawMessage { return e.body }

func (e *APIError) Error() string {
	text, code, message := "Provider returned an error", e.code, e.message
	if e.InStream {
		text, code, message = "Provider stream returned an error event", "", ""
		if obj, ok := objectMember(e.body, "error"); ok {
			code, message = truthyText(obj, "code"), truthyText(obj, "message")
		}
	}
	if e.Status != 0 {
		text += " - HTTP " + strconv.Itoa(e.Status)
	}
	if code != "" {
		text += " - " + code
	}
	if message != "" {
		text += ": " + message
	}
	return text
}

// Wait the reply's Retry-After header asks for, false when absent or unreadable
func (e *APIError) RetryAfter(now time.Time) (time.Duration, bool) {
	return httpclient.RetryAfter(e.Header, now)
}

// Stream frame a decoder could not read, its text kept masked and bounded
type StreamError struct {
	Code string

	data string
}

// Failure for an unreadable stream frame, the frame text masked and cut to 4096 characters
func NewStreamError(code, frame string) *StreamError {
	return &StreamError{Code: code, data: maskFrame(trimSpace(frame))}
}

// Frame text with credentials masked
func (e *StreamError) Data() string { return e.data }

func (e *StreamError) Error() string {
	text := "Provider stream returned an error event - " + e.Code + ": "
	switch {
	case e.Code == StreamEmptyFrame:
		return text + "Provider stream returned an empty SSE data frame (keepalive with no payload)."
	case e.data == "":
		return text + "Provider stream returned non-JSON SSE data."
	}
	return text + e.data
}

// Failure for a 4xx or 5xx reply, code and message from the error envelope, else the body text
func NewAPIError(status int, header http.Header, body []byte) *APIError {
	text := httpclient.Text(header, body)
	payload := trimJSON([]byte(text))
	code, message := "", ""
	switch {
	case len(payload) == 0 || !json.Valid(payload):
	case payload[0] == '{':
		code, message = envelopeFields(payload)
	default:
		message, _ = stringValue(payload)
	}
	return NewAPIErrorDetail(status, header, code, message, []byte(text))
}

// Failure with the code and message a protocol read, an empty message falling back to the body text
func NewAPIErrorDetail(status int, header http.Header, code, message string, body []byte) *APIError {
	e := &APIError{Status: status, Header: header, code: MaskCredentials(code)}
	payload := trimJSON(body)
	switch {
	case len(payload) == 0:
	case json.Valid(payload):
		e.body = maskJSON(bytes.Clone(payload))
	default:
		e.body = appendString(nil, MaskCredentials(string(body)), 0)
	}
	if message == "" {
		message = trimSpace(string(body))
	}
	e.message = MaskCredentials(message)
	return e
}

// Code and message of an error payload, from its error object or its top-level members
func envelopeFields(payload []byte) (code, message string) {
	if obj, ok := objectMember(payload, "error"); ok && obj[0] == '{' {
		return firstTruthyText(obj, "code", "type"), truthyText(obj, "message")
	}
	return firstTruthyText(payload, "code", "error_code", "type"),
		firstTruthyText(payload, "message", "error_description", "error")
}

// Failure for an error payload a provider sent inside a stream, its status read from the payload
func StreamErrorFromPayload(payload []byte, header http.Header) *APIError {
	payload = trimJSON(payload)
	if len(payload) == 0 || payload[0] != '{' || !looseValid(string(payload), true) {
		payload = nil
	}
	return streamError(payload, statusFromPayload(payload), header)
}

// Builds the in-stream failure for an object payload and the status found for it
func streamError(payload []byte, status int, header http.Header) *APIError {
	body := maskJSON(errorBody(payload, status))
	e := &APIError{Status: status, body: body, Header: header, InStream: true}
	if obj, ok := objectMember(body, "error"); ok {
		e.code, e.message = firstTruthyText(obj, "code", "type"), truthyText(obj, "message")
	}
	return e
}

// Error payload normalized to an object with an error member holding message and code
func errorBody(payload []byte, status int) json.RawMessage {
	if obj, ok := objectMember(payload, "error"); ok && obj[0] == '{' {
		return bytes.Clone(payload)
	}
	code := firstTruthyText(payload, "code", "error_code", "type")
	if code == "" {
		code = "provider_stream_error"
		if status != 0 {
			code = "HTTP_" + strconv.Itoa(status)
		}
	}
	message := firstTruthyText(payload, "message", "error_description", "error")
	if message == "" {
		message = "Provider stream returned an error event."
	}
	inner := []Member{{"message", appendString(nil, message, 0)}, {"code", appendString(nil, code, 0)}}
	for _, key := range [...]string{"request_id", "param", "type"} {
		if raw, ok := objectMember(payload, key); ok && truthy(raw) {
			inner = append(inner, Member{key, raw})
		}
	}
	return Object([]Member{{"error", Object(inner)}})
}

// Reports whether a JSON value counts as present, null, false, zero and empty values not counting
func truthy(raw []byte) bool {
	raw = trimJSON(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case 'n', 'f':
		return false
	case '"':
		return len(raw) > 2
	case '[', '{':
		return len(raw) < 2 || len(trimJSON(raw[1:len(raw)-1])) > 0
	case 't', 'N', 'I':
		return true
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	return err != nil || f != 0
}

// Text of a JSON value, a string as itself and anything else as its JSON text
func valueText(raw []byte) string {
	if s, ok := stringValue(raw); ok {
		return s
	}
	return string(trimJSON(raw))
}

// Text of a member when its value counts as present, empty otherwise
func truthyText(obj []byte, key string) string {
	if raw, ok := objectMember(obj, key); ok && truthy(raw) {
		return valueText(raw)
	}
	return ""
}

// Text of the first listed member whose value counts as present
func firstTruthyText(obj []byte, keys ...string) string {
	for _, key := range keys {
		if text := truthyText(obj, key); text != "" {
			return text
		}
	}
	return ""
}

// Reports whether r can be part of a word, letters, numbers and the underscore
func isWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// Value of a decimal digit of any script
func digitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if r < utf8.RuneSelf || !unicode.IsDigit(r) {
		return 0, false
	}
	first := r
	for unicode.IsDigit(first - 1) {
		first--
	}
	return int(r-first) % 10, true
}

// First standalone three-digit HTTP status from 100 to 599 in a text, zero when it has none
func statusFromText(s string) int {
	afterWord := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !afterWord && r >= '1' && r <= '5' {
			second, n2 := utf8.DecodeRuneInString(s[i+size:])
			third, n3 := utf8.DecodeRuneInString(s[i+size+n2:])
			tens, ok2 := digitValue(second)
			ones, ok3 := digitValue(third)
			following, n4 := utf8.DecodeRuneInString(s[i+size+n2+n3:])
			if ok2 && ok3 && (n4 == 0 || !isWord(following)) {
				return int(r-'0')*100 + tens*10 + ones
			}
		}
		afterWord = isWord(r)
		i += size
	}
	return 0
}

// HTTP status a JSON value names, an integer from 100 to 599 or the first one inside a string
func statusFromValue(raw []byte) int {
	if s, ok := stringValue(raw); ok {
		return statusFromText(s)
	}
	if n, err := strconv.Atoi(string(trimJSON(raw))); err == nil && n >= 100 && n < 600 {
		return n
	}
	return 0
}

// HTTP status an error payload names in its status and code members, zero when it names none
func statusFromPayload(payload []byte) int {
	var candidates []json.RawMessage
	add := func(obj []byte, keys ...string) {
		for _, key := range keys {
			if raw, ok := objectMember(obj, key); ok {
				candidates = append(candidates, raw)
			}
		}
	}
	add(payload, "status_code", "status", "http_status")
	if obj, ok := objectMember(payload, "error"); ok && obj[0] == '{' {
		add(obj, "status_code", "status", "http_status", "code")
	}
	add(payload, "code")
	for _, raw := range candidates {
		if status := statusFromValue(raw); status != 0 {
			return status
		}
	}
	return 0
}

// Object a text holds once trimmed, false unless the whole text is one JSON object
func objectFromText(text string) ([]byte, bool) {
	text = trimSpace(text)
	if !strings.HasPrefix(text, "{") || !looseValid(text, true) {
		return nil, false
	}
	return trimJSON([]byte(text)), true
}

// Lines of a text split at every line boundary Unicode defines, terminators removed
func textLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			start = i + size
		case '\r':
			lines = append(lines, s[start:i])
			if strings.HasPrefix(s[i+size:], "\n") {
				size++
			}
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// One event of text laid out like a server-sent event stream, with the HTTP code its comments and fields name
type textEvent struct {
	event  string
	data   []string
	filled bool
	status int
}

// Events of provider text that looks like a server-sent event stream
func textEvents(text string) []textEvent {
	var events []textEvent
	var cur textEvent
	var named [3]string
	flush := func() {
		for _, value := range named {
			if cur.status == 0 {
				cur.status = statusFromText(value)
			}
		}
		if cur.event != "" || len(cur.data) > 0 || cur.filled {
			events = append(events, cur)
		}
		cur, named = textEvent{}, [3]string{}
	}
	for _, line := range textLines(text) {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if line[0] == ':' {
			if cur.filled = true; cur.status == 0 {
				cur.status = statusFromText(trimSpace(line[1:]))
			}
			continue
		}
		name, value, found := strings.Cut(line, ":")
		name = strings.ToLower(trimSpace(name))
		value = strings.TrimPrefix(value, " ")
		switch {
		case found && name == "event":
			cur.event = trimSpace(value)
		case found && name == "data":
			cur.data = append(cur.data, value)
		default:
			cur.filled = true
			if at := slices.Index([]string{"status", "status_code", "http_status"}, name); at >= 0 && found {
				named[at] = value
			} else if at >= 0 {
				named[at] = ""
			}
		}
	}
	flush()
	return events
}

// Reports whether a payload reads as an error, an error member or a message with a code or status
func errorShaped(payload []byte) bool {
	if raw, ok := objectMember(payload, "error"); ok && (raw[0] == '{' || raw[0] == '"') {
		return true
	}
	if truthyText(payload, "message") == "" {
		return false
	}
	return firstTruthyText(payload, "code", "error_code") != "" || statusFromPayload(payload) != 0
}

// Failure a provider streamed as message text when the finish reason marks an error, nil otherwise
func StreamErrorFromText(text, finishReason string, header http.Header) *APIError {
	if text == "" {
		return nil
	}
	if reason := strings.ToLower(finishReason); reason != "error" && reason != "error_finish" {
		return nil
	}
	for _, event := range textEvents(text) {
		payload, _ := objectFromText(strings.Join(event.data, "\n"))
		status := event.status
		if status == 0 {
			status = statusFromPayload(payload)
		}
		if status >= 400 || strings.ToLower(trimSpace(event.event)) == "error" || errorShaped(payload) {
			return streamError(payload, status, header)
		}
	}
	if payload, ok := objectFromText(text); ok {
		return streamError(payload, statusFromPayload(payload), header)
	}
	if trimSpace(text) != "" {
		return streamError(nil, 0, header)
	}
	return nil
}

// Reports whether streamed text still reads as the start of an echoed server-sent event block
func StreamTextMayBeSSE(text string) bool {
	stripped := strings.TrimLeftFunc(text, isSpace)
	if stripped == "" {
		return false
	}
	lines := textLines(stripped)
	trailing := strings.HasSuffix(stripped, "\n") || strings.HasSuffix(stripped, "\r")
	saw := false
	for index, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ':' {
			saw = true
			continue
		}
		name, _, found := strings.Cut(line, ":")
		name = strings.ToLower(trimSpace(name))
		if found && (name == "event" || name == "data" || name == "id" || name == "retry") {
			saw = true
			continue
		}
		if index != len(lines)-1 || trailing {
			return false
		}
		for _, known := range [...]string{"event", "data", "id", "retry"} {
			if strings.HasPrefix(known, name) {
				return true
			}
		}
		return false
	}
	return saw
}
