package pyhttpx

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"golang.org/x/text/encoding/ianaindex"
)

// Ordered query parameters as httpx receives them in a params mapping
type Params [][2]string

// TimeoutError is the httpx.TimeoutException raised when one request phase outlasts its timeout
type TimeoutError struct{ Phase string }

func (e *TimeoutError) Error() string { return "timed out" }

// ErrTooManyRedirects is the httpx error raised past the client redirect cap
var ErrTooManyRedirects = errors.New("Exceeded maximum allowed redirects.")

// Buffered response with the status, headers, final URL and body bytes
type Response struct {
	StatusCode int
	Header     http.Header
	URL        string
	Content    []byte
}

// Body decoded with the declared charset or utf-8, invalid bytes replaced
func (r *Response) Text() string { return DecodeText(r.Header, r.Content) }

// Body parsed by json.loads over the raw bytes
func (r *Response) JSON() (any, error) { return pyjson.LoadsBytes(r.Content) }

// Decodes a body like httpx Response.text, the content type charset when known and utf-8 otherwise
func DecodeText(h http.Header, body []byte) string {
	charset := ""
	if ct := h.Get("Content-Type"); ct != "" {
		if _, params, err := mime.ParseMediaType(ct); err == nil {
			charset = strings.ToLower(strings.Trim(params["charset"], `"' `))
		}
	}
	switch charset {
	case "", "utf-8", "utf8", "u8", "utf":
		return pystr.DecodeUTF8Replace(body)
	}
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil || enc == nil {
		return pystr.DecodeUTF8Replace(body)
	}
	out, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return pystr.DecodeUTF8Replace(body)
	}
	return string(out)
}

// URL with params merged into its query like httpx, new keys replacing old ones and the query re-encoded
func MergeParams(rawURL string, params Params) string {
	if len(params) == 0 {
		return rawURL
	}
	frag := ""
	if i := strings.IndexByte(rawURL, '#'); i >= 0 {
		rawURL, frag = rawURL[:i], rawURL[i:]
	}
	base, query, _ := strings.Cut(rawURL, "?")
	type entry struct {
		key  string
		vals []string
	}
	var order []*entry
	byKey := map[string]*entry{}
	add := func(k, v string, replace map[string]bool) {
		e := byKey[k]
		if e == nil {
			e = &entry{key: k}
			byKey[k] = e
			order = append(order, e)
		}
		if replace != nil && !replace[k] {
			replace[k] = true
			e.vals = nil
		}
		e.vals = append(e.vals, v)
	}
	for part := range strings.SplitSeq(query, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		uk, err1 := url.QueryUnescape(k)
		uv, err2 := url.QueryUnescape(v)
		if err1 != nil {
			uk = k
		}
		if err2 != nil {
			uv = v
		}
		add(uk, uv, nil)
	}
	replaced := map[string]bool{}
	for _, kv := range params {
		add(kv[0], kv[1], replaced)
	}
	var b strings.Builder
	for _, e := range order {
		for _, v := range e.vals {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(e.key) + "=" + url.QueryEscape(v))
		}
	}
	return base + "?" + b.String() + frag
}

// Cancel timer restarted at every request phase so each phase gets the full timeout like httpx
type watchdog struct {
	mu    sync.Mutex
	t     *time.Timer
	d     time.Duration
	per   *Timeout
	fired atomic.Bool
	phase atomic.Value
}

// Distinct httpx.Timeout connect, read, write and pool limits, a zero limit never fires
type Timeout struct{ Connect, Read, Write, Pool time.Duration }

func (t Timeout) of(phase string) time.Duration {
	switch phase {
	case "connect":
		return t.Connect
	case "write":
		return t.Write
	case "read":
		return t.Read
	}
	return t.Pool
}

// Watchdog arming each request phase with its own limit, starting with the pool wait
func newPhaseWatchdog(t Timeout, cancel func()) *watchdog {
	w := &watchdog{per: &t}
	w.phase.Store("pool")
	w.t = time.AfterFunc(time.Hour, func() {
		w.fired.Store(true)
		cancel()
	})
	w.reset("pool")
	return w
}

func newWatchdog(d time.Duration, cancel func()) *watchdog {
	w := &watchdog{d: d}
	w.phase.Store("connect")
	if d > 0 {
		w.t = time.AfterFunc(d, func() {
			w.fired.Store(true)
			cancel()
		})
	}
	return w
}

func (w *watchdog) reset(phase string) {
	w.phase.Store(phase)
	if w.t == nil || w.fired.Load() {
		return
	}
	d := w.d
	if w.per != nil {
		d = w.per.of(phase)
	}
	w.mu.Lock()
	if d > 0 {
		w.t.Reset(d)
	} else {
		w.t.Stop()
	}
	w.mu.Unlock()
}

func (w *watchdog) stop() {
	if w.t != nil {
		w.t.Stop()
	}
}

func (w *watchdog) wrap(err error) error {
	if err != nil && w.fired.Load() {
		return &TimeoutError{Phase: w.phase.Load().(string)}
	}
	return err
}

func (w *watchdog) trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		ConnectStart:         func(string, string) { w.reset("connect") },
		GotConn:              func(httptrace.GotConnInfo) { w.reset("write") },
		WroteRequest:         func(httptrace.WroteRequestInfo) { w.reset("read") },
		GotFirstResponseByte: func() { w.reset("read") },
	})
}

// Streamed response whose body reads each restart the read timeout
type StreamResponse struct {
	Response
	Body io.ReadCloser
}

type watchedBody struct {
	rc     io.ReadCloser
	w      *watchdog
	cancel func()
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.w.reset("read")
	if err != nil && err != io.EOF {
		return n, b.w.wrap(err)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	b.w.stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

// Sends a request with httpx per phase timeouts and returns the open response stream
func Stream(client *http.Client, req *http.Request, timeout time.Duration) (*StreamResponse, error) {
	return streamWith(client, req, func(cancel func()) *watchdog { return newWatchdog(timeout, cancel) })
}

// Sends a request whose connect, write, read and pool phases each get their own httpx.Timeout limit
func StreamTimeout(client *http.Client, req *http.Request, t Timeout) (*StreamResponse, error) {
	return streamWith(client, req, func(cancel func()) *watchdog { return newPhaseWatchdog(t, cancel) })
}

func streamWith(client *http.Client, req *http.Request, watch func(cancel func()) *watchdog) (*StreamResponse, error) {
	ctx, cancel := context.WithCancel(req.Context())
	w := watch(cancel)
	resp, err := client.Do(req.WithContext(w.trace(ctx)))
	if err != nil {
		w.stop()
		cancel()
		if errors.Is(err, ErrTooManyRedirects) {
			return nil, ErrTooManyRedirects
		}
		return nil, w.wrap(err)
	}
	w.reset("read")
	return &StreamResponse{
		Response: Response{StatusCode: resp.StatusCode, Header: resp.Header, URL: resp.Request.URL.String()},
		Body:     &watchedBody{rc: resp.Body, w: w, cancel: cancel},
	}, nil
}

// Sends a request with httpx per phase timeouts and reads the whole body
func Do(client *http.Client, req *http.Request, timeout time.Duration) (*Response, error) {
	return readStream(Stream(client, req, timeout))
}

// Sends a request with distinct httpx.Timeout phase limits and reads the whole body
func DoTimeout(client *http.Client, req *http.Request, t Timeout) (*Response, error) {
	return readStream(StreamTimeout(client, req, t))
}

func readStream(s *StreamResponse, err error) (*Response, error) {
	if err != nil {
		return nil, err
	}
	defer s.Body.Close()
	body, err := io.ReadAll(s.Body)
	if err != nil {
		return nil, err
	}
	s.Content = body
	return &s.Response, nil
}

// Redirect policy following up to the httpx cap of twenty hops, or returning each redirect response
func RedirectPolicy(follow bool) func(*http.Request, []*http.Request) error {
	return func(_ *http.Request, via []*http.Request) error {
		if !follow {
			return http.ErrUseLastResponse
		}
		if len(via) > 20 {
			return ErrTooManyRedirects
		}
		return nil
	}
}

// Plain client like httpx.get, no proxy vetting and redirects followed only on request
func PlainClient(follow bool) *http.Client {
	return &http.Client{CheckRedirect: RedirectPolicy(follow)}
}
