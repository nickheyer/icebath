package dashboard

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Response a route or middleware returns, sent through the ASGI send callable
type ASGIResponse interface {
	Status() int
	Header() Headers
	Serve(ctx context.Context, scope *Scope, receive Receive, send Send) error
}

// Plain body response, the Starlette Response with its rendered body and raw headers
type Response struct {
	StatusCode int
	MediaType  string
	Body       []byte
	RawHeaders []Header
	Background func(ctx context.Context) error
}

// Response of a body with headers given as name and value pairs, the media type an empty string for None
func NewResponse(body []byte, status int, mediaType string, headers ...string) *Response {
	r := &Response{StatusCode: status, MediaType: mediaType, Body: body}
	if body == nil {
		r.Body = []byte{}
	}
	r.RawHeaders = initHeaders(status, r.Body, true, mediaType, headers)
	return r
}

// Raw headers of a response, content length and type appended when the given ones lack them
func initHeaders(status int, body []byte, hasBody bool, mediaType string, headers []string) []Header {
	raw := make([]Header, 0, len(headers)/2+2)
	popLength, popType := true, true
	for i := 0; i+1 < len(headers); i += 2 {
		k := mustLatin1(pystr.Lower(headers[i]))
		raw = append(raw, Header{k, mustLatin1(headers[i+1])})
		popLength = popLength && k != "content-length"
		popType = popType && k != "content-type"
	}
	if hasBody && popLength && !(status < 200 || status == 204 || status == 304) {
		raw = append(raw, Header{"content-length", strconv.Itoa(len(body))})
	}
	if mediaType != "" && popType {
		if strings.HasPrefix(mediaType, "text/") && !strings.Contains(pystr.Lower(mediaType), "charset=") {
			mediaType += "; charset=utf-8"
		}
		raw = append(raw, Header{"content-type", mustLatin1(mediaType)})
	}
	return raw
}

func (r *Response) Status() int     { return r.StatusCode }
func (r *Response) Header() Headers { return headersOfRaw(&r.RawHeaders) }

// Sends the start and the whole body, a websocket denial when the scope is a websocket
func (r *Response) Serve(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type == "websocket" {
		send = websocketDenialSend(send)
	}
	if err := send(ctx, Message{Type: "http.response.start", Status: r.StatusCode, Headers: r.RawHeaders}); err != nil {
		return err
	}
	if err := send(ctx, Message{Type: "http.response.body", Body: r.Body}); err != nil {
		return err
	}
	if r.Background != nil {
		return r.Background(ctx)
	}
	return nil
}

// Send that renames http response events to their websocket denial forms
func websocketDenialSend(send Send) Send {
	return func(ctx context.Context, m Message) error {
		if m.Type == "http.response.start" || m.Type == "http.response.body" {
			m.Type = "websocket." + m.Type
		}
		return send(ctx, m)
	}
}

// Text response under text/plain with the utf-8 charset
func PlainTextResponse(text string, status int, headers ...string) *Response {
	return NewResponse([]byte(text), status, "text/plain", headers...)
}

// Markup response under text/html with the utf-8 charset
func HTMLResponse(html string, status int, headers ...string) *Response {
	return NewResponse([]byte(html), status, "text/html", headers...)
}

// Compact json.dumps of content without ASCII escaping or NaN, the Starlette JSON body bytes
func renderJSON(content any) ([]byte, error) {
	text, err := pyjson.Dumps(content, pyjson.EnsureASCII(false), pyjson.AllowNaN(false), pyjson.Separators(",", ":"))
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// JSON response of content, the encoding error when content cannot be rendered
func JSONResponse(content any, status int, headers ...string) (*Response, error) {
	body, err := renderJSON(content)
	if err != nil {
		return nil, err
	}
	return NewResponse(body, status, "application/json", headers...), nil
}

// JSON response of content Go code builds and knows to be encodable
func MustJSONResponse(content any, status int, headers ...string) *Response {
	r, err := JSONResponse(content, status, headers...)
	if err != nil {
		panic(err)
	}
	return r
}

// Empty body redirect with the quoted location header, 307 unless a status is given
func RedirectResponse(url string, status int, headers ...string) *Response {
	if status == 0 {
		status = 307
	}
	r := NewResponse([]byte{}, status, "", headers...)
	r.Header().Set("location", pyurl.Quote(url, ":/%#?=@[]!$&'()*+,;"))
	return r
}

// Response whose body chunks come from an iterator, sent until it ends or the client disconnects
type StreamingResponse struct {
	StatusCode int
	MediaType  string
	RawHeaders []Header
	Body       func(ctx context.Context, yield func(chunk []byte) error) error
	Background func(ctx context.Context) error
}

// Streaming response with no content length, the iterator yielding body chunks
func NewStreamingResponse(body func(ctx context.Context, yield func([]byte) error) error, status int, mediaType string, headers ...string) *StreamingResponse {
	return &StreamingResponse{StatusCode: status, MediaType: mediaType, Body: body,
		RawHeaders: initHeaders(status, nil, false, mediaType, headers)}
}

func (r *StreamingResponse) Status() int     { return r.StatusCode }
func (r *StreamingResponse) Header() Headers { return headersOfRaw(&r.RawHeaders) }

func (r *StreamingResponse) stream(ctx context.Context, send Send) error {
	if err := send(ctx, Message{Type: "http.response.start", Status: r.StatusCode, Headers: r.RawHeaders}); err != nil {
		return err
	}
	if err := r.Body(ctx, func(chunk []byte) error {
		return send(ctx, Message{Type: "http.response.body", Body: chunk, MoreBody: true})
	}); err != nil {
		return err
	}
	return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
}

// Streams the body while a listener cancels the stream when the client disconnects
func (r *StreamingResponse) Serve(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type == "websocket" {
		if err := r.stream(ctx, websocketDenialSend(send)); err != nil {
			return err
		}
	} else if err := untilDisconnect(ctx, receive, func(ctx context.Context) error { return r.stream(ctx, send) }); err != nil {
		return err
	}
	if r.Background != nil {
		return r.Background(ctx)
	}
	return nil
}

// Runs work under a context cancelled once receive reports the client gone, whichever ends first
func untilDisconnect(ctx context.Context, receive Receive, work func(ctx context.Context) error) error {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		err := work(wctx)
		cancel()
		done <- err
	}()
	go func() {
		for {
			m, err := receive(wctx)
			if err != nil || m.Type == "http.disconnect" {
				cancel()
				return
			}
		}
	}()
	err := <-done
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return nil
	}
	return err
}

// File response with stat headers, conditional ranges and an attachment disposition when named
type FileResponse struct {
	Path        string
	StatusCode  int
	MediaType   string
	Filename    *string
	RawHeaders  []Header
	Background  func(ctx context.Context) error
	stat        fs.FileInfo
	opener      func() (*os.File, error)
	pinned      io.ReadSeeker
	chunkSize   int
	maxRanges   int
	disposition string
}

// File response of a path, the media type guessed from the filename or path when empty
func NewFileResponse(path string, status int, mediaType string, filename *string, dispositionType string, headers ...string) *FileResponse {
	if mediaType == "" {
		guessFrom := path
		if filename != nil {
			guessFrom = *filename
		}
		if t, _, err := pymimetypes.GuessType(guessFrom, true); err == nil && t != "" {
			mediaType = t
		} else {
			mediaType = "application/octet-stream"
		}
	}
	if dispositionType == "" {
		dispositionType = "attachment"
	}
	r := &FileResponse{Path: path, StatusCode: status, MediaType: mediaType, Filename: filename,
		chunkSize: 64 * 1024, maxRanges: 100, disposition: dispositionType}
	r.RawHeaders = initHeaders(status, nil, false, mediaType, headers)
	r.Header().SetDefault("accept-ranges", "bytes")
	if filename != nil {
		quoted := pyurl.Quote(*filename, "/")
		if quoted != *filename {
			r.Header().SetDefault("content-disposition", dispositionType+"; filename*=utf-8''"+quoted)
		} else {
			r.Header().SetDefault("content-disposition", dispositionType+"; filename=\""+*filename+"\"")
		}
	}
	return r
}

func (r *FileResponse) Status() int     { return r.StatusCode }
func (r *FileResponse) Header() Headers { return headersOfRaw(&r.RawHeaders) }

// Python float seconds of a modification time, the os.stat_result st_mtime value
func statMtime(st fs.FileInfo) float64 {
	t := st.ModTime()
	return float64(t.Unix()) + float64(t.Nanosecond())*1e-9
}

// Sets content length, last modified and the md5 etag of mtime and size unless already set
func (r *FileResponse) setStatHeaders(st fs.FileInfo) {
	mtime := statMtime(st)
	size := strconv.FormatInt(st.Size(), 10)
	sum := md5.Sum([]byte(pyrepr.Float(mtime) + "-" + size))
	h := r.Header()
	h.SetDefault("content-length", size)
	h.SetDefault("last-modified", time.Unix(int64(mtime), 0).UTC().Format(http.TimeFormat))
	h.SetDefault("etag", "\""+hex.EncodeToString(sum[:])+"\"")
}

// Python ValueError a range header fails with, carried as a 400 or 416 response
type rangeError struct {
	malformed string
	maxSize   int64
}

func (e *rangeError) Error() string { return e.malformed }

// Sends the file whole or the requested byte ranges, the header alone for HEAD
func (r *FileResponse) Serve(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	headerOnly := scope.Type == "http" && strings.ToUpper(scope.Method) == "HEAD"
	if scope.Type == "websocket" {
		send = websocketDenialSend(send)
	}
	if r.opener != nil {
		f, err := r.opener()
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		r.pinned, r.stat = f, st
		r.setStatHeaders(st)
	}
	st := r.stat
	if st == nil {
		var err error
		st, err = os.Stat(r.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return &RuntimeError{Msg: fmt.Sprintf("File at path %s does not exist.", r.Path)}
		}
		if err != nil {
			return err
		}
		r.setStatHeaders(st)
		if !st.Mode().IsRegular() {
			return &RuntimeError{Msg: fmt.Sprintf("File at path %s is not a file.", r.Path)}
		}
	}
	reqHeaders := headersOfScope(scope)
	httpRange, hasRange := reqHeaders.Lookup("range")
	ifRange, hasIfRange := reqHeaders.Lookup("if-range")
	sendFile := func(ctx context.Context) error { return r.handleSimple(ctx, send, headerOnly) }
	if r.StatusCode == 200 && hasRange && (!hasIfRange || r.shouldUseRange(ifRange)) {
		ranges, err := r.parseRangeHeader(httpRange, st.Size())
		var re *rangeError
		if errors.As(err, &re) {
			if re.malformed != "" {
				return PlainTextResponse(re.malformed, 400).Serve(ctx, scope, receive, send)
			}
			return NewResponse([]byte{}, 416, "text/plain", "Content-Range", fmt.Sprintf("bytes */%d", re.maxSize)).Serve(ctx, scope, receive, send)
		}
		switch len(ranges) {
		case 0:
		case 1:
			sendFile = func(ctx context.Context) error {
				return r.handleSingleRange(ctx, send, ranges[0][0], ranges[0][1], st.Size(), headerOnly)
			}
		default:
			sendFile = func(ctx context.Context) error {
				return r.handleMultipleRanges(ctx, send, ranges, st.Size(), headerOnly)
			}
		}
	}
	var err error
	if scope.Type != "http" || headerOnly {
		err = sendFile(ctx)
	} else {
		err = untilDisconnect(ctx, receive, sendFile)
	}
	if err != nil {
		return err
	}
	if r.Background != nil {
		return r.Background(ctx)
	}
	return nil
}

func (r *FileResponse) handleSimple(ctx context.Context, send Send, headerOnly bool) error {
	if r.pinned != nil {
		return r.pinnedSimple(ctx, send, headerOnly)
	}
	if err := send(ctx, Message{Type: "http.response.start", Status: r.StatusCode, Headers: r.RawHeaders}); err != nil {
		return err
	}
	if headerOnly {
		return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		chunk := make([]byte, r.chunkSize)
		n, err := io.ReadFull(f, chunk)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		more := n == r.chunkSize
		if err := send(ctx, Message{Type: "http.response.body", Body: chunk[:n], MoreBody: more}); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (r *FileResponse) handleSingleRange(ctx context.Context, send Send, start, end, size int64, headerOnly bool) error {
	if r.pinned != nil {
		return r.pinnedSingleRange(ctx, send, start, end, size, headerOnly)
	}
	raw := slices.Clone(r.RawHeaders)
	h := headersOfRaw(&raw)
	h.Set("content-range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	h.Set("content-length", strconv.FormatInt(end-start, 10))
	if err := send(ctx, Message{Type: "http.response.start", Status: 206, Headers: raw}); err != nil {
		return err
	}
	if headerOnly {
		return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	for {
		chunk := make([]byte, min(int64(r.chunkSize), end-start))
		n, err := io.ReadFull(f, chunk)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return err
		}
		start += int64(n)
		more := n == r.chunkSize && start < end
		if err := send(ctx, Message{Type: "http.response.body", Body: chunk[:n], MoreBody: more}); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (r *FileResponse) handleMultipleRanges(ctx context.Context, send Send, ranges [][2]int64, size int64, headerOnly bool) error {
	if r.pinned != nil {
		return r.pinnedMultipleRanges(ctx, send, ranges, size, headerOnly)
	}
	boundaryBytes := make([]byte, 13)
	rand.Read(boundaryBytes)
	boundary := hex.EncodeToString(boundaryBytes)
	contentType := r.Header().Get("content-type")
	static := int64(49 + len(boundary) + len(contentType) + len(strconv.FormatInt(size, 10)))
	length := int64(4 + len(boundary))
	for _, rg := range ranges {
		length += int64(len(strconv.FormatInt(rg[0], 10))+len(strconv.FormatInt(rg[1]-1, 10))) + static + rg[1] - rg[0]
	}
	raw := slices.Clone(r.RawHeaders)
	h := headersOfRaw(&raw)
	h.Set("content-type", "multipart/byteranges; boundary="+boundary)
	h.Set("content-length", strconv.FormatInt(length, 10))
	if err := send(ctx, Message{Type: "http.response.start", Status: 206, Headers: raw}); err != nil {
		return err
	}
	if headerOnly {
		return send(ctx, Message{Type: "http.response.body", Body: []byte{}})
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, rg := range ranges {
		start, end := rg[0], rg[1]
		part := fmt.Sprintf("--%s\r\nContent-Type: %s\r\nContent-Range: bytes %d-%d/%d\r\n\r\n", boundary, contentType, start, end-1, size)
		if err := send(ctx, Message{Type: "http.response.body", Body: []byte(mustLatin1(part)), MoreBody: true}); err != nil {
			return err
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return err
		}
		for start < end {
			chunk := make([]byte, min(int64(r.chunkSize), end-start))
			n, err := f.Read(chunk)
			if n == 0 {
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				return &RuntimeError{Msg: fmt.Sprintf("File at path %s is shorter than expected.", r.Path)}
			}
			start += int64(n)
			if err := send(ctx, Message{Type: "http.response.body", Body: chunk[:n], MoreBody: true}); err != nil {
				return err
			}
		}
		if err := send(ctx, Message{Type: "http.response.body", Body: []byte("\r\n"), MoreBody: true}); err != nil {
			return err
		}
	}
	return send(ctx, Message{Type: "http.response.body", Body: []byte("--" + boundary + "--")})
}

// Whether an If-Range value matches the strong last modified or etag validator
func (r *FileResponse) shouldUseRange(ifRange string) bool {
	if strings.HasPrefix(ifRange, "W/") {
		return false
	}
	return ifRange == r.Header().Get("last-modified") || ifRange == r.Header().Get("etag")
}

// Sorted merged byte ranges of a Range header, the malformed and unsatisfiable errors Starlette raises
func (r *FileResponse) parseRangeHeader(httpRange string, size int64) ([][2]int64, error) {
	units, rng, ok := strings.Cut(httpRange, "=")
	if !ok {
		return nil, &rangeError{malformed: "Malformed range header."}
	}
	if pystr.Lower(pystr.Strip(units)) != "bytes" {
		return nil, &rangeError{malformed: "Only support bytes range"}
	}
	if strings.Count(rng, ",")+1 > r.maxRanges {
		return nil, nil
	}
	ranges := parseRanges(rng, size)
	if len(ranges) == 0 {
		return nil, &rangeError{malformed: "Range header: range must be requested"}
	}
	for _, rg := range ranges {
		if !(0 <= rg[0] && rg[0] < size) {
			return nil, &rangeError{maxSize: size}
		}
	}
	for _, rg := range ranges {
		if rg[0] >= rg[1] {
			return nil, &rangeError{malformed: "Range header: start must be less than end"}
		}
	}
	if len(ranges) == 1 {
		return ranges, nil
	}
	slices.SortFunc(ranges, func(a, b [2]int64) int {
		if a[0] != b[0] {
			return int(a[0] - b[0])
		}
		return int(a[1] - b[1])
	})
	result := [][2]int64{ranges[0]}
	for _, rg := range ranges[1:] {
		last := &result[len(result)-1]
		if rg[0] <= last[1] {
			last[1] = max(last[1], rg[1])
		} else {
			result = append(result, rg)
		}
	}
	return result, nil
}

// Start and exclusive end of each well formed range part, a suffix part counting back from the end
func parseRanges(rng string, size int64) [][2]int64 {
	var out [][2]int64
	for _, part := range strings.Split(rng, ",") {
		part = pystr.Strip(part)
		if part == "" || part == "-" || !strings.Contains(part, "-") {
			continue
		}
		startStr, endStr, _ := strings.Cut(part, "-")
		startStr, endStr = pystr.Strip(startStr), pystr.Strip(endStr)
		var start, end int64
		if startStr != "" {
			v, ok := pyIntText(startStr)
			if !ok {
				continue
			}
			start = v
		} else {
			v, ok := pyIntText(endStr)
			if !ok {
				continue
			}
			start = max(size-v, 0)
		}
		end = size
		if startStr != "" && endStr != "" {
			v, ok := pyIntText(endStr)
			if !ok {
				continue
			}
			if v < size {
				end = v + 1
			}
		}
		out = append(out, [2]int64{start, end})
	}
	return out
}

// Value of a Python int() literal that fits in 64 bits
func pyIntText(s string) (int64, bool) {
	n, ok := pystr.IntText(s)
	if !ok || !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}
