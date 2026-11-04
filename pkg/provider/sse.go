package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Error code of an SSE data frame that was not JSON
const ProviderStreamNonJSONCode = "provider_stream_non_json_data"

// Error code of an SSE frame that carried no payload at all
const ProviderStreamEmptyFrameCode = "provider_stream_empty_frame"

// Response id of a stream that ended without a finish reason after delivering content
const PartialStreamStubID = "partial-stream-stub"

// Upper bound on provider error text copied into an error message
const providerStreamErrorTextLimit = 4096

// Finish reasons some relays use to mark an error encoded as content
var providerStreamErrorFinishReasons = map[string]bool{"error": true, "error_finish": true}

var sseFieldNames = map[string]bool{"event": true, "data": true, "id": true, "retry": true}

// One parsed Server Sent Events block
type sseFrame struct {
	Event      string
	Data       string
	ID         string
	Retry      string
	Comments   []string
	Fields     map[string]string
	StatusCode int
}

// Incremental SSE frame reader over a response body
type sseReader struct {
	r    *bufio.Reader
	done bool
}

func newSSEReader(body io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(body, 64*1024)}
}

// Reads the next frame, io.EOF once the body is exhausted
func (s *sseReader) Next() (*sseFrame, error) {
	if s.done {
		return nil, io.EOF
	}
	cur := &sseFrame{Fields: map[string]string{}}
	var data []string
	saw := false
	for {
		line, err := s.r.ReadString('\n')
		if err != nil && line == "" {
			s.done = true
			if saw {
				cur.Data = strings.Join(data, "\n")
				cur.StatusCode = frameStatusCode(cur)
				return cur, nil
			}
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if saw {
				cur.Data = strings.Join(data, "\n")
				cur.StatusCode = frameStatusCode(cur)
				return cur, nil
			}
			if err != nil {
				s.done = true
				return nil, io.EOF
			}
			continue
		}
		saw = true
		if strings.HasPrefix(line, ":") {
			cur.Comments = append(cur.Comments, strings.TrimSpace(line[1:]))
			continue
		}
		field, value, hasSep := strings.Cut(line, ":")
		field = strings.ToLower(strings.TrimSpace(field))
		if !hasSep {
			cur.Fields[field] = ""
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			cur.Event = strings.TrimSpace(value)
		case "data":
			data = append(data, value)
		case "id":
			cur.ID = value
		case "retry":
			cur.Retry = value
		default:
			cur.Fields[field] = value
		}
		if err != nil {
			s.done = true
			cur.Data = strings.Join(data, "\n")
			cur.StatusCode = frameStatusCode(cur)
			return cur, nil
		}
	}
}

// Status code hinted by comments or status fields of a frame
func frameStatusCode(f *sseFrame) int {
	cands := append([]string{}, f.Comments...)
	for _, k := range []string{"status", "status_code", "http_status"} {
		if v, ok := f.Fields[k]; ok {
			cands = append(cands, v)
		}
	}
	for _, c := range cands {
		if code := statusCodeFromValue(c); code != 0 {
			return code
		}
	}
	return 0
}

// Parses provider text that looks like SSE into frames
func parseSSEEvents(text string) []*sseFrame {
	var out []*sseFrame
	r := newSSEReader(strings.NewReader(text))
	for {
		f, err := r.Next()
		if err != nil {
			return out
		}
		out = append(out, f)
	}
}

var statusCodeRe = regexp.MustCompile(`(?i)(?:HTTP_STATUS/)?\b([1-5]\d\d)\b`)

// HTTP status carried by an int or a string such as HTTP_STATUS/429
func statusCodeFromValue(v any) int {
	switch c := v.(type) {
	case int:
		if c >= 100 && c < 600 {
			return c
		}
	case int64:
		if c >= 100 && c < 600 {
			return int(c)
		}
	case float64:
		if c == float64(int(c)) && c >= 100 && c < 600 {
			return int(c)
		}
	case json.Number:
		if n, err := c.Int64(); err == nil && n >= 100 && n < 600 {
			return int(n)
		}
	case string:
		if m := statusCodeRe.FindStringSubmatch(c); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// HTTP status carried by status fields of an error payload or its error object
func statusCodeFromPayload(payload map[string]any) int {
	if payload == nil {
		return 0
	}
	cands := []any{payload["status_code"], payload["status"], payload["http_status"]}
	if eo, ok := payload["error"].(map[string]any); ok {
		cands = append(cands, eo["status_code"], eo["status"], eo["http_status"], eo["code"])
	}
	cands = append(cands, payload["code"])
	for _, c := range cands {
		if c == nil {
			continue
		}
		if code := statusCodeFromValue(c); code != 0 {
			return code
		}
	}
	return 0
}

// JSON object decoded from text that starts with a brace
func jsonObjectFromText(text string) map[string]any {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "{") {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}

// Error payload normalized to the OpenAI body.error shape
func providerErrorBody(payload map[string]any, status int) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	} else if _, ok := payload["error"].(map[string]any); ok {
		return payload
	}
	code := firstString(payload["code"], payload["error_code"], payload["type"])
	if code == "" {
		if status != 0 {
			code = fmt.Sprintf("HTTP_%d", status)
		} else {
			code = "provider_stream_error"
		}
	}
	message := firstString(payload["message"], payload["error_description"], payload["error"])
	if message == "" {
		message = "Provider stream returned an error event."
	}
	norm := map[string]any{"message": message, "code": code}
	for _, k := range []string{"request_id", "param", "type"} {
		if v, ok := payload[k]; ok && v != nil && v != "" {
			norm[k] = v
		}
	}
	return map[string]any{"error": norm}
}

// First non empty value rendered as a string
func firstString(vals ...any) string {
	for _, v := range vals {
		switch c := v.(type) {
		case nil:
			continue
		case string:
			if c != "" {
				return c
			}
		case bool:
			if c {
				return "true"
			}
		case float64:
			return strconv.FormatFloat(c, 'f', -1, 64)
		case map[string]any, []any:
			continue
		default:
			return fmt.Sprint(c)
		}
	}
	return ""
}

func payloadHasErrorShape(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	switch payload["error"].(type) {
	case map[string]any, string:
		return true
	}
	if firstString(payload["message"]) == "" {
		return false
	}
	return firstString(payload["code"], payload["error_code"]) != "" || statusCodeFromPayload(payload) != 0
}

// Reports whether pending text still looks like an SSE control block
func providerStreamTextMayBeSSE(text string) bool {
	stripped := strings.TrimLeft(text, " \t\r\n")
	if stripped == "" {
		return false
	}
	lines := strings.Split(stripped, "\n")
	trailingNewline := strings.HasSuffix(stripped, "\n") || strings.HasSuffix(stripped, "\r")
	sawField := false
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			sawField = true
			continue
		}
		field, _, hasSep := strings.Cut(line, ":")
		name := strings.ToLower(strings.TrimSpace(field))
		if hasSep && sseFieldNames[name] {
			sawField = true
			continue
		}
		lastIncomplete := i == len(lines)-1 && !trailingNewline
		if lastIncomplete {
			for f := range sseFieldNames {
				if strings.HasPrefix(f, name) {
					return true
				}
			}
		}
		return false
	}
	return sawField
}

// ProviderStreamError is an API error a provider encoded inside streamed content
type ProviderStreamError struct {
	StatusCode int
	Body       map[string]any
	RawText    string
	Headers    http.Header
}

func (e *ProviderStreamError) Error() string {
	eo, _ := e.Body["error"].(map[string]any)
	parts := []string{"Provider stream returned an error event"}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.StatusCode))
	}
	if code := firstString(eo["code"]); code != "" {
		parts = append(parts, code)
	}
	text := strings.Join(parts, " - ")
	if msg := firstString(eo["message"]); msg != "" {
		text += ": " + msg
	}
	return text
}

// Code of the error object carried by the body
func (e *ProviderStreamError) Code() string {
	eo, _ := e.Body["error"].(map[string]any)
	return firstString(eo["code"])
}

// ResponseHeaders exposes the stream response headers for reset and welcome tier parsing
func (e *ProviderStreamError) ResponseHeaders() http.Header { return e.Headers }

// Body as JSON text for classification
func (e *ProviderStreamError) BodyText() string {
	b, _ := json.Marshal(e.Body)
	return string(b)
}

// Reports whether the error is the contentless SSE frame translation
func isProviderStreamEmptyFrameError(err error) bool {
	pse, ok := err.(*ProviderStreamError)
	return ok && pse.Code() == ProviderStreamEmptyFrameCode
}

// Error for an SSE data frame the JSON decoder rejected
func providerStreamErrorFromNonJSON(raw string, headers http.Header) *ProviderStreamError {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &ProviderStreamError{Body: providerErrorBody(map[string]any{
			"code":    ProviderStreamEmptyFrameCode,
			"message": "Provider stream returned an empty SSE data frame (keepalive with no payload).",
		}, 0), Headers: headers}
	}
	safe := redactSensitiveText(sanitizeSurrogates(raw))
	if len(safe) > providerStreamErrorTextLimit {
		safe = safe[:providerStreamErrorTextLimit]
	}
	msg := safe
	if msg == "" {
		msg = "Provider stream returned non-JSON SSE data."
	}
	return &ProviderStreamError{Body: providerErrorBody(map[string]any{
		"code": ProviderStreamNonJSONCode, "message": msg,
	}, 0), RawText: safe, Headers: headers}
}

// Error for provider error text streamed as assistant content
func providerStreamErrorFromText(text, finishReason string, headers http.Header) *ProviderStreamError {
	if text == "" || !providerStreamErrorFinishReasons[strings.ToLower(finishReason)] {
		return nil
	}
	mk := func(payload map[string]any, status int) *ProviderStreamError {
		return &ProviderStreamError{StatusCode: status, Body: providerErrorBody(payload, status), RawText: text, Headers: headers}
	}
	for _, ev := range parseSSEEvents(text) {
		isErr := strings.ToLower(strings.TrimSpace(ev.Event)) == "error"
		payload := jsonObjectFromText(ev.Data)
		status := ev.StatusCode
		if status == 0 {
			status = statusCodeFromPayload(payload)
		}
		if status >= 400 || isErr || payloadHasErrorShape(payload) {
			if payload == nil {
				payload = map[string]any{}
			}
			return mk(payload, status)
		}
	}
	if payload := jsonObjectFromText(text); payload != nil {
		return mk(payload, statusCodeFromPayload(payload))
	}
	if strings.TrimSpace(text) != "" {
		return mk(map[string]any{}, 0)
	}
	return nil
}

// Sentinel some routers return as the sole assistant message after an upstream connect timeout
const routerTimeoutShim = "Connect timeout, please try again later."

// Reports whether streamed text is still a prefix of the router shim sentinel
func routerTimeoutShimMayFollow(text string) bool {
	return text != "" && strings.HasPrefix(routerTimeoutShim, strings.TrimLeft(text, " \t\r\n"))
}

// Reports whether a completed response is a router failure encoded as HTTP 200
func isRouterTimeoutShim(content string, toolCalls int, usage Usage) bool {
	if strings.TrimSpace(content) != routerTimeoutShim || toolCalls > 0 {
		return false
	}
	return usage.OutputTokens <= 0
}

var finishReasonAliases = map[string]string{"max_tokens": "length", "end": "stop", "function_call": "tool_calls"}

// Folds a wire finish reason to the lowercase OpenAI contract value
func normalizeFinishReason(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	if alias, ok := finishReasonAliases[lower]; ok {
		return alias
	}
	return lower
}

// Replaces invalid UTF8 bytes, which is how lone surrogates arrive, with the replacement character
func sanitizeSurrogates(s string) string {
	return strings.ToValidUTF8(s, "\ufffd")
}

var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9_\-\.=]{16,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|secret|password|authorization)(["']?\s*[:=]\s*["']?)[^\s"',}]{8,}`),
}

// Masks credential shaped substrings of provider text copied into error messages
func redactSensitiveText(text string) string {
	if text == "" {
		return text
	}
	out := text
	for i, re := range redactPatterns {
		if i == len(redactPatterns)-1 {
			out = re.ReplaceAllString(out, "${1}${2}***")
			continue
		}
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			if len(m) <= 8 {
				return "***"
			}
			return m[:4] + "***" + m[len(m)-4:]
		})
	}
	return out
}
