package speech

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Largest reply body a speech service may send
const BodyCap = 16 << 20

// Agent every service of speech is told
const Agent = "icebath/speech"

// Client whose connect, TLS and first byte each get the timeout
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}}
}

// Body whose reads fail once no byte arrives within the idle window
type idleBody struct {
	r    io.ReadCloser
	idle time.Duration
}

func (b idleBody) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := b.r.Read(p)
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		return r.n, r.err
	case <-time.After(b.idle):
		b.r.Close()
		return 0, fmt.Errorf("read timed out after %d seconds", int(b.idle.Seconds()))
	}
}

func (b idleBody) Close() error { return b.r.Close() }

// A request sent with the headers, its connect, first byte and every quiet stretch of its body each bounded by the timeout
func Send(ctx context.Context, method, url string, body []byte, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", Agent)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := Client(timeout).Do(request)
	if err != nil {
		return nil, err
	}
	response.Body = idleBody{r: response.Body, idle: timeout}
	return response, nil
}

// A JSON body posted with a 60 second bound on each phase
func PostJSON(ctx context.Context, url string, payload any, headers map[string]string) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for name, value := range headers {
		merged[name] = value
	}
	return Send(ctx, http.MethodPost, url, body, merged, 60*time.Second)
}

// A failed reply worded as HTTP libraries word it, with what the service said
type StatusError struct {
	Status int
	Reason string
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	kind := "Client"
	if e.Status >= 500 {
		kind = "Server"
	}
	message := fmt.Sprintf("%d %s Error: %s for url: %s", e.Status, kind, e.Reason, e.URL)
	if detail := strings.TrimSpace(e.Body); detail != "" {
		message += ": " + Prefix(detail, 300)
	}
	return message
}

// First n characters of text
func Prefix(text string, n int) string {
	text = strings.ToValidUTF8(text, "�")
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n])
}

// The failure of a 4xx or 5xx reply, its body read and closed, nil for any other
func Failed(response *http.Response) error {
	if response.StatusCode < 400 {
		return nil
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	reason := strings.TrimSpace(strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)))
	return &StatusError{Status: response.StatusCode, Reason: reason, URL: response.Request.URL.String(), Body: string(raw)}
}

// A reply body read under the cap, the reply closed
func Body(response *http.Response, label string, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = BodyCap
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("%s response exceeds %d bytes", label, limit)
	}
	return raw, nil
}

// A JSON object of a reply body read under the cap, empty for an empty body
func Object(response *http.Response, label string) (map[string]any, error) {
	raw, err := Body(response, label, 0)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s response is not a JSON object: %w", label, err)
	}
	return out, nil
}

// Writes a reply body to a file under the cap, the reply closed
func Save(response *http.Response, path, label string) (string, error) {
	data, err := Body(response, label, 0)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("%s returned no audio", label)
	}
	return path, os.WriteFile(path, data, 0o644)
}

// Pieces of a reply body as they arrive, at most size bytes each and exactly size when filled but for the last, the body closed after
func Chunks(body io.ReadCloser, size int, fill bool) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		defer body.Close()
		buf := make([]byte, size)
		for {
			var n int
			var err error
			if fill {
				if n, err = io.ReadFull(body, buf); errors.Is(err, io.ErrUnexpectedEOF) {
					err = io.EOF
				}
			} else {
				n, err = body.Read(buf)
			}
			if n > 0 && !yield(append([]byte(nil), buf[:n]...), nil) {
				return
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// One field or file of a multipart form
type Part struct {
	Name, Value string
	// File to upload under the name, with its type, guessed from the extension when empty
	File, Type string
}

// Media types of the audio and video files speech uploads, the same on every machine
var mediaTypes = map[string]string{
	".aac": "audio/aac", ".amr": "audio/amr", ".flac": "audio/flac", ".m4a": "audio/mp4", ".mp3": "audio/mpeg", ".oga": "audio/ogg",
	".ogg": "audio/ogg", ".opus": "audio/opus", ".wav": "audio/vnd.wave", ".mp4": "video/mp4", ".mpeg": "video/mpeg", ".webm": "video/webm",
}

// Media type of a file by its extension, this machine's table for those speech has none of, application/octet-stream when unknown
func MediaType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if kind, ok := mediaTypes[ext]; ok {
		return kind
	}
	if kind, _, err := mime.ParseMediaType(mime.TypeByExtension(ext)); err == nil && kind != "" {
		return kind
	}
	return "application/octet-stream"
}

// Multipart body of the parts and its content type
func Form(parts []Part) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.File == "" {
			if err := w.WriteField(p.Name, p.Value); err != nil {
				return nil, "", err
			}
			continue
		}
		data, err := os.ReadFile(p.File)
		if err != nil {
			return nil, "", err
		}
		kind := p.Type
		if kind == "" {
			kind = MediaType(p.File)
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, p.Name, filepath.Base(p.File)))
		header.Set("Content-Type", kind)
		part, err := w.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		part.Write(data)
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// A multipart upload with a bound on each phase
func Upload(ctx context.Context, url string, parts []Part, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	body, kind, err := Form(parts)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{"Content-Type": kind}
	for name, value := range headers {
		merged[name] = value
	}
	return Send(ctx, http.MethodPost, url, body, merged, timeout)
}

// Why a transcription failed, the permission of the file named on its own
func Failing(err error, path, label string) Transcript {
	if errors.Is(err, os.ErrPermission) {
		return Failure("Permission denied: " + path)
	}
	return Failure(label + " failed: " + err.Error())
}

// Text of a value as JSON gives it
func Text(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

// Bytes of base64 text in the standard or the URL alphabet, white space skipped and the padding optional, as services send audio
func Base64(text string) ([]byte, error) {
	out := make([]byte, 0, len(text)/4*3+3)
	var acc uint32
	n, last := 0, 0
	flush := func() {
		switch n {
		case 2:
			out = append(out, byte(acc>>4))
		case 3:
			out = append(out, byte(acc>>10), byte(acc>>2))
		}
		acc, n = 0, 0
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		var v byte
		switch {
		case c >= 'A' && c <= 'Z':
			v = c - 'A'
		case c >= 'a' && c <= 'z':
			v = c - 'a' + 26
		case c >= '0' && c <= '9':
			v = c - '0' + 52
		case c == '+' || c == '-':
			v = 62
		case c == '/' || c == '_':
			v = 63
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			continue
		case c == '=':
			if n == 1 {
				return nil, base64.CorruptInputError(last)
			}
			flush()
			continue
		default:
			return nil, base64.CorruptInputError(i)
		}
		acc, n, last = acc<<6|uint32(v), n+1, i
		if n == 4 {
			out, acc, n = append(out, byte(acc>>16), byte(acc>>8), byte(acc)), 0, 0
		}
	}
	if n == 1 {
		return nil, base64.CorruptInputError(last)
	}
	flush()
	return out, nil
}

// Bytes of base64 text in the standard alphabet read leniently: any other character dropped and the padding made whole after its first
// sign; text cut short of a whole quantum refused
func Base64Std(text string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' {
			return r
		}
		return -1
	}, text)
	if i := strings.IndexByte(clean, '='); i >= 0 {
		clean = clean[:i] + strings.Repeat("=", (4-i%4)%4)
	}
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, Misconfigured("Incorrect padding")
	}
	return data, nil
}

// Whether a JSON value holds something: not null, false, zero, empty text, an empty list or an empty object
func Held(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// What a JSON value is, as a refusal of a reply of the wrong shape names it
func Kind(value any) string {
	switch value.(type) {
	case nil:
		return "nothing"
	case bool:
		return "true or false"
	case string:
		return "text"
	case float64, int, int64:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "a mapping"
	}
	return fmt.Sprintf("%T", value)
}
