package homeassistant

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// aiohttp ClientError of a request the transport failed, err the os error behind it
type clientError struct {
	kind string
	text string
	err  error
}

func (e *clientError) Error() string      { return e.text }
func (e *clientError) PyTypeName() string { return e.kind }
func (e *clientError) Unwrap() error      { return e.err }

// asyncio TimeoutError of a request that outlasted its total timeout, its str empty
type timeoutError struct{}

func (e *timeoutError) Error() string      { return "" }
func (e *timeoutError) PyTypeName() string { return "TimeoutError" }

// aiohttp ClientResponseError with its status, message and request URL, ContentTypeError for a non JSON body
type responseError struct {
	status      int
	message     string
	url         string
	contentType bool
}

func (e *responseError) Error() string {
	return fmt.Sprintf("%d, message=%s, url=%s", e.status, pyrepr.Repr(e.message), pyrepr.Repr(e.url))
}

func (e *responseError) PyTypeName() string {
	if e.contentType {
		return "ContentTypeError"
	}
	return "ClientResponseError"
}

// Content types the aiohttp json reader accepts for a JSON body
var jsonContentTypeRE = pyre.MustCompile(`^application/(?:[\w.+-]+?\+)?json`)

// One fully read HTTP response
type httpResponse struct {
	status int
	reason string
	header http.Header
	body   []byte
	url    string
}

// Lowercased raw Content-Type header, empty when absent, as aiohttp reads it for json and the encoding
func (r *httpResponse) rawContentType() string { return pystr.Lower(r.header.Get("Content-Type")) }

// Charset parameter parse_mimetype yields, the first one with spaces and double quotes stripped
func mimetypeCharset(ctype string) string {
	if ctype == "" {
		return ""
	}
	parts := strings.Split(ctype, ";")
	for _, item := range parts[1:] {
		if item == "" {
			continue
		}
		key, value, _ := strings.Cut(item, "=")
		if pystr.Strip(pystr.Lower(key)) == "charset" {
			return pystr.StripChars(value, ` "`)
		}
	}
	return ""
}

// Codec name get_encoding picks, the CodecInfo name codecs.lookup gives the declared charset, else utf-8
func (r *httpResponse) encoding() string {
	if charset := mimetypeCharset(r.rawContentType()); charset != "" {
		if name, ok := pycodecs.InfoName(charset); ok {
			return name
		}
	}
	return "utf-8"
}

// aiohttp text of the body, a strict decode with the get_encoding codec
func (r *httpResponse) text() (string, error) { return pycodecs.Decode(r.body, r.encoding()) }

// aiohttp json of the body, None for a blank body and the ContentTypeError of a non JSON content type
func (r *httpResponse) json() (any, error) {
	ctype := r.rawContentType()
	if ok, err := jsonContentTypeRE.MatchString(ctype); err != nil || !ok {
		return nil, &responseError{status: r.status, message: "Attempt to decode JSON with unexpected mimetype: " + ctype, url: r.url,
			contentType: true}
	}
	stripped := bytes.Trim(r.body, " \t\n\r\x0b\x0c")
	if len(stripped) == 0 {
		return nil, nil
	}
	text, err := pycodecs.Decode(stripped, r.encoding())
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// HTTP/1.1 client like an aiohttp session with only its total timeout, proxy env honoured only when trustEnv
func newSession(trustEnv bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 0
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if !trustEnv {
		transport.Proxy = nil
	}
	return &http.Client{Transport: transport}
}

// TLS handshake failure a traced connection attempt recorded
type tlsFailure struct {
	mu  sync.Mutex
	err error
}

// Context whose connection attempts record a failed TLS handshake into the returned holder
func withTLSTrace(ctx context.Context) (context.Context, *tlsFailure) {
	failure := &tlsFailure{}
	trace := &httptrace.ClientTrace{TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
		if err != nil {
			failure.mu.Lock()
			failure.err = err
			failure.mu.Unlock()
		}
	}}
	return httptrace.WithClientTrace(ctx, trace), failure
}

// Recorded handshake failure, nil when every handshake succeeded
func (f *tlsFailure) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// aiohttp connector error of a failed TLS handshake, the certificate form for a verify failure and the ssl form otherwise
func sslConnectError(rawURL string, err error) error {
	text := pyrequests.SSLErrorText(err)
	host, port := hostPort(rawURL)
	if strings.HasPrefix(text, "[SSL: CERTIFICATE_VERIFY_FAILED]") {
		return &clientError{kind: "ClientConnectorCertificateError",
			text: fmt.Sprintf("Cannot connect to host %s:%s ssl:True [SSLCertVerificationError: (1, %s)]", host, port, pyrepr.Repr(text)), err: err}
	}
	return &clientError{kind: "ClientConnectorSSLError", text: fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, text), err: err}
}

// Host and port of a URL with the scheme default port
func hostPort(rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" || u.Scheme == "wss" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

// aiohttp ClientError a failed request raises, the connector text for a DNS or dial failure
func transportError(rawURL string, err error) error {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		host, port := hostPort(rawURL)
		reason := "Name or service not known"
		if dns.IsTemporary || dns.IsTimeout {
			reason = "Temporary failure in name resolution"
		}
		return &clientError{kind: "ClientConnectorDNSError", text: fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, reason), err: err}
	case errors.As(err, &op) && op.Op == "dial":
		host, port := hostPort(rawURL)
		reason := pyhttpx.OSErrorText(err)
		if tcp, ok := op.Addr.(*net.TCPAddr); ok {
			reason = fmt.Sprintf("Connect call failed ('%s', %d)", tcp.IP.String(), tcp.Port)
		}
		return &clientError{kind: "ClientConnectorError", text: fmt.Sprintf("Cannot connect to host %s:%s ssl:default [%s]", host, port, reason), err: err}
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return &clientError{kind: "ServerDisconnectedError", text: "Server disconnected", err: err}
	}
	return &clientError{kind: "ClientOSError", text: pyhttpx.OSErrorText(err), err: err}
}

// Accept-Encoding an aiohttp session sends when zstd is importable and Brotli is not installed
const defaultAcceptEncoding = "gzip, deflate, zstd"

// Body decoded as the aiohttp payload parser decodes gzip, deflate and zstd, with its errors for br and corrupt data
func decodeContent(encoding string, raw []byte, rawURL string) ([]byte, error) {
	enc := strings.ToLower(encoding)
	if enc == "br" {
		return nil, &responseError{status: 400, message: "Can not decode content-encoding: brotli (br). Please install `Brotli`", url: rawURL}
	}
	if len(raw) == 0 || (enc != "gzip" && enc != "deflate" && enc != "zstd") {
		return raw, nil
	}
	var out []byte
	var err error
	if enc == "zstd" {
		out, err = unzstdFrames(raw)
	} else {
		out, err = inflateMembers(enc, raw)
	}
	switch {
	case err == nil:
		return out, nil
	case !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF):
		return nil, &clientError{kind: "ClientPayloadError", text: "400, message:\n  Can not decode content-encoding: " + enc, err: err}
	case enc == "deflate":
		return nil, &responseError{status: 400, message: "deflate", url: rawURL}
	}
	return out, nil
}

// Concatenated members aiohttp decodes from one feed before it refuses the stream
const maxDecompressMembers = 1024

var errTooManyMembers = errors.New("Compressed stream has more than 1024 members")

// True while the bytes present can still begin a gzip member zlib accepts
func gzipPrefixValid(b []byte) bool {
	if len(b) >= 2 && (b[0] != 0x1f || b[1] != 0x8b) {
		return false
	}
	return len(b) < 4 || (b[2] == 8 && b[3]&0xe0 == 0)
}

// Concatenated gzip, zlib or raw deflate members decoded in order, output so far kept beside a truncation error
func inflateMembers(enc string, raw []byte) ([]byte, error) {
	wrapped := raw[0]&0xF == 8
	var out []byte
	rest := raw
	for members := 1; len(rest) > 0; members++ {
		if members > maxDecompressMembers {
			return nil, errTooManyMembers
		}
		src := bytes.NewReader(rest)
		var reader io.Reader
		var err error
		switch {
		case enc == "gzip":
			if !gzipPrefixValid(rest) {
				return nil, gzip.ErrHeader
			}
			var zr *gzip.Reader
			if zr, err = gzip.NewReader(src); err == nil {
				zr.Multistream(false)
				reader = zr
			}
		case wrapped:
			reader, err = zlib.NewReader(src)
		default:
			reader = flate.NewReader(src)
		}
		if err != nil {
			return out, err
		}
		chunk, err := io.ReadAll(reader)
		out = append(out, chunk...)
		if err != nil {
			return out, err
		}
		rest = rest[len(rest)-src.Len():]
	}
	return out, nil
}

// Frames at the start of a zstd body counted by walking frame and block headers up to the first unreadable one
func zstdFrameCount(raw []byte) int {
	count := 0
	for len(raw) > 0 {
		var h zstd.Header
		if err := h.Decode(raw); err != nil || h.HeaderSize > len(raw) {
			return count
		}
		count++
		pos := h.HeaderSize
		if h.Skippable {
			pos += int(h.SkippableSize)
		} else {
			for last := false; !last; {
				if pos+3 > len(raw) {
					return count
				}
				bh := int(raw[pos]) | int(raw[pos+1])<<8 | int(raw[pos+2])<<16
				size := bh >> 3
				if kind := (bh >> 1) & 3; kind == 1 {
					size = 1
				} else if kind == 3 {
					return count
				}
				last = bh&1 == 1
				pos += 3 + size
			}
			if h.HasCheckSum {
				pos += 4
			}
		}
		if pos > len(raw) {
			return count
		}
		raw = raw[pos:]
	}
	return count
}

// Concatenated zstd frames decoded in order, output so far kept beside a truncation error
func unzstdFrames(raw []byte) ([]byte, error) {
	if zstdFrameCount(raw) > maxDecompressMembers {
		return nil, errTooManyMembers
	}
	dec, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return io.ReadAll(dec)
}

// One request reading the whole body within the total timeout, a timeout as the asyncio TimeoutError
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte,
	total time.Duration) (*httpResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	rctx, handshake := withTLSTrace(rctx)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, rawURL, reader)
	if err != nil {
		return nil, &clientError{kind: "InvalidUrlClientError", text: rawURL, err: err}
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", defaultAcceptEncoding)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		if raw, err = decodeContent(resp.Header.Get("Content-Encoding"), raw, resp.Request.URL.String()); err != nil {
			return nil, err
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if rctx.Err() != nil {
			return nil, &timeoutError{}
		}
		if tlsErr := handshake.get(); tlsErr != nil {
			return nil, sslConnectError(rawURL, tlsErr)
		}
		return nil, transportError(rawURL, err)
	}
	reason := strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
	return &httpResponse{status: resp.StatusCode, reason: reason, header: resp.Header, body: raw, url: resp.Request.URL.String()}, nil
}

// One JSON POST, the payload encoded as json.dumps does with the JSON content type
func postJSON(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, payload any,
	total time.Duration) (*httpResponse, error) {
	text, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		merged[k] = v
	}
	return doRequest(ctx, client, http.MethodPost, rawURL, merged, []byte(text), total)
}
